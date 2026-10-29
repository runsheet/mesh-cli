package nodev1

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/jobresult"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// settled is how a one-shot session's finish stands once the session has
// ended (projectmesh-io/api ADR 001 amendment, finish enforcement): the
// result to finish with, the learnings the finish carried, or why there
// is no finish — the lease then fails no_finish, never finishes with
// prose.
type settled struct {
	result    json.RawMessage
	learnings json.RawMessage
	noFinish  string
}

// settle reads the finish of b's ended session. The product's answer to
// the finish tool is the result, with the learnings the call carried.
// Without one, the session's last message is the result only when it is
// a JSON object that — its own `learnings` set aside — fits the kind's
// finish_schema. A kind that names neither a finish tool nor a
// finish_schema has nothing to hold the session to, and its last message
// is the result as text, as before.
func settle(b *meshapi.Brief, fin *finishCapture, last string, refused int) settled {
	if r := fin.result(); r != nil {
		return settled{result: r, learnings: fin.carried()}
	}
	if b.FinishTool == "" && len(bytes.TrimSpace(b.FinishSchema)) == 0 {
		raw, _ := json.Marshal(map[string]string{"text": clip(last, 16<<10)})
		return settled{result: raw}
	}
	owed := "the session ended without calling its finish tool " + cmpOr(b.FinishTool, "(none named)")
	if refused > 0 {
		owed = fmt.Sprintf("the session ended without calling its finish tool %s, after %d refused stops", b.FinishTool, refused)
	}
	obj, ok := lastObject(last)
	if !ok {
		return settled{noFinish: owed + ", and its last message is not a JSON object to take as the result"}
	}
	learnings := obj["learnings"]
	delete(obj, "learnings")
	if len(bytes.TrimSpace(b.FinishSchema)) == 0 {
		return settled{noFinish: owed + ", and the kind has no finish_schema its last message could fit"}
	}
	if err := fitsSchema(b.FinishSchema, obj); err != nil {
		return settled{noFinish: owed + ", and its last message does not fit the kind's finish_schema: " + clip(err.Error(), 500)}
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return settled{noFinish: owed + ": " + err.Error()}
	}
	return settled{result: raw, learnings: learnings}
}

func cmpOr(s, or string) string {
	if s == "" {
		return or
	}
	return s
}

// lastObject is the JSON object a last message is: the whole message,
// or else its last fenced block.
func lastObject(text string) (map[string]json.RawMessage, bool) {
	try := func(s string) (map[string]json.RawMessage, bool) {
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, "{") {
			return nil, false
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(s), &obj) != nil || obj == nil {
			return nil, false
		}
		return obj, true
	}
	if obj, ok := try(text); ok {
		return obj, true
	}
	parts := strings.Split(text, "```")
	for i := len(parts) - 2; i >= 1; i -= 2 {
		block := parts[i]
		if nl := strings.IndexByte(block, '\n'); nl >= 0 && !strings.HasPrefix(strings.TrimSpace(block), "{") {
			block = block[nl+1:] // the fence's language
		}
		if obj, ok := try(block); ok {
			return obj, true
		}
	}
	return nil, false
}

// fitsSchema validates obj against schema.
func fitsSchema(schema json.RawMessage, obj map[string]json.RawMessage) error {
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return jobresult.Fits(string(schema), raw)
}

// hasLearnings says whether learnings is a non-empty JSON array.
func hasLearnings(learnings json.RawMessage) bool {
	var list []json.RawMessage
	return json.Unmarshal(learnings, &list) == nil && len(list) > 0
}

// handInLearnings calls the kind's learnings tool with the learnings the
// finish carried, when the session did not call it itself: through the
// lease's sessionproxy on sock, as the session — the proxy adds the
// lease's session token — before the lease ends (ADR 001 amendment,
// learnings and transcripts). Mesh never holds them.
func (r *Runtime) handInLearnings(ctx context.Context, b *meshapi.Brief, sock string, fin *finishCapture, learnings json.RawMessage) {
	if b.LearningsTool == "" || sock == "" || fin.learnedAlready() || !hasLearnings(learnings) {
		return
	}
	server := productServer(b)
	if server == "" {
		r.logf("nodev1: lease %s: the brief names no product server for its learnings tool %s", b.LeaseID, b.LearningsTool)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	tool := b.LearningsTool
	if i := strings.LastIndex(tool, "__"); i >= 0 && strings.HasPrefix(tool, "mcp__") {
		tool = tool[i+2:]
	}
	args, _ := json.Marshal(map[string]json.RawMessage{"learnings": learnings})
	if err := callProductTool(ctx, sock, server, tool, args); err != nil {
		r.logf("nodev1: lease %s: handing the finish's learnings to %s: %v", b.LeaseID, b.LearningsTool, err)
		return
	}
	r.logf("nodev1: lease %s: handed the finish's learnings to %s for the session", b.LeaseID, b.LearningsTool)
}

// productServer is the brief's server named for its product, else its
// only server.
func productServer(b *meshapi.Brief) string {
	for _, s := range b.Servers {
		if s.Name == b.Product {
			return s.Name
		}
	}
	if len(b.Servers) == 1 {
		return b.Servers[0].Name
	}
	return ""
}

// callProductTool is one MCP tools/call on server through the sessionproxy on
// sock: initialize, initialized, then the call, over streamable HTTP. A
// call the server answers with isError is an error.
func callProductTool(ctx context.Context, sock, server, tool string, args json.RawMessage) error {
	c := &mcpClient{url: "http://relay" + sessionproxy.Prefix + server, http: &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}}
	if _, err := c.rpc(ctx, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "mesh-node", "version": "1"}}); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if _, err := c.rpc(ctx, 0, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("initialized: %w", err)
	}
	res, err := c.rpc(ctx, 2, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return err
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return err
	}
	if out.IsError {
		var text []string
		for _, c := range out.Content {
			text = append(text, c.Text)
		}
		return fmt.Errorf("%s refused: %s", tool, strings.Join(text, " "))
	}
	return nil
}

// mcpClient is the least of a streamable-HTTP MCP client: one request
// at a time, the server's session id kept.
type mcpClient struct {
	url     string
	http    *http.Client
	session string
}

// rpc sends one message; id 0 is a notification, which has no answer.
func (c *mcpClient) rpc(ctx context.Context, id int, method string, params any) (json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if id > 0 {
		msg["id"] = id
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s answered %d: %s", method, resp.StatusCode, clip(string(raw), 300))
	}
	if id == 0 {
		return nil, nil
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw = lastEventData(raw)
	}
	var ans struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if ans.Error != nil {
		return nil, fmt.Errorf("%s: %d %s", method, ans.Error.Code, ans.Error.Message)
	}
	if ans.Result == nil {
		return nil, errors.New(method + ": no result")
	}
	return ans.Result, nil
}

// lastEventData is the data of an SSE body's last event.
func lastEventData(body []byte) []byte {
	var last, cur []byte
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case bytes.HasPrefix(line, []byte("data:")):
			cur = append(cur, bytes.TrimSpace(line[5:])...)
		case len(bytes.TrimSpace(line)) == 0 && len(cur) > 0:
			last, cur = cur, nil
		}
	}
	if len(cur) > 0 {
		last = cur
	}
	return last
}
