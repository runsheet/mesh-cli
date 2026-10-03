package meshmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/version"
)

// The hosted path (ADR-023, Hosted path): when the platform's /mcp
// serves the work tools, mesh_join's claim and mesh_progress, mesh_ask,
// mesh_finish and mesh_release go to their hosted namesakes on the
// person's own bearer and the tool's text is relayed; when it does not,
// the GraphQL mutations stay and HostedFallbackLine is said once per
// run, as TokenFallback says the item token's fallback.

// HostedWorkTools are the hosted tools the work tools call: all five
// served is the hosted path, any missing is the mutations'.
var HostedWorkTools = []string{"mesh_claim", "mesh_progress", "mesh_ask", "mesh_finish", "mesh_release"}

// HostedFallbackLine is said once per run on stderr when the platform's
// /mcp serves no hosted work tools.
const HostedFallbackLine = "the platform serves no hosted work tools; using claimWorkItem"

// HostedProbeEvery is how long a tools/list answer is kept before the
// next work tool probes again.
const HostedProbeEvery = time.Hour

// Hosted is an HTTP MCP client to the gateway's /mcp (the URL
// node.MeshContextMCP builds), Bearer the person's token.
type Hosted struct {
	// BaseURL is the gateway's: /mcp is appended.
	BaseURL string
	Source  gateway.TokenSource
	HTTP    *http.Client
	// Stderr takes the fallback line; os.Stderr when nil.
	Stderr io.Writer
	// Now is time.Now when nil: the hourly re-probe's clock.
	Now func() time.Time

	mu       sync.Mutex
	probedAt time.Time
	served   bool
	session  string
	nextID   atomic.Int64
	once     sync.Once
}

// NewHosted is the hosted client over gw's base URL and token source.
func NewHosted(gw *gateway.Client) *Hosted {
	return &Hosted{BaseURL: gw.BaseURL, Source: gw.Source}
}

// HostedError is the hosted tool answering isError: its text is the
// refusal.
type HostedError struct {
	Tool, Text string
}

func (e *HostedError) Error() string { return e.Text }

func (h *Hosted) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Hosted) client() *http.Client {
	if h.HTTP != nil {
		return h.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// endpoint is the /mcp URL and its headers, as node.MeshContextMCP
// names them for a session.
func (h *Hosted) endpoint(ctx context.Context) (string, map[string]string, error) {
	tok, err := h.Source.Token(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("token: %w", err)
	}
	srv, _ := node.MeshContextMCP(h.BaseURL, tok)["mesh-context"].(map[string]any)
	if srv == nil {
		return "", nil, errors.New("no gateway URL or token")
	}
	url, _ := srv["url"].(string)
	headers, _ := srv["headers"].(map[string]string)
	return url, headers, nil
}

type hostedReply struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// rpc posts one JSON-RPC request (a notification when id is false) and
// reads its reply, a JSON body or an event stream's first message.
func (h *Hosted) rpc(ctx context.Context, method string, params any, notify bool) (json.RawMessage, error) {
	url, headers, err := h.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if !notify {
		msg["id"] = h.nextID.Add(1)
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if h.session != "" {
		req.Header.Set("Mcp-Session-Id", h.session)
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		h.session = s
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s: %s", method, resp.Status, strings.TrimSpace(string(b)))
	}
	if notify {
		return nil, nil
	}
	raw, err := readReply(resp)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	var r hostedReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
	}
	return r.Result, nil
}

// readReply is the body's JSON, or the first `data:` message of an
// event stream.
func readReply(resp *http.Response) ([]byte, error) {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if d, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(d))
			continue
		}
		if line == "" && len(data) > 0 {
			return []byte(strings.Join(data, "\n")), nil
		}
	}
	if len(data) > 0 {
		return []byte(strings.Join(data, "\n")), nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("empty event stream")
}

// probe initializes a session and lists the tools: whether all five
// work tools are served.
func (h *Hosted) probe(ctx context.Context) (bool, error) {
	h.session = ""
	if _, err := h.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mesh mcp", "version": version.Resolved()},
	}, false); err != nil {
		return false, err
	}
	if _, err := h.rpc(ctx, "notifications/initialized", nil, true); err != nil {
		return false, err
	}
	raw, err := h.rpc(ctx, "tools/list", map[string]any{}, false)
	if err != nil {
		return false, err
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return false, fmt.Errorf("tools/list: %w", err)
	}
	have := map[string]bool{}
	for _, t := range list.Tools {
		have[t.Name] = true
	}
	for _, n := range HostedWorkTools {
		if !have[n] {
			return false, nil
		}
	}
	return true, nil
}

// Serves reports whether the platform's /mcp serves the five work
// tools, probing at the first ask and again once the last answer is an
// hour old. A probe that fails is a no, asked again the same way.
func (h *Hosted) Serves(ctx context.Context) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.probedAt.IsZero() && h.now().Sub(h.probedAt) < HostedProbeEvery {
		return h.served
	}
	served, err := h.probe(ctx)
	if err != nil {
		served = false
	}
	h.served, h.probedAt = served, h.now()
	return served
}

// Use is Serves for a work tool about to call: a no says
// HostedFallbackLine, the first time only. A nil Hosted is the
// mutations with nothing said.
func (h *Hosted) Use(ctx context.Context) bool {
	if h == nil {
		return false
	}
	if h.Serves(ctx) {
		return true
	}
	h.once.Do(func() {
		w := h.Stderr
		if w == nil {
			w = os.Stderr
		}
		fmt.Fprintln(w, "mesh mcp: "+HostedFallbackLine)
	})
	return false
}

// Call calls the hosted tool with args and answers its text; an
// isError answer is a *HostedError carrying it.
func (h *Hosted) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, err := h.rpc(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, false)
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	var texts []string
	for _, c := range res.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if res.IsError {
		return "", &HostedError{Tool: tool, Text: text}
	}
	return text, nil
}

// The arguments each hosted work tool is sent
// (specs/contracts/hostedworktools.json).

// ClaimArgs is hosted mesh_claim's: the goal's slug, the item's seq,
// whether it is a pair's join, and the personal node claiming it.
func ClaimArgs(goal string, seq int, pair bool, nodeID string) map[string]any {
	return map[string]any{"goal": goal, "item": seq, "pair": pair, "node": nodeID}
}

// ProgressArgs is hosted mesh_progress's.
func ProgressArgs(itemID, note string) map[string]any {
	return map[string]any{"item": itemID, "note": note}
}

// AskArgs is hosted mesh_ask's.
func AskArgs(itemID, question string) map[string]any {
	return map[string]any{"item": itemID, "question": question}
}

// FinishArgs is hosted mesh_finish's for a built item.
func FinishArgs(itemID, branch, head string) map[string]any {
	return map[string]any{"item": itemID, "branch": branch, "headSha": head}
}

// FinishDoneArgs is hosted mesh_finish's for an already_done item.
func FinishDoneArgs(itemID, evidence string) map[string]any {
	return map[string]any{"item": itemID, "outcome": "already_done", "evidence": evidence}
}

// ReleaseArgs is hosted mesh_release's.
func ReleaseArgs(itemID, reason string) map[string]any {
	return map[string]any{"item": itemID, "reason": reason}
}
