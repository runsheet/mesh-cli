package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
)

// mesh lease-egress (ADR-051 decision 1) is a lease's egress as a
// process of its own, outside the node and its pod: its own CA, its
// own tunnel, serving the CONNECT route of both the lease's node
// socket and its lane socket. The node drives it over a control
// socket of the lease's own (decision 2), reached by the node's UID
// alone, shaped like relaysidecar's ReplaceCredentials: Start pushes
// the policy and rules once; Credentials replaces the session token
// and DPoP key whenever the node has new ones, reaching every rule
// with no rebuild, as ReplaceCredentials reaches a sidecar's upstreams.
// The control connection also carries the tunnel's audit the other
// way, streamed: a dropped connection leaves the tunnel running
// unchanged, its calls held in a bounded buffer and replayed
// oldest-dropped-first on reconnect, with the exact count dropped
// meanwhile (open question 2).

const (
	// LeaseEgressStartPath is served once, before any session reaches
	// the lease's sockets: the policy's hosts and its rules.
	LeaseEgressStartPath = "/lease-egress/start"
	// LeaseEgressCredentialsPath replaces the lease's session token and
	// DPoP key, as many times as the node has new ones.
	LeaseEgressCredentialsPath = "/lease-egress/credentials"
	// LeaseEgressAuditPath streams the tunnel's audit: a resync naming
	// what was dropped since this control connection last read it, the
	// buffer's backlog oldest first, then every call as it happens.
	LeaseEgressAuditPath = "/lease-egress/audit"

	// DefaultAuditBufferCap bounds how many audit events the process
	// holds while no control connection is reading them (ADR-051 open
	// question 2): a lease's session can drive many requests a minute,
	// so 256 is a few minutes of them — enough to survive a short node
	// restart's quiet window without an unread lease growing the buffer
	// without bound.
	DefaultAuditBufferCap = 256

	leaseEgressBodyMax = 1 << 20
)

// LeaseEgressStartRequest is the node's one-time ask that the process
// begin serving its tunnel: the hosts its policy allows (open allows
// any host, for a lease whose kind declares no egress) and the rules
// to terminate TLS for (egress.RuleSpec; a rule's hosts narrow the
// policy's CA, never widen it past Hosts).
type LeaseEgressStartRequest struct {
	Hosts []string          `json:"hosts"`
	Open  bool              `json:"open,omitempty"`
	Rules []egress.RuleSpec `json:"rules,omitempty"`
}

// LeaseEgressStartResult is the process's answer to Start.
type LeaseEgressStartResult struct {
	Started bool   `json:"started"`
	Refused string `json:"refused,omitempty"`
}

// LeaseEgressCredentialsRequest is the node's push of the lease's
// session token and DPoP key, replacing whatever the process already
// holds: every sign rule reads them of the moment (egress.SignRule),
// so the swap alone reaches it.
type LeaseEgressCredentialsRequest struct {
	Key     dpop.Handoff       `json:"key"`
	Session relaysidecar.Token `json:"session"`
}

// LeaseEgressCredentialsResult is the process's answer to Credentials.
type LeaseEgressCredentialsResult struct {
	Replaced bool   `json:"replaced"`
	Refused  string `json:"refused,omitempty"`
}

// AuditEvent is one line of the audit stream: a Call the tunnel heard
// (Audit) or finished (Done).
type AuditEvent struct {
	Seq  uint64       `json:"seq"`
	Kind string       `json:"kind"` // "call" or "done"
	Call *egress.Call `json:"call,omitempty"`
}

// AuditResync opens every audit stream connection: Dropped is how many
// records this control connection missed since it was last reading —
// everything dropped while it was disconnected, zero on a first
// connection or one that never fell behind.
type AuditResync struct {
	Kind    string `json:"kind"` // "resync"
	Dropped int64  `json:"dropped"`
	// Seq is the first seq this connection will see next, in the
	// backlog or live: a resync with no backlog and nothing dropped
	// names the next call's seq, so a reader can tell two resyncs with
	// Dropped 0 apart.
	Seq uint64 `json:"seq"`
}

// auditBuffer is the lease's audit, as mesh lease-egress keeps it while
// no control connection reads it: a ring bounded at cap, oldest
// dropped first, with the exact count dropped since the buffer was
// last read tracked separately from events still held.
type auditBuffer struct {
	mu      sync.Mutex
	cond    *sync.Cond
	cap     int
	events  []AuditEvent
	next    uint64
	dropped int64
	closed  bool
}

func newAuditBuffer(cap int) *auditBuffer {
	if cap <= 0 {
		cap = DefaultAuditBufferCap
	}
	b := &auditBuffer{cap: cap}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *auditBuffer) push(kind string, c egress.Call) {
	b.mu.Lock()
	ev := AuditEvent{Seq: b.next, Kind: kind, Call: &c}
	b.next++
	b.events = append(b.events, ev)
	if len(b.events) > b.cap {
		drop := len(b.events) - b.cap
		b.events = b.events[drop:]
		b.dropped += int64(drop)
	}
	b.cond.Broadcast()
	b.mu.Unlock()
}

// resync starts a fresh read of the buffer: the dropped count since
// the last read (reset to zero here, since this connection now owns
// knowing about any future drop), the backlog currently held, and the
// seq a live read should resume from.
func (b *auditBuffer) resync() (AuditResync, []AuditEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rs := AuditResync{Kind: "resync", Dropped: b.dropped}
	b.dropped = 0
	backlog := append([]AuditEvent(nil), b.events...)
	if len(backlog) > 0 {
		rs.Seq = backlog[0].Seq
	} else {
		rs.Seq = b.next
	}
	return rs, backlog
}

// since waits, if need be, for an event at or past from, and returns
// every event currently held at or past it and the seq to resume from
// next; ok is false once the buffer is closed and nothing more is
// coming.
func (b *auditBuffer) since(ctx context.Context, from uint64) (events []AuditEvent, next uint64, ok bool) {
	stop := context.AfterFunc(ctx, func() {
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
	})
	defer stop()

	b.mu.Lock()
	defer b.mu.Unlock()
	for b.next <= from && !b.closed && ctx.Err() == nil {
		b.cond.Wait()
	}
	if ctx.Err() != nil {
		return nil, from, false
	}
	for _, ev := range b.events {
		if ev.Seq >= from {
			events = append(events, ev)
		}
	}
	next = b.next
	return events, next, !b.closed
}

func (b *auditBuffer) Close() {
	b.mu.Lock()
	b.closed = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// signerFunc adapts a function to egress.Signer.
type signerFunc func(method, rawURL, token string) (string, error)

func (f signerFunc) Proof(method, rawURL, token string) (string, error) {
	return f(method, rawURL, token)
}

// LeaseEgressServer is the process ADR-051 decision 1 names, mesh
// lease-egress: a lease's own tunnel and CA, reached by the node alone
// over its control socket (Start, Credentials, the audit stream below)
// and by the lease's session and lanes over the node and lane sockets
// (ServeTunnelSocket, the tunnel's own ServeHTTP, unchanged). It holds
// the session token and DPoP key Credentials last pushed; Start's
// rules read them of the moment, so a replace reaches an already
// running sign rule with no rebuild.
type LeaseEgressServer struct {
	Tunnel *egress.Tunnel
	Logf   func(format string, args ...any)

	mu      sync.Mutex
	started bool
	key     *dpop.Key
	session relaysidecar.Token

	buf *auditBuffer
}

// NewLeaseEgressServer wires tunnel's Audit and Done into the server's
// bounded buffer, so every call the tunnel makes, served or not, is
// held for the control connection's audit stream.
func NewLeaseEgressServer(tunnel *egress.Tunnel, logf func(format string, args ...any)) *LeaseEgressServer {
	s := &LeaseEgressServer{Tunnel: tunnel, Logf: logf}
	s.buf = newAuditBuffer(0)
	tunnel.Audit = func(c egress.Call) { s.buf.push("call", c) }
	tunnel.Done = func(c egress.Call) { s.buf.push("done", c) }
	return s
}

func (s *LeaseEgressServer) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *LeaseEgressServer) token(ctx context.Context) (string, error) {
	s.mu.Lock()
	tok := s.session.Value
	s.mu.Unlock()
	if tok == "" {
		return "", errors.New("the lease's egress process holds no session token yet")
	}
	return tok, nil
}

func (s *LeaseEgressServer) proof(method, rawURL, token string) (string, error) {
	s.mu.Lock()
	key := s.key
	s.mu.Unlock()
	if key == nil {
		return "", errors.New("the lease's egress process holds no DPoP key yet")
	}
	return key.Proof(method, rawURL, token)
}

// Start installs the tunnel's policy and rules; called once, before
// any session reaches the node or lane socket. A rule's prefixes
// outside Hosts never widen the CA past what Hosts names.
func (s *LeaseEgressServer) Start(req LeaseEgressStartRequest) LeaseEgressStartResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return LeaseEgressStartResult{Refused: "already started"}
	}
	policy := egress.NewPolicy(req.Hosts, nil, nil)
	if req.Open {
		policy = policy.Open()
	}
	rules, err := egress.BuildRules(req.Rules, s.token, signerFunc(s.proof))
	if err != nil {
		return LeaseEgressStartResult{Refused: err.Error()}
	}
	var ca *egress.CA
	if len(rules) > 0 {
		seen := map[string]bool{}
		var hosts []string
		for _, r := range rules {
			hr, ok := r.(interface{ Hosts() []string })
			if !ok {
				continue
			}
			for _, h := range hr.Hosts() {
				if !seen[h] {
					seen[h] = true
					hosts = append(hosts, h)
				}
			}
		}
		if len(hosts) > 0 {
			ca, err = egress.NewCA("lease-egress", hosts, 0)
			if err != nil {
				return LeaseEgressStartResult{Refused: err.Error()}
			}
		}
	}
	s.Tunnel.Policy = policy
	s.Tunnel.Rules = rules
	s.Tunnel.CA = ca
	s.started = true
	s.logf("lease-egress: started (%d host(s), %d rule(s))", len(req.Hosts), len(rules))
	return LeaseEgressStartResult{Started: true}
}

// Credentials replaces the lease's session token and DPoP key, for
// every rule Start installed: each reads them of the moment, so the
// swap alone reaches them.
func (s *LeaseEgressServer) Credentials(req LeaseEgressCredentialsRequest) LeaseEgressCredentialsResult {
	key, err := dpop.KeyFromHandoff(req.Key)
	if err != nil {
		return LeaseEgressCredentialsResult{Refused: fmt.Sprintf("the new key: %v", err)}
	}
	if req.Session.Value == "" {
		return LeaseEgressCredentialsResult{Refused: "no session token"}
	}
	s.mu.Lock()
	s.key, s.session = key, req.Session
	s.mu.Unlock()
	s.logf("lease-egress: credentials replaced")
	return LeaseEgressCredentialsResult{Replaced: true}
}

func (s *LeaseEgressServer) serveStart(w http.ResponseWriter, r *http.Request) {
	var req LeaseEgressStartRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, leaseEgressBodyMax)).Decode(&req); err != nil {
		http.Error(w, "not a start request", http.StatusBadRequest)
		return
	}
	writeJSON(w, s.Start(req))
}

func (s *LeaseEgressServer) serveCredentials(w http.ResponseWriter, r *http.Request) {
	var req LeaseEgressCredentialsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, leaseEgressBodyMax)).Decode(&req); err != nil {
		http.Error(w, "not credentials", http.StatusBadRequest)
		return
	}
	writeJSON(w, s.Credentials(req))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// serveAudit streams the buffer's resync, its backlog, then every call
// as it happens, until the client disconnects: the buffer itself keeps
// running (push never blocks on a reader), so a disconnect here never
// touches the tunnel.
func (s *LeaseEgressServer) serveAudit(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)

	rs, backlog := s.buf.resync()
	if err := enc.Encode(rs); err != nil {
		return
	}
	for _, ev := range backlog {
		if err := enc.Encode(ev); err != nil {
			return
		}
	}
	flusher.Flush()

	ctx := r.Context()
	from := rs.Seq + uint64(len(backlog))
	for {
		events, next, ok := s.buf.since(ctx, from)
		if !ok && len(events) == 0 {
			return
		}
		for _, ev := range events {
			if err := enc.Encode(ev); err != nil {
				return
			}
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		from = next
		if ctx.Err() != nil {
			return
		}
	}
}

// ServeControl serves the control socket (Start, Credentials, the
// audit stream) until ctx is done, then closes it and returns ctx's
// error.
func (s *LeaseEgressServer) ServeControl(ctx context.Context, path string) error {
	ln, err := listenUnix(path)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+LeaseEgressStartPath, s.serveStart)
	mux.HandleFunc("POST "+LeaseEgressCredentialsPath, s.serveCredentials)
	mux.HandleFunc("GET "+LeaseEgressAuditPath, s.serveAudit)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		s.buf.Close()
		_ = srv.Close()
		_ = os.Remove(path)
		<-errCh
		return ctx.Err()
	case err := <-errCh:
		_ = os.Remove(path)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// listenUnix removes a stale socket at path, listens there and sets it
// 0600: the node's own UID reaches a lease's control socket, never the
// session (ADR-051 decision 2).
func listenUnix(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// ServeTunnelSocket serves tunnel's CONNECT route alone on a unix
// socket at path, 0600, until the returned closer is closed. The
// lease-egress process's node socket and lane socket are both this:
// neither the sidecar nor a lane ever sees an address change
// (ADR-051 decision 1), and egress.Tunnel.Listen tracks one socket at
// a time, for a lease that opens no lane.
func ServeTunnelSocket(tunnel *egress.Tunnel, path string) (io.Closer, error) {
	ln, err := listenUnix(path)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: tunnel, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return closerFunc(func() error {
		err := srv.Close()
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
			err = rerr
		}
		return err
	}), nil
}

// LeaseEgressClient is the node's way to a lease's egress process, on
// its control socket (ADR-051 decision 2).
type LeaseEgressClient struct {
	// Socket is the process's control socket.
	Socket string
}

func (c LeaseEgressClient) httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", c.Socket)
	}}}
}

func (c LeaseEgressClient) post(ctx context.Context, path string, req, out any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://lease-egress"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(hr)
	if err != nil {
		return fmt.Errorf("the lease's egress process: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the lease's egress process answered %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, leaseEgressBodyMax)).Decode(out)
}

// Start asks the process to install its policy and rules.
func (c LeaseEgressClient) Start(ctx context.Context, req LeaseEgressStartRequest) (LeaseEgressStartResult, error) {
	var out LeaseEgressStartResult
	err := c.post(ctx, LeaseEgressStartPath, req, &out)
	return out, err
}

// Credentials asks the process to take req as the lease's session
// token and DPoP key from now on.
func (c LeaseEgressClient) Credentials(ctx context.Context, req LeaseEgressCredentialsRequest) (LeaseEgressCredentialsResult, error) {
	var out LeaseEgressCredentialsResult
	err := c.post(ctx, LeaseEgressCredentialsPath, req, &out)
	return out, err
}

// Audit opens the control connection's audit stream: onResync once,
// with what was dropped since this client last read it, then onEvent
// for every call, oldest first, until ctx is done or the connection
// drops (returning nil, since a dropped audit stream is ordinary, not
// an error the node need act on).
func (c LeaseEgressClient) Audit(ctx context.Context, onResync func(AuditResync), onEvent func(AuditEvent)) error {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://lease-egress"+LeaseEgressAuditPath, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(hr)
	if err != nil {
		return fmt.Errorf("the lease's egress process: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the lease's egress process answered %d", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	first := true
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if first {
			first = false
			var rs AuditResync
			if err := json.Unmarshal(raw, &rs); err == nil && rs.Kind == "resync" {
				if onResync != nil {
					onResync(rs)
				}
				continue
			}
		}
		var ev AuditEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return err
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
}
