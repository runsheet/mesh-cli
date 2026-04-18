package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	meshv1 "projectmesh.io/mesh-cli/proto/meshv1"
)

// Per spec A.3 / A.4: fixed 120×40 PTY, 4 MB per-session scrollback ring,
// drop-oldest semantics with a dropped_bytes counter surfaced on the next
// attach snapshot.
const (
	ptyCols          uint16 = 120
	ptyRows          uint16 = 40
	sessionRingCap          = 4 * 1024 * 1024
	attachFrameCap          = 4 * 1024
	attachQueueDepth        = 64
)

// ringBuffer is a fixed-capacity byte buffer with drop-oldest semantics. It
// tracks the number of bytes that have been evicted since the last successful
// Snapshot so a freshly-attached viewer can be warned that output was missed.
type ringBuffer struct {
	mu      sync.Mutex
	cap     int
	buf     []byte
	dropped uint64
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{cap: capacity, buf: make([]byte, 0, capacity)}
}

func (r *ringBuffer) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// If the incoming slice alone is larger than cap, retain only its tail.
	if len(p) >= r.cap {
		r.dropped += uint64(len(r.buf)) + uint64(len(p)-r.cap)
		r.buf = append(r.buf[:0], p[len(p)-r.cap:]...)
		return
	}

	overflow := len(r.buf) + len(p) - r.cap
	if overflow > 0 {
		r.dropped += uint64(overflow)
		r.buf = append(r.buf[:0], r.buf[overflow:]...)
	}
	r.buf = append(r.buf, p...)
}

// Snapshot returns a copy of the current buffer plus the accumulated
// dropped-byte count, and resets the counter.
func (r *ringBuffer) Snapshot() ([]byte, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	dropped := r.dropped
	r.dropped = 0
	return out, dropped
}

type attacher struct {
	id string
	ch chan []byte
}

// PtySession is a live Claude Code PTY owned by the daemon on behalf of an agent.
type PtySession struct {
	AgentID         string
	SessionID       string // mesh-internal id
	ClaudeSessionID string // Claude Code --resume handle once known
	PTY             *os.File
	Cmd             *exec.Cmd
	StartedAt       time.Time
	LastActivity    time.Time

	buffer *ringBuffer

	mu        sync.Mutex
	attachers map[string]*attacher
	exited    bool
	exitErr   error
}

func (s *PtySession) addAttacher(a *attacher) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return false
	}
	s.attachers[a.id] = a
	return true
}

func (s *PtySession) removeAttacher(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.attachers[id]; ok {
		delete(s.attachers, id)
		close(a.ch)
	}
}

func (s *PtySession) broadcast(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.attachers {
		// Non-blocking send: a slow viewer drops the frame. The ring buffer
		// keeps the canonical replay copy.
		select {
		case a.ch <- chunk:
		default:
		}
	}
}

func (s *PtySession) markExited(err error) {
	s.mu.Lock()
	s.exited = true
	s.exitErr = err
	attachers := s.attachers
	s.attachers = map[string]*attacher{}
	s.mu.Unlock()

	for _, a := range attachers {
		close(a.ch)
	}
}

// PtySessionSummary mirrors the proto SessionSummary but is tied to our
// in-process state so we can compute it without importing protoc types into
// every caller.
type PtySessionSummary struct {
	AgentID         string
	SessionID       string
	ClaudeSessionID string
	PID             int
	State           meshv1.SessionState
	StartedAt       time.Time
	LastActivity    time.Time
	Cols            uint32
	Rows            uint32
}

func (s *PtySessionSummary) Proto() *meshv1.SessionSummary {
	return &meshv1.SessionSummary{
		AgentId:         s.AgentID,
		ClaudeSessionId: s.ClaudeSessionID,
		Pid:             int32(s.PID),
		State:           s.State,
		StartedAt:       timestamppb.New(s.StartedAt),
		LastActivityAt:  timestamppb.New(s.LastActivity),
		Cols:            s.Cols,
		Rows:            s.Rows,
	}
}

// SessionRegistry owns the set of live PTY sessions keyed by agent id.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*PtySession

	onExit func(agentID string, err error)
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{sessions: map[string]*PtySession{}}
}

// SetExitHook wires an optional callback invoked when a PTY process exits. The
// daemon uses this to push a SessionEvent over the gRPC stream.
func (r *SessionRegistry) SetExitHook(fn func(agentID string, err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onExit = fn
}

// Start spawns a new Claude Code PTY for agentID. If resumeID is non-empty it
// becomes `claude --resume <id>`; otherwise a fresh `claude` session. Returns
// the existing session unchanged if one is already live for the agent.
func (r *SessionRegistry) Start(agentID, model, resumeID string) (*PtySession, bool, error) {
	return r.StartWithOptions(StartOptions{AgentID: agentID, Model: model, ResumeID: resumeID})
}

// StartOptions feeds StartWithOptions. Env is overlaid on top of the daemon's
// environment when provided; WorkingDir, if set, becomes cmd.Dir so the
// Claude Code session starts in the template's declared workdir.
type StartOptions struct {
	AgentID    string
	Model      string
	ResumeID   string
	Env        map[string]string
	WorkingDir string
}

// StartWithOptions is Start but threads env + workdir through to the child.
func (r *SessionRegistry) StartWithOptions(opts StartOptions) (*PtySession, bool, error) {
	agentID := opts.AgentID
	if agentID == "" {
		return nil, false, errors.New("agentID required")
	}

	r.mu.Lock()
	if s, ok := r.sessions[agentID]; ok {
		r.mu.Unlock()
		return s, false, nil
	}
	r.mu.Unlock()

	args := []string{}
	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	cmd := exec.Command("claude", args...)
	if opts.WorkingDir != "" {
		cmd.Dir = opts.WorkingDir
	}
	if len(opts.Env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), opts.Env)
	}

	ptyFile, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: ptyCols, Rows: ptyRows})
	if err != nil {
		return nil, false, fmt.Errorf("spawn claude pty for agent %s: %w", agentID, err)
	}

	now := time.Now()
	s := &PtySession{
		AgentID:         agentID,
		SessionID:       uuid.NewString(),
		ClaudeSessionID: opts.ResumeID,
		PTY:             ptyFile,
		Cmd:             cmd,
		StartedAt:       now,
		LastActivity:    now,
		buffer:          newRingBuffer(sessionRingCap),
		attachers:       map[string]*attacher{},
	}

	r.mu.Lock()
	// Lost-race protection: another caller may have started a session while we
	// were spawning. If so, reap ours.
	if existing, ok := r.sessions[agentID]; ok {
		r.mu.Unlock()
		_ = ptyFile.Close()
		_ = cmd.Process.Kill()
		return existing, false, nil
	}
	r.sessions[agentID] = s
	r.mu.Unlock()

	go r.readLoop(s)
	go r.waitLoop(s)

	return s, true, nil
}

func (r *SessionRegistry) readLoop(s *PtySession) {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.PTY.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.buffer.Write(chunk)
			s.mu.Lock()
			s.LastActivity = time.Now()
			s.mu.Unlock()
			s.broadcast(chunk)
		}
		if err != nil {
			return
		}
	}
}

func (r *SessionRegistry) waitLoop(s *PtySession) {
	err := s.Cmd.Wait()
	_ = s.PTY.Close()
	s.markExited(err)

	r.mu.Lock()
	delete(r.sessions, s.AgentID)
	hook := r.onExit
	r.mu.Unlock()

	if hook != nil {
		hook(s.AgentID, err)
	}
}

// Stop terminates the session for agentID. force=true sends SIGKILL; otherwise
// SIGTERM. Returns (stopped=false, nil) when no session exists — callers treat
// stop-of-unknown as a no-op (see spec note on idempotency).
func (r *SessionRegistry) Stop(agentID string, force bool) (bool, error) {
	r.mu.RLock()
	s, ok := r.sessions[agentID]
	r.mu.RUnlock()
	if !ok {
		return false, nil
	}
	if s.Cmd == nil || s.Cmd.Process == nil {
		return false, errors.New("session has no process handle")
	}

	var signalErr error
	if force {
		signalErr = s.Cmd.Process.Kill()
	} else {
		signalErr = s.Cmd.Process.Signal(os.Interrupt)
	}
	if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
		return false, fmt.Errorf("signal claude process: %w", signalErr)
	}
	return true, nil
}

// NewUp kills the existing session (if any) without --resume, then starts a
// fresh one so Claude Code loses its prior conversation state. Intended for
// prompt changes where replay would be nonsensical.
func (r *SessionRegistry) NewUp(agentID, model string) (*PtySession, error) {
	return r.NewUpWithOptions(StartOptions{AgentID: agentID, Model: model})
}

// NewUpWithOptions is NewUp but preserves env + workdir on the new session.
func (r *SessionRegistry) NewUpWithOptions(opts StartOptions) (*PtySession, error) {
	_, _ = r.Stop(opts.AgentID, true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.RLock()
		_, ok := r.sessions[opts.AgentID]
		r.mu.RUnlock()
		if !ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	s, _, err := r.StartWithOptions(opts)
	return s, err
}

// mergeEnv overlays overrides onto base (VAR=VALUE slice form). Later wins.
func mergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	idx := map[string]int{}
	for i, kv := range base {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			idx[kv[:eq]] = i
		}
	}
	out := append([]string{}, base...)
	for k, v := range overrides {
		entry := k + "=" + v
		if i, ok := idx[k]; ok {
			out[i] = entry
		} else {
			out = append(out, entry)
		}
	}
	return out
}

// List returns a stable-ordered snapshot of all live sessions.
func (r *SessionRegistry) List() []PtySessionSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PtySessionSummary, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, r.summaryLocked(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// Get returns a summary for a single agent's session, or ok=false when absent.
func (r *SessionRegistry) Get(agentID string) (PtySessionSummary, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[agentID]
	if !ok {
		return PtySessionSummary{}, false
	}
	return r.summaryLocked(s), true
}

func (r *SessionRegistry) summaryLocked(s *PtySession) PtySessionSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	pid := 0
	state := meshv1.SessionState_SESSION_STATE_RUNNING
	if s.Cmd != nil && s.Cmd.Process != nil {
		pid = s.Cmd.Process.Pid
	}
	if s.exited {
		if s.exitErr != nil {
			state = meshv1.SessionState_SESSION_STATE_CRASHED
		} else {
			state = meshv1.SessionState_SESSION_STATE_EXITED
		}
	}
	return PtySessionSummary{
		AgentID:         s.AgentID,
		SessionID:       s.SessionID,
		ClaudeSessionID: s.ClaudeSessionID,
		PID:             pid,
		State:           state,
		StartedAt:       s.StartedAt,
		LastActivity:    s.LastActivity,
		Cols:            uint32(ptyCols),
		Rows:            uint32(ptyRows),
	}
}

// Attach registers a new viewer for agentID. The returned channel delivers the
// initial buffer snapshot as the first chunk, followed by live output. cleanup
// must be called when the caller detaches. The returned dropped_bytes is the
// count that applies to the *first* frame the caller sends downstream.
func (r *SessionRegistry) Attach(agentID string) (string, <-chan []byte, uint64, func(), error) {
	return r.AttachWithID(agentID, uuid.NewString())
}

// AttachWithID is Attach but lets the caller supply the attach_id (mesh-api is
// authoritative per the proto). Empty attachID falls back to a fresh uuid.
func (r *SessionRegistry) AttachWithID(agentID, attachID string) (string, <-chan []byte, uint64, func(), error) {
	r.mu.RLock()
	s, ok := r.sessions[agentID]
	r.mu.RUnlock()
	if !ok {
		return "", nil, 0, nil, fmt.Errorf("no session for agent %s", agentID)
	}

	if attachID == "" {
		attachID = uuid.NewString()
	}
	a := &attacher{id: attachID, ch: make(chan []byte, attachQueueDepth)}

	// Prime the channel with the replay snapshot BEFORE registering, so we
	// don't drop live bytes that arrive in the gap between snapshot and
	// registration. We drain the snapshot under the session lock via addAttacher
	// by taking the snapshot first (which also zeros the drop counter), then
	// registering — bytes written after the snapshot read will be appended to
	// the ring and delivered via broadcast.
	replay, dropped := s.buffer.Snapshot()
	if !s.addAttacher(a) {
		return "", nil, 0, nil, errors.New("session exited before attach")
	}

	if len(replay) > 0 {
		// Split replay into chunks that fit attachFrameCap so the first real
		// frame carrying dropped_bytes is at a known boundary.
		go func() {
			for i := 0; i < len(replay); i += attachFrameCap {
				end := i + attachFrameCap
				if end > len(replay) {
					end = len(replay)
				}
				select {
				case a.ch <- replay[i:end]:
				default:
					// Viewer is already behind; bail — subsequent broadcasts
					// will keep landing in ch as space frees up.
					return
				}
			}
		}()
	}

	cleanup := func() { s.removeAttacher(attachID) }
	return attachID, a.ch, dropped, cleanup, nil
}

// WriteInput forwards bytes from a viewer into the PTY. read_only viewers
// should not call this. Returns the number of bytes written.
func (r *SessionRegistry) WriteInput(agentID string, data []byte) (int, error) {
	r.mu.RLock()
	s, ok := r.sessions[agentID]
	r.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("no session for agent %s", agentID)
	}
	return s.PTY.Write(data)
}

// DetachAttacher tears down a single viewer by attach id without killing the
// session itself. Other attachers stay subscribed. No-op when the session or
// attach is already gone.
func (r *SessionRegistry) DetachAttacher(agentID, attachID string) {
	r.mu.RLock()
	s, ok := r.sessions[agentID]
	r.mu.RUnlock()
	if !ok {
		return
	}
	s.removeAttacher(attachID)
}
