// Package mcprelay is a stdio MCP server that relays every message to
// an HTTP MCP endpoint (the gateway's /mcp) on a bearer it reads from a
// file on every request. A session's MCP client never re-reads its
// config, so a bearer written into it dies with the token; a relay whose
// bearer is a file the node keeps fresh outlives any one token (goal
// a-sessions-mcp-token-outlives-the-session).
package mcprelay

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
	"os"
	"strings"
	"sync"
)

// MaxMessage is the longest line the relay reads from stdin and the
// longest event-stream line it reads from the server.
const MaxMessage = 16 << 20

// SocketPrefix is the path every server is served under on a
// sessionproxy socket, mirroring internal/sessionproxy's Prefix: a
// relay in Socket mode names its server at SocketPrefix + Server.
// mcprelay does not import sessionproxy (which imports this package)
// to keep the two from cycling.
const SocketPrefix = "/mcp/"

// Relay relays newline-delimited JSON-RPC from a stdio MCP client to an
// HTTP MCP server — the gateway's /mcp on a bearer (URL, TokenFile), or
// one upstream of a sessionproxy on a unix socket (Socket, Server),
// never both.
type Relay struct {
	// URL is the server's MCP endpoint, e.g. https://api.runsheet.dev/mcp.
	URL string
	// TokenFile holds the bearer, read and trimmed on every request. Not
	// read, and no Authorization header sent, when empty — Socket mode's
	// case: the proxy adds whatever credential each upstream needs, and
	// the session reaching it never does.
	TokenFile string
	// Socket is a sessionproxy's unix socket; Server is the upstream's
	// name there, reached at SocketPrefix + Server. Set together,
	// instead of URL and TokenFile.
	Socket string
	Server string
	// HTTP is the client; a plain one with no timeout when nil, since a
	// tool call may stream for as long as it runs. On Socket, its
	// Transport dials the socket regardless of what HTTP names.
	HTTP *http.Client
	// Stderr takes what the relay cannot answer on stdout; os.Stderr
	// when nil.
	Stderr io.Writer

	mu      sync.Mutex
	session string
	// initialize is the client's own initialize request, re-sent when
	// the server forgets the session.
	initialize []byte

	outMu sync.Mutex
	out   io.Writer
}

// message is what the relay reads of a client's message.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func (m message) isRequest() bool {
	return len(m.ID) > 0 && string(m.ID) != "null" && m.Method != ""
}

// Run relays every line of in to the server and every message the server
// answers to out, until in ends or ctx is done. Requests run
// concurrently once the session is initialized; Run returns once every
// one has been answered.
func (r *Relay) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	r.out = out
	var wg sync.WaitGroup
	defer wg.Wait()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), MaxMessage)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		body := append([]byte(nil), line...)
		var m message
		_ = json.Unmarshal(body, &m) // a batch or garbage is relayed as is
		// The handshake and notifications go in order; requests after it
		// run side by side, as an MCP client expects of a server.
		if !m.isRequest() || m.Method == "initialize" {
			r.handle(ctx, body, m)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.handle(ctx, body, m)
		}()
	}
	return sc.Err()
}

// handle relays one message and writes the server's answer, or a
// JSON-RPC error when there is none to write and the message wants one.
func (r *Relay) handle(ctx context.Context, body []byte, m message) {
	if m.Method == "initialize" {
		r.mu.Lock()
		r.initialize = body
		r.session = ""
		r.mu.Unlock()
	}
	err := r.send(ctx, body, true)
	if err == nil {
		return
	}
	if m.isRequest() {
		r.write(errorReply(m.ID, err))
		return
	}
	r.logf("mesh mcp-relay: %s: %v", orNotification(m.Method), err)
}

func orNotification(method string) string {
	if method == "" {
		return "message"
	}
	return method
}

// statusError is the server answering a status other than 2xx.
type statusError struct {
	Code int
	Body string
}

func (e *statusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%d %s", e.Code, http.StatusText(e.Code))
	}
	return fmt.Sprintf("%d %s: %s", e.Code, http.StatusText(e.Code), e.Body)
}

// sessionGone is the server saying it no longer knows the session the
// relay named: a 404, or a 400 whose body names the session.
func sessionGone(err error, hadSession bool) bool {
	var se *statusError
	if !errors.As(err, &se) || !hadSession {
		return false
	}
	switch se.Code {
	case http.StatusNotFound:
		return true
	case http.StatusBadRequest:
		return strings.Contains(strings.ToLower(se.Body), "session")
	}
	return false
}

// send posts body, relaying the answer to out. A 401 is tried once more
// on the file read again; a session the server forgot is initialized
// again with the client's own initialize and tried once more.
func (r *Relay) send(ctx context.Context, body []byte, relay bool) error {
	session := r.currentSession()
	err := r.post(ctx, body, session, relay)
	var se *statusError
	if errors.As(err, &se) && se.Code == http.StatusUnauthorized {
		err = r.post(ctx, body, session, relay)
	}
	if sessionGone(err, session != "") {
		if rerr := r.reinitialize(ctx, session); rerr != nil {
			return fmt.Errorf("%w; re-initializing: %v", err, rerr)
		}
		err = r.post(ctx, body, r.currentSession(), relay)
	}
	return err
}

func (r *Relay) currentSession() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session
}

// reinitialize re-sends the client's saved initialize and its
// notifications/initialized, unless another request already replaced
// the stale session. The initialize's answer is the relay's own, never
// the client's.
func (r *Relay) reinitialize(ctx context.Context, stale string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session != stale {
		return nil
	}
	if r.initialize == nil {
		return errors.New("no initialize to re-send")
	}
	r.session = ""
	if err := r.postLocked(ctx, r.initialize, "", false); err != nil {
		return err
	}
	note := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return r.postLocked(ctx, note, r.session, false)
}

// post sends one message under the session named, keeping the session
// the server returns.
func (r *Relay) post(ctx context.Context, body []byte, session string, relay bool) error {
	resp, err := r.do(ctx, body, session)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		r.mu.Lock()
		r.session = s
		r.mu.Unlock()
	}
	return r.reply(resp, relay)
}

// postLocked is post with r.mu already held.
func (r *Relay) postLocked(ctx context.Context, body []byte, session string, relay bool) error {
	resp, err := r.do(ctx, body, session)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		r.session = s
	}
	return r.reply(resp, relay)
}

func (r *Relay) do(ctx context.Context, body []byte, session string) (*http.Response, error) {
	var bearer string
	if r.TokenFile != "" {
		tok, err := r.token()
		if err != nil {
			return nil, err
		}
		bearer = tok
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	return r.client().Do(req)
}

// endpoint is the URL a request goes to: r.URL, or, on a socket, a
// fixed host over it — the socket alone decides where the request
// lands, so the host in the URL is never read.
func (r *Relay) endpoint() string {
	if r.Socket != "" {
		return "http://unix" + SocketPrefix + r.Server
	}
	return r.URL
}

// token is the file's contents, trimmed: a missing or empty file is an
// error naming it.
func (r *Relay) token() (string, error) {
	b, err := os.ReadFile(r.TokenFile)
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", r.TokenFile, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", r.TokenFile)
	}
	return tok, nil
}

// reply reads the server's answer: a status error, nothing (202), a
// JSON body, or an event stream whose every message is relayed.
func (r *Relay) reply(resp *http.Response, relay bool) error {
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &statusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, err := io.ReadAll(io.LimitReader(resp.Body, MaxMessage))
		if err != nil {
			return err
		}
		if relay && len(bytes.TrimSpace(b)) > 0 {
			r.write(b)
		}
		return nil
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), MaxMessage)
	var data []string
	flush := func() {
		if len(data) > 0 && relay {
			r.write([]byte(strings.Join(data, "\n")))
		}
		data = nil
	}
	for sc.Scan() {
		line := sc.Text()
		if d, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(d, " "))
			continue
		}
		if line == "" {
			flush()
		}
	}
	flush()
	return sc.Err()
}

// write puts one message on its own line of stdout.
func (r *Relay) write(msg []byte) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, msg); err != nil {
		r.logf("mesh mcp-relay: the server answered something not JSON: %.200s", msg)
		return
	}
	buf.WriteByte('\n')
	r.outMu.Lock()
	defer r.outMu.Unlock()
	_, _ = r.out.Write(buf.Bytes())
}

// errorReply is the JSON-RPC error a request gets when the relay has no
// answer of the server's to give.
func errorReply(id json.RawMessage, err error) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32603, "message": "mesh mcp-relay: " + err.Error()},
	})
	return b
}

func (r *Relay) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	if r.Socket != "" {
		return &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", r.Socket)
			},
		}}
	}
	return &http.Client{}
}

func (r *Relay) logf(format string, args ...any) {
	w := r.Stderr
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format+"\n", args...)
}
