// Package session is a streaming Claude Code session (Mesh v1 spec,
// "Streaming sessions everywhere"; PR 2.10): `claude -p --input-format
// stream-json --output-format stream-json`, its input held open. The
// first message goes in when the session starts; more turns and control
// (set_model, interrupt) go in at any time after, and every line of the
// output reaches the caller as it comes, for liveness, usage and
// evidence. Closing the input lets the session finish what it was given
// and exit.
//
// The protocol, as the spike against claude 2.1.312 found it:
//
//   - a user turn is {"type":"user","message":{"role":"user","content":…}};
//   - every turn ends in its own result event: its usage and num_turns
//     are that turn's, its total_cost_usd and modelUsage the session's so
//     far;
//   - a control request is {"type":"control_request","request_id":…,
//     "request":{"subtype":"set_model","model":…}} (or "interrupt"), and
//     the CLI answers {"type":"control_response","response":{"subtype":
//     "success"|"error","request_id":…,"response":{…},"error":…}};
//   - set_model takes effect from the next turn, which opens with a
//     system/init event naming the new model, in the same session;
//   - an interrupt ends the turn in flight with a result of subtype
//     error_during_execution, and the session carries on;
//   - closing the input lets a turn already sent run to its result, then
//     the CLI exits.
//
// Codex's `codex app-server` (JSON-RPC over stdio, experimental in
// codex-cli 0.157.1) has the same live input in its protocol schema —
// thread/start, then turn/start per turn (whose model overrides that
// turn and the ones after), turn/steer to add input to the turn in
// flight, turn/interrupt — but it is not built here: Claude Code is the
// streaming harness until it is.
package session

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

	"projectmesh.io/mesh-cli/internal/harness"
)

// ErrClosed is a message sent after the input closed or the session
// ended.
var ErrClosed = errors.New("session: the input is closed")

// Options start one session.
type Options struct {
	// Binary is the claude executable; "claude" on PATH when empty.
	Binary string
	// Args follow -p and the stream flags: the model, settings, MCP
	// config and the like.
	Args []string
	// Dir is the session's working directory.
	Dir string
	// Env is the session's whole environment.
	Env []string
	// Stderr hears the process's stderr; discarded when nil.
	Stderr io.Writer
	// OnEvent hears every event of the output, in order, on the
	// session's reading goroutine: it must not block.
	OnEvent func(harness.Event)
	// OnLine hears every line of the output the stream parser reads
	// (control responses aside), raw, before OnEvent hears its event, on
	// the same goroutine: it must not block, and must copy what it keeps.
	// The node's transcript upload reads the stream here.
	OnLine func(line []byte)
	// Spawn, when set, runs the session's process in place of exec on
	// the host — a v1 node's lease pod (mesh-cli ADR-044): it runs argv
	// (Binary, the stream flags and Args) to its end on the streams it is
	// given, closing nothing of them, and answers how it ended. Env and
	// Dir are Spawn's to apply; ctx's end must stop it.
	Spawn func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// Session is one running streaming session.
type Session struct {
	// waitProc reaps the process: exec's Wait, or Spawn's end.
	waitProc func() error
	start    time.Time

	wmu    sync.Mutex
	in     io.WriteCloser
	closed bool

	mu      sync.Mutex
	nextReq int
	waiting map[string]chan controlAnswer
	turns   int
	results int
	models  map[string]harness.ModelSpend
	// modelTurns are the model turns by the model that ran them
	modelTurns map[string]int
	ended      bool

	parsed chan harness.Result
	done   chan struct{}
	err    error
	res    Result
}

// Result is how a session ended.
type Result struct {
	// SessionID is the CLI's session, for a later --resume.
	SessionID string
	// Text is the last turn's result text, or the last assistant text.
	Text string
	// Success is the last turn's: it ended in a success result.
	Success bool
	// CostUSD is the session's whole spend.
	CostUSD float64
	// Turns are the model turns of every user turn, summed.
	Turns int
	// Results counts the turns that ended in a result event.
	Results int
	// Models is the spend by model, as the CLI names each, from the last
	// result event's modelUsage.
	Models map[string]harness.ModelSpend
	// ModelTurns are Turns by the model that ran them, as the CLI names
	// it: each user turn's model turns go to the model whose spend grew
	// most in it (a turn escalated mid-way is credited to one model).
	ModelTurns map[string]int
	// Stream is everything else the stream parser kept.
	Stream   harness.Result
	Duration time.Duration
}

// controlAnswer is a control_response's response.
type controlAnswer struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response"`
	Error     string          `json:"error"`
}

// Start runs the session and sends first as its first turn. ctx bounds
// the whole session: its end kills the process.
func Start(ctx context.Context, opts Options, first string) (*Session, error) {
	bin := opts.Binary
	if bin == "" {
		bin = "claude"
	}
	args := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}, opts.Args...)
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
	s := &Session{waitProc: waitProc, start: time.Now(), in: in, waiting: map[string]chan controlAnswer{},
		parsed: make(chan harness.Result, 1), done: make(chan struct{})}
	pr, pw := io.Pipe()
	go func() { s.parsed <- harness.ParseStream(pr, s.observe(opts.OnEvent)) }()
	go func() {
		s.read(out, pw, opts.OnLine)
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
	if err := s.Send(first); err != nil {
		s.CloseInput()
		return s, err
	}
	return s, nil
}

// observe counts the turns and keeps the spend by model, then hands the
// event on.
func (s *Session) observe(next func(harness.Event)) func(harness.Event) {
	return func(ev harness.Event) {
		if ev.Type == "result" {
			s.mu.Lock()
			s.turns += ev.NumTurns
			s.results++
			if ev.ModelUsage != nil {
				if m := grewMost(s.models, ev.ModelUsage); m != "" && ev.NumTurns > 0 {
					if s.modelTurns == nil {
						s.modelTurns = map[string]int{}
					}
					s.modelTurns[m] += ev.NumTurns
				}
				s.models = ev.ModelUsage
			}
			s.mu.Unlock()
		}
		if next != nil {
			next(ev)
		}
	}
}

// grewMost is the model whose spend grew most from before to now — a
// result's modelUsage is the session's whole spend so far — by tokens,
// then cost; "" when none grew.
func grewMost(before, now map[string]harness.ModelSpend) string {
	best, bestTokens, bestCost := "", int64(0), 0.0
	for name, m := range now {
		b := before[name]
		tokens := (m.InputTokens + m.OutputTokens + m.CacheReadInputTokens + m.CacheCreationInputTokens) -
			(b.InputTokens + b.OutputTokens + b.CacheReadInputTokens + b.CacheCreationInputTokens)
		cost := m.CostUSD - b.CostUSD
		if tokens <= 0 && cost <= 0 {
			continue
		}
		better := tokens > bestTokens ||
			tokens == bestTokens && cost > bestCost ||
			tokens == bestTokens && cost == bestCost && (best == "" || name < best)
		if better {
			best, bestTokens, bestCost = name, tokens, cost
		}
	}
	return best
}

// read splits the output: control responses go to their waiters, every
// line to the stream parser.
func (s *Session) read(out io.Reader, pw *io.PipeWriter, onLine func([]byte)) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.Contains(line, []byte(`"control_response"`)) {
			var msg struct {
				Type     string        `json:"type"`
				Response controlAnswer `json:"response"`
			}
			if json.Unmarshal(line, &msg) == nil && msg.Type == "control_response" {
				s.answer(msg.Response)
				continue
			}
		}
		if onLine != nil {
			onLine(line)
		}
		if _, err := pw.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			break
		}
	}
	pw.Close()
	// the output is over: nobody will answer a control request now
	s.mu.Lock()
	s.ended = true
	for id, ch := range s.waiting {
		close(ch)
		delete(s.waiting, id)
	}
	s.mu.Unlock()
}

func (s *Session) answer(a controlAnswer) {
	s.mu.Lock()
	ch := s.waiting[a.RequestID]
	delete(s.waiting, a.RequestID)
	s.mu.Unlock()
	if ch != nil {
		ch <- a
	}
}

// spawn runs argv through opts.Spawn on pipes of its own: the input's
// reader closes once the process has ended, so a turn sent after that
// fails rather than blocking, and the output's writer closes then too,
// which ends the session's reading.
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

// wait reaps the process once its output is read to the end (exec: Wait
// closes the output pipe).
func (s *Session) wait() {
	err := s.waitProc()
	parsed := <-s.parsed
	s.mu.Lock()
	s.res = Result{
		SessionID: parsed.SessionID, Text: parsed.Text, Success: parsed.Success, CostUSD: parsed.CostUSD,
		Turns: s.turns, Results: s.results, Models: s.models, ModelTurns: s.modelTurns, Stream: parsed, Duration: time.Since(s.start),
	}
	s.err = err
	s.mu.Unlock()
	s.wmu.Lock()
	s.closed = true
	s.wmu.Unlock()
	close(s.done)
}

// write sends one line on the input.
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
		return fmt.Errorf("session: writing the input: %w", err)
	}
	return nil
}

// Send sends a user turn.
func (s *Session) Send(text string) error {
	return s.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
}

// control sends a control request and waits for its answer.
func (s *Session) control(ctx context.Context, request map[string]any) (controlAnswer, error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return controlAnswer{}, ErrClosed
	}
	s.nextReq++
	id := fmt.Sprintf("mesh-%d", s.nextReq)
	ch := make(chan controlAnswer, 1)
	s.waiting[id] = ch
	s.mu.Unlock()
	if err := s.write(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		s.mu.Lock()
		delete(s.waiting, id)
		s.mu.Unlock()
		return controlAnswer{}, err
	}
	select {
	case a, ok := <-ch:
		if !ok {
			return controlAnswer{}, ErrClosed
		}
		if a.Subtype != "success" {
			why := a.Error
			if why == "" {
				why = a.Subtype
			}
			return a, fmt.Errorf("session: %s refused: %s", request["subtype"], why)
		}
		return a, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.waiting, id)
		s.mu.Unlock()
		return controlAnswer{}, ctx.Err()
	}
}

// SetModel moves the session to model from its next turn, answering the
// model the CLI says it now runs (its full name; model may be an alias).
func (s *Session) SetModel(ctx context.Context, model string) (string, error) {
	a, err := s.control(ctx, map[string]any{"subtype": "set_model", "model": model})
	if err != nil {
		return "", err
	}
	var r struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(a.Response, &r)
	if r.Model == "" {
		r.Model = model
	}
	return r.Model, nil
}

// Interrupt stops the turn in flight, which ends in an error result; the
// session takes more turns after.
func (s *Session) Interrupt(ctx context.Context) error {
	_, err := s.control(ctx, map[string]any{"subtype": "interrupt"})
	return err
}

// CloseInput closes the input: the session finishes the turns it was
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
