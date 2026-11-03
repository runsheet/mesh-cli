// Package codexsession is a streaming Codex session over `codex
// app-server`'s transport (JSON-RPC 2.0, newline-delimited, over the
// process's stdio; experimental in codex-cli 0.157.1 —
// internal/harness/session's doc comment names the shape): the input
// stays open for the session's life, as internal/harness/session's
// does for Claude Code.
//
// thread/start opens the thread once, at Start, before the first
// turn; every Send after that opens a new turn with turn/start when
// none is in flight, or adds to the one in flight with turn/steer —
// "in flight" until a turn.completed or turn.failed notification
// ends it; Interrupt sends turn/interrupt for the turn in flight and
// waits for it to be acknowledged. Every notification the server
// sends — item.started, item.completed, agentMessage, turn.completed,
// turn.failed, and whatever else it adds — reaches the caller through
// OnEvent, in order, each line through OnLine first: the same two
// hooks internal/harness/session.Options carries for Claude Code, so
// a node's transcript upload and liveness reporting read either
// harness the same way. This package reads the transport alone, not
// the shape of a notification's params: that is a later item's.
package codexsession

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// ErrClosed is a request or a turn sent after the input closed or the
// session ended.
var ErrClosed = errors.New("codexsession: the input is closed")

// Notification is one line the app-server sent with a method and no
// id: item.started, agentMessage, turn.completed and the rest. Params
// is kept raw; a later item reads the shapes out of it.
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Options start one session.
type Options struct {
	// Binary is the codex executable; "codex" on PATH when empty.
	Binary string
	// Args follow "app-server": accounts, models and the like.
	Args []string
	// Dir is the session's working directory.
	Dir string
	// Env is the session's whole environment.
	Env []string
	// Stderr hears the process's stderr; discarded when nil.
	Stderr io.Writer
	// OnEvent hears every notification of the output, in order, on
	// the session's reading goroutine: it must not block.
	OnEvent func(Notification)
	// OnLine hears every line of the output that is a notification,
	// raw, before OnEvent hears it, on the same goroutine: it must not
	// block, and must copy what it keeps. A response to one of the
	// session's own requests is not a line OnLine hears.
	OnLine func(line []byte)
	// Spawn, when set, runs the session's process in place of exec on
	// the host — a v1 node's lease pod (mesh-cli ADR-044): it runs
	// argv (Binary, "app-server" and Args) to its end on the streams
	// it is given, closing nothing of them, and answers how it ended.
	// Env and Dir are Spawn's to apply; ctx's end must stop it.
	Spawn func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// Session is one running app-server session.
type Session struct {
	// waitProc reaps the process: exec's Wait, or Spawn's end.
	waitProc func() error
	start    time.Time

	wmu    sync.Mutex
	in     io.WriteCloser
	closed bool

	mu           sync.Mutex
	nextID       int
	waiting      map[int]chan rpcResponse
	threadID     string
	turnInFlight bool
	ended        bool

	done chan struct{}
	err  error
	res  Result
}

// Result is how a session ended.
type Result struct {
	// ThreadID is the app-server's thread id, for a later resume.
	ThreadID string
	Duration time.Duration
}

// rpcLine is one line read from the app-server: a response to one of
// the session's own requests (Method empty) or a notification
// (Method set, ID empty).
type rpcLine struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage
	Error  *rpcError
}

// Start runs the session, opens its thread with thread/start and
// sends first as its first turn. ctx bounds the whole session: its
// end kills the process.
func Start(ctx context.Context, opts Options, first string) (*Session, error) {
	bin := opts.Binary
	if bin == "" {
		bin = "codex"
	}
	args := append([]string{"app-server"}, opts.Args...)
	var (
		in       io.WriteCloser
		out      io.ReadCloser
		waitProc func() error
	)
	if opts.Spawn != nil {
		in, out, waitProc = spawn(ctx, opts, append([]string{bin}, args...))
	} else {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = opts.Dir
		cmd.Env = opts.Env
		cmd.Stderr = opts.Stderr
		cmd.WaitDelay = 5 * time.Second
		var err error
		if in, err = cmd.StdinPipe(); err != nil {
			return nil, err
		}
		if out, err = cmd.StdoutPipe(); err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		waitProc = cmd.Wait
	}
	s := &Session{waitProc: waitProc, start: time.Now(), in: in, waiting: map[int]chan rpcResponse{}, done: make(chan struct{})}
	go func() {
		s.read(out, opts.OnLine, opts.OnEvent)
		s.wait()
	}()
	go func() {
		// a child that kept the output open would hold the reader past
		// the session's end: close it once ctx is over and a grace has
		// passed
		select {
		case <-s.done:
		case <-ctx.Done():
			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
				out.Close()
			}
		}
	}()
	tres, err := s.call(ctx, "thread/start", map[string]any{})
	if err != nil {
		s.CloseInput()
		return s, err
	}
	var tr struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(tres, &tr)
	s.mu.Lock()
	s.threadID = tr.ThreadID
	s.mu.Unlock()
	if err := s.Send(first); err != nil {
		s.CloseInput()
		return s, err
	}
	return s, nil
}

// read splits the output: a response to one of the session's own
// requests goes to its waiter, a notification to onLine then onEvent.
func (s *Session) read(out io.Reader, onLine func([]byte), onEvent func(Notification)) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m rpcLine
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m.Method == "" {
			var id int
			_ = json.Unmarshal(m.ID, &id)
			s.answer(id, rpcResponse{Result: m.Result, Error: m.Error})
			continue
		}
		if onLine != nil {
			onLine(append([]byte(nil), line...))
		}
		s.observe(m.Method)
		if onEvent != nil {
			onEvent(Notification{Method: m.Method, Params: m.Params})
		}
	}
	// the output is over: nobody will answer a request now
	s.mu.Lock()
	s.ended = true
	for id, ch := range s.waiting {
		close(ch)
		delete(s.waiting, id)
	}
	s.mu.Unlock()
}

// observe clears the turn-in-flight state once the turn it was for
// ends.
func (s *Session) observe(method string) {
	switch method {
	case "turn.completed", "turn.failed":
		s.mu.Lock()
		s.turnInFlight = false
		s.mu.Unlock()
	}
}

func (s *Session) answer(id int, r rpcResponse) {
	s.mu.Lock()
	ch := s.waiting[id]
	delete(s.waiting, id)
	s.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

// spawn runs argv through opts.Spawn on pipes of its own: the input's
// reader closes once the process has ended, so a turn sent after that
// fails rather than blocking, and the output's writer closes then
// too, which ends the session's reading.
func spawn(ctx context.Context, opts Options, argv []string) (io.WriteCloser, io.ReadCloser, func() error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ended := make(chan error, 1)
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	go func() {
		err := opts.Spawn(ctx, argv, inR, outW, stderr)
		inR.CloseWithError(ErrClosed)
		outW.Close()
		ended <- err
	}()
	return inW, outR, func() error { return <-ended }
}

// wait reaps the process once its output is read to the end (exec:
// Wait closes the output pipe).
func (s *Session) wait() {
	err := s.waitProc()
	s.mu.Lock()
	s.res = Result{ThreadID: s.threadID, Duration: time.Since(s.start)}
	s.err = err
	s.mu.Unlock()
	s.wmu.Lock()
	s.closed = true
	s.wmu.Unlock()
	close(s.done)
}

// write sends one JSON-RPC line on the input.
func (s *Session) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("codexsession: writing the input: %w", err)
	}
	return nil
}

// request writes a JSON-RPC request with a fresh id and does not
// wait for its response: turn/start and turn/steer, as Send sends
// them — the server's answer, if any, is dropped on the floor since
// nobody is waiting for it.
func (s *Session) request(method string, params any) error {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	return s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// call writes a JSON-RPC request and waits for its response:
// thread/start, to learn the thread id, and turn/interrupt, to know
// the interrupt took.
func (s *Session) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.nextID++
	id := s.nextID
	ch := make(chan rpcResponse, 1)
	s.waiting[id] = ch
	s.mu.Unlock()
	if err := s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		s.mu.Lock()
		delete(s.waiting, id)
		s.mu.Unlock()
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, ErrClosed
		}
		if r.Error != nil {
			return nil, fmt.Errorf("codexsession: %s refused: %s", method, r.Error.Message)
		}
		return r.Result, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.waiting, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Send opens a new turn with turn/start when none is in flight, else
// adds text to the turn in flight with turn/steer.
func (s *Session) Send(text string) error {
	s.mu.Lock()
	tid := s.threadID
	inFlight := s.turnInFlight
	s.mu.Unlock()
	params := map[string]any{"threadId": tid, "input": []map[string]any{{"type": "text", "text": text}}}
	if inFlight {
		return s.request("turn/steer", params)
	}
	s.mu.Lock()
	s.turnInFlight = true
	s.mu.Unlock()
	return s.request("turn/start", params)
}

// Interrupt stops the turn in flight with turn/interrupt and waits
// for it to be acknowledged; the session takes more turns after, once
// the turn.completed or turn.failed it ends in reaches OnEvent.
func (s *Session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	tid := s.threadID
	s.mu.Unlock()
	_, err := s.call(ctx, "turn/interrupt", map[string]any{"threadId": tid})
	return err
}

// CloseInput closes the input: the session finishes the turn it was
// given and exits. Closing twice is nothing.
func (s *Session) CloseInput() {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.in.Close()
}

// Done closes when the session's process has exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// Wait waits for the session to exit and answers how it ended and the
// process's own error.
func (s *Session) Wait() (Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res, s.err
}
