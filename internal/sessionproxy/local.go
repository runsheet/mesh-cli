package sessionproxy

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
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/mcprelay"
)

// Local is a stdio MCP server the proxy runs itself, on the host and
// outside the agent's container: Command and Args as internal/tools
// renders a local tool (npx, uvx, docker), Env and Secrets added to the
// proxy's own environment for that child alone. A Secrets value is
// masked in what the child answers and in its stderr; an Env value is
// not, since a plain setting such as `1` would mask every digit.
type Local struct {
	Command string
	Args    []string
	Env     map[string]string
	Secrets map[string]string
}

func (l Local) endpoint() string { return "stdio:" + l.Command }

func (Local) retries401() bool { return false }

func (Local) authorize(context.Context, http.Header, bool) ([]string, error) {
	return nil, nil
}

// LocalStarts is how many times a child is started: once, then once
// more after it exits.
const LocalStarts = 2

// LocalReplayTimeout bounds the initialize a restarted child is sent
// again before the caller's message.
var LocalReplayTimeout = 30 * time.Second

// localKillWait is how long Close waits for a killed child to be reaped.
const localKillWait = 5 * time.Second

// errChildExited answers a request whose child exited before replying.
var errChildExited = errors.New("the server exited")

// child is one Local's process for one proxy and server name: started
// lazily, restarted once, killed on Close. Requests are written in the
// order they arrive, each with an id of the proxy's own so two callers'
// ids never meet, and the answer goes back under the caller's id.
type child struct {
	p    *Proxy
	name string
	spec Local

	start   sync.Mutex // serialises a start and its initialize replay
	writeMu sync.Mutex // one message at a time on stdin

	mu          sync.Mutex
	stdin       io.WriteCloser
	exited      chan struct{} // closed when the running process is reaped
	pid         int
	running     bool
	starts      int
	closed      bool
	closing     chan struct{}
	lastExit    error
	nextID      int64
	pending     map[string]*waiter
	subs        map[chan []byte]struct{}
	initMsg     map[string]json.RawMessage
	initialized bool
}

type waiter struct {
	orig json.RawMessage
	ch   chan reply
}

type reply struct {
	msg []byte
	err error
}

func newChild(p *Proxy, name string, spec Local) *child {
	return &child{
		p: p, name: name, spec: spec,
		closing: make(chan struct{}),
		pending: map[string]*waiter{},
		subs:    map[chan []byte]struct{}{},
	}
}

// localChild is name's child, made on first use.
func (p *Proxy) localChild(name string, spec Local) *child {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.children == nil {
		p.children = map[string]*child{}
	}
	c, ok := p.children[name]
	if !ok {
		c = newChild(p, name, spec)
		p.children[name] = c
	}
	return c
}

// closeChildren kills every child the proxy started and waits for each.
func (p *Proxy) closeChildren() {
	p.mu.Lock()
	cs := p.children
	p.children = nil
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func(c *child) {
			defer wg.Done()
			c.close()
		}(c)
	}
	wg.Wait()
}

// ensure starts the child when none runs, replaying the initialize the
// client sent the one before when this is the restart.
func (c *child) ensure() error {
	c.start.Lock()
	defer c.start.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("the proxy is closed")
	}
	if c.running {
		c.mu.Unlock()
		return nil
	}
	if c.starts >= LocalStarts {
		err := c.lastExit
		c.mu.Unlock()
		return fmt.Errorf("exited and was restarted once already (%v)", err)
	}
	restart := c.starts > 0
	lastExit := c.lastExit
	initMsg, initialized := c.initMsg, c.initialized
	if err := c.spawnLocked(); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	if !restart {
		return nil
	}
	c.p.logf("sessionproxy: %s: restarting after exit (%v)", c.name, lastExit)
	if initMsg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), LocalReplayTimeout)
	defer cancel()
	msg := make(map[string]json.RawMessage, len(initMsg))
	for k, v := range initMsg {
		msg[k] = v
	}
	w, err := c.request(msg)
	if err == nil {
		_, err = c.await(ctx, w, msg["id"])
	}
	if err != nil {
		return fmt.Errorf("initialize after restart: %w", err)
	}
	if initialized {
		if err := c.write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
			return fmt.Errorf("initialized after restart: %w", err)
		}
	}
	return nil
}

// spawnLocked starts the process; c.mu is held.
func (c *child) spawnLocked() error {
	cmd := exec.Command(c.spec.Command, c.spec.Args...)
	cmd.Env = append(append(os.Environ(), envList(c.spec.Env)...), envList(c.spec.Secrets)...)
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", c.spec.Command, err)
	}
	c.starts++
	c.running = true
	c.stdin = stdin
	c.pid = cmd.Process.Pid
	exited := make(chan struct{})
	c.exited = exited
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); c.readStdout(stdout) }()
	go func() { defer readers.Done(); c.readStderr(stderr) }()
	go func() {
		readers.Wait()
		err := cmd.Wait()
		c.mu.Lock()
		c.running = false
		c.stdin = nil
		if err == nil {
			err = errors.New("exit 0")
		}
		c.lastExit = err
		pending := c.pending
		c.pending = map[string]*waiter{}
		closed := c.closed
		c.mu.Unlock()
		for _, w := range pending {
			w.ch <- reply{err: errChildExited}
		}
		if !closed {
			c.p.logf("sessionproxy: %s: exited: %v", c.name, err)
		}
		close(exited)
	}()
	return nil
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// close kills the child's process group and waits for it to be reaped.
func (c *child) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.closing)
	running, pid, exited := c.running, c.pid, c.exited
	c.mu.Unlock()
	if !running {
		return
	}
	killGroup(pid)
	select {
	case <-exited:
	case <-time.After(localKillWait):
		c.p.logf("sessionproxy: %s: pid %d not reaped %s after kill", c.name, pid, localKillWait)
	}
}

func (c *child) readStdout(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), mcprelay.MaxMessage)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		c.received(append([]byte(nil), line...))
	}
	if err := sc.Err(); err != nil {
		c.p.logf("sessionproxy: %s: stdout: %v", c.name, err)
	}
	// Whatever is left unread is drained so the process can exit.
	_, _ = io.Copy(io.Discard, r)
}

func (c *child) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), mcprelay.MaxMessage)
	for sc.Scan() {
		c.p.logf("sessionproxy: %s: stderr: %s", c.name, sc.Text())
	}
	_, _ = io.Copy(io.Discard, r)
}

// received routes one line from the child: a response to the caller
// waiting on its id, anything else to the GET streams.
func (c *child) received(line []byte) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		c.p.logf("sessionproxy: %s: stdout: not JSON-RPC: %.200s", c.name, line)
		return
	}
	_, hasMethod := msg["method"]
	id, hasID := msg["id"]
	if hasID && !hasMethod {
		key := string(bytes.TrimSpace(id))
		c.mu.Lock()
		w, ok := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if !ok {
			c.p.logf("sessionproxy: %s: a response to no waiting request (id %s)", c.name, key)
			return
		}
		msg["id"] = w.orig
		out, err := json.Marshal(msg)
		w.ch <- reply{msg: out, err: err}
		return
	}
	c.broadcast(line)
}

func (c *child) broadcast(line []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.subs) == 0 {
		c.p.logf("sessionproxy: %s: a server message with no stream open; dropped", c.name)
		return
	}
	for ch := range c.subs {
		select {
		case ch <- line:
		default:
			c.p.logf("sessionproxy: %s: a stream is behind; message dropped", c.name)
		}
	}
}

func (c *child) write(line []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errChildExited
	}
	if _, err := stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// request gives msg an id of the proxy's own, registers its waiter
// under it and writes it; msg["id"] is the caller's on return.
func (c *child) request(msg map[string]json.RawMessage) (*waiter, error) {
	orig := msg["id"]
	c.mu.Lock()
	c.nextID++
	inner := strconv.FormatInt(c.nextID, 10)
	w := &waiter{orig: orig, ch: make(chan reply, 1)}
	c.pending[inner] = w
	if method(msg) == "initialize" {
		saved := make(map[string]json.RawMessage, len(msg))
		for k, v := range msg {
			saved[k] = v
		}
		c.initMsg, c.initialized = saved, false
	}
	c.mu.Unlock()
	msg["id"] = json.RawMessage(inner)
	line, err := json.Marshal(msg)
	msg["id"] = orig
	if err == nil {
		err = c.write(line)
	}
	if err != nil {
		c.forget(inner)
		return nil, err
	}
	return w, nil
}

func (c *child) forget(inner string) {
	c.mu.Lock()
	delete(c.pending, inner)
	c.mu.Unlock()
}

// await is w's answer, or ctx's end — then the child is told the
// request is cancelled.
func (c *child) await(ctx context.Context, w *waiter, orig json.RawMessage) ([]byte, error) {
	select {
	case r := <-w.ch:
		return r.msg, r.err
	case <-ctx.Done():
		c.mu.Lock()
		var inner string
		for k, v := range c.pending {
			if v == w {
				inner = k
				delete(c.pending, k)
			}
		}
		c.mu.Unlock()
		if inner != "" {
			_ = c.write([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + inner + `}}`))
		}
		return nil, ctx.Err()
	}
}

func method(msg map[string]json.RawMessage) string {
	var m string
	_ = json.Unmarshal(msg["method"], &m)
	return m
}

// isRequest is a message with a method and a non-null id.
func isRequest(msg map[string]json.RawMessage) bool {
	_, hasMethod := msg["method"]
	id, hasID := msg["id"]
	return hasMethod && hasID && string(bytes.TrimSpace(id)) != "null"
}

// serveLocal bridges streamable HTTP to the child's stdio: a POST's
// messages written in order, its requests answered as JSON once the
// child answers each; a GET an event stream of what the child says
// unasked.
func (p *Proxy) serveLocal(w http.ResponseWriter, req *http.Request, name string, spec Local, body []byte) {
	c := p.localChild(name, spec)
	switch req.Method {
	case http.MethodPost:
		c.post(w, req, body)
	case http.MethodGet:
		c.stream(w, req)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, -32600, req.Method+" is not served")
	}
}

func (c *child) fail(w http.ResponseWriter, err error) {
	msg := c.p.mask(err.Error())
	c.p.logf("sessionproxy: %s: %s", c.name, msg)
	c.p.noteError(c.name, fmt.Sprintf("sessionproxy: %s: %s", c.name, msg))
	writeError(w, http.StatusBadGateway, -32603, fmt.Sprintf("sessionproxy: %s: %s", c.name, msg))
}

func (c *child) post(w http.ResponseWriter, req *http.Request, body []byte) {
	trimmed := bytes.TrimSpace(body)
	batch := len(trimmed) > 0 && trimmed[0] == '['
	var raws []json.RawMessage
	if batch {
		if err := json.Unmarshal(trimmed, &raws); err != nil {
			writeError(w, http.StatusBadRequest, -32700, "parse error: "+err.Error())
			return
		}
	} else {
		raws = []json.RawMessage{trimmed}
	}
	msgs := make([]map[string]json.RawMessage, len(raws))
	for i, raw := range raws {
		if err := json.Unmarshal(raw, &msgs[i]); err != nil || msgs[i] == nil {
			writeError(w, http.StatusBadRequest, -32700, "parse error: not a JSON-RPC message")
			return
		}
	}
	if err := c.ensure(); err != nil {
		c.fail(w, err)
		return
	}
	type sent struct {
		w    *waiter
		orig json.RawMessage
	}
	var waits []sent
	for _, msg := range msgs {
		if isRequest(msg) {
			wt, err := c.request(msg)
			if err != nil {
				c.fail(w, err)
				return
			}
			waits = append(waits, sent{wt, msg["id"]})
			continue
		}
		if method(msg) == "notifications/initialized" {
			c.mu.Lock()
			c.initialized = true
			c.mu.Unlock()
		}
		line, err := json.Marshal(msg)
		if err == nil {
			err = c.write(line)
		}
		if err != nil {
			c.fail(w, err)
			return
		}
	}
	if len(waits) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	answers := make([]json.RawMessage, 0, len(waits))
	for _, s := range waits {
		out, err := c.await(req.Context(), s.w, s.orig)
		if errors.Is(err, errChildExited) {
			out, err = errorReply(s.orig, -32603, "sessionproxy: "+c.name+": "+errChildExited.Error()), nil
		}
		if err != nil {
			if req.Context().Err() != nil {
				return
			}
			c.fail(w, err)
			return
		}
		answers = append(answers, out)
	}
	var out []byte
	if batch {
		out, _ = json.Marshal(answers)
	} else {
		out = answers[0]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, c.p.mask(string(out)))
}

func errorReply(id json.RawMessage, code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
	return b
}

// stream is a GET's event stream: every message the child sends that
// answers no request, until the client goes or the proxy closes.
func (c *child) stream(w http.ResponseWriter, req *http.Request) {
	if !strings.Contains(req.Header.Get("Accept"), "text/event-stream") {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, -32600, "GET takes Accept: text/event-stream")
		return
	}
	if err := c.ensure(); err != nil {
		c.fail(w, err)
		return
	}
	ch := make(chan []byte, 64)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	closing := c.closing
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.subs, ch)
		c.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case line := <-ch:
			if _, err := io.WriteString(w, "event: message\ndata: "+c.p.mask(string(line))+"\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-req.Context().Done():
			return
		case <-closing:
			return
		}
	}
}
