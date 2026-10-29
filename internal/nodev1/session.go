package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/harness/session"
)

// SessionSpec is one harnessed session of a lease.
type SessionSpec struct {
	// Label names the session in logs: the lease.
	Label string
	// Workspace is the directory the session runs in.
	Workspace string
	// Prompt is the session's first message.
	Prompt string
	// Model is the kind's default model; empty leaves the CLI's own.
	Model string
	// BudgetUSD caps the session's spend; 0 is none.
	BudgetUSD float64
	// Servers are the session's MCP servers (an mcp.json's mcpServers):
	// every one an entry of the lease's sessionproxy (harness.ProxyRelay),
	// so no credential is on the command line or in the config.
	Servers map[string]any
	// Env is added to the node's environment.
	Env []string
	// Unset are the node's variables the session must not see.
	Unset []string
	// Secrets are masked in anything the session's error carries.
	Secrets []string
	// Mailbox is the lease's: turns and escalations for the live session,
	// and the finish that closes a one-shot job's input. A session with
	// none takes its first message alone.
	Mailbox *Mailbox
	// Ladder is the lease's model policy, stepped by MsgEscalate and by
	// the session's own stall; nil never escalates.
	Ladder *escalate.Ladder
	// Thread keeps the input open between turns, for the thread's next
	// turn; a one-shot job's input closes once a turn ends with no more
	// to do.
	Thread bool
	// ParkAfter is a thread's quiet time: once no turn has been in flight
	// for it, the session's input closes and the session ends parked,
	// for the lease to hand back with cause parked and its session_ref
	// (projectmesh-io/api ADR 001, Threads). Zero never parks.
	ParkAfter time.Duration
	// Resume is a session this node parked earlier, which the session
	// resumes (`claude -p --resume <id>`) with its context, rather than
	// starting afresh.
	Resume string
	// OnEvent hears every event of the session's output as it comes:
	// the lease's liveness (progress_at) and the evidence hooks. It must
	// not block.
	OnEvent func(harness.Event)
	// Pod is the lease's pod when its actor opened one (the brief named
	// pods and the node has a sandbox runtime): a sandboxed session runs
	// its agent there, as sandbox.AgentName(pod). "" is none.
	Pod string
}

// SessionResult is how a session ended.
type SessionResult struct {
	SessionID string
	Text      string
	CostUSD   float64
	Turns     int
	Usage     *harness.Usage
	// Models is the session's spend by model, as the CLI names each.
	Models map[string]harness.ModelSpend
	// ModelTurns are Turns by model, as the CLI names each.
	ModelTurns map[string]int
	Duration   time.Duration
	// Parked is a thread's session that ended after ParkAfter of quiet.
	Parked bool
}

// Session runs one harnessed session to its end. A cancelled ctx kills
// it and answers ctx's error.
type Session interface {
	Run(ctx context.Context, spec SessionSpec) (SessionResult, error)
}

// ClaudeSession is a streaming `claude -p` session (internal/harness/
// session; PR 2.10): stream-json in and out, the prompt its first
// message, the input held open for the lease's mailbox until the job is
// done.
type ClaudeSession struct {
	// Binary is the claude executable; "claude" on PATH when empty.
	Binary string
	// ExtraArgs are added to every session (--dangerously-skip-permissions
	// on an unattended node).
	ExtraArgs []string
	// Stderr hears the process's stderr; discarded when nil.
	Stderr io.Writer
	// StallAfter is how long a turn may stay quiet before the session
	// escalates on stall, when its kind does; 10 minutes when zero.
	StallAfter time.Duration
	// ControlTimeout bounds one control request; 30 s when zero.
	ControlTimeout time.Duration
	// Logf hears the session's escalations; nothing is logged when nil.
	Logf func(format string, args ...any)
}

// Args is spec's command line after -p and the stream flags.
func (c *ClaudeSession) Args(spec SessionSpec) []string {
	var args []string
	if spec.Resume != "" {
		args = append(args, "--resume", spec.Resume)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(spec.BudgetUSD, 'f', 2, 64))
	}
	if cfg, err := json.Marshal(harness.SessionSettings(nil)); err == nil {
		args = append(args, "--settings", string(cfg))
	}
	args = append(args, harness.MCPArgs(spec.Servers)...)
	return append(args, c.ExtraArgs...)
}

// Run implements Session.
func (c *ClaudeSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	var tail bytes.Buffer
	stderr := io.Writer(&limited{b: &tail, n: 8 << 10})
	if c.Stderr != nil {
		stderr = io.MultiWriter(stderr, c.Stderr)
	}
	results := make(chan harness.Event, 16)
	lastEvent := make(chan struct{}, 1)
	loopDone := make(chan struct{})
	onEvent := func(ev harness.Event) {
		if spec.OnEvent != nil {
			spec.OnEvent(ev)
		}
		select {
		case lastEvent <- struct{}{}:
		default:
		}
		if ev.Type == "result" {
			select {
			case results <- ev:
			case <-loopDone: // nobody counts turns once the input is closed
			}
		}
	}
	s, err := session.Start(ctx, session.Options{
		Binary: c.Binary, Args: c.Args(spec), Dir: spec.Workspace,
		Env:    harness.SessionEnviron(os.Environ(), harness.Job{Env: spec.Env, Unset: spec.Unset}),
		Stderr: stderr, OnEvent: onEvent,
	}, spec.Prompt)
	if s == nil {
		return SessionResult{}, fmt.Errorf("claude: %s", harness.Redact(err.Error(), spec.Secrets...))
	}
	parked := false
	if err == nil {
		parked = c.loop(ctx, s, spec, results, lastEvent)
	}
	close(loopDone)
	s.CloseInput()
	res, waitErr := s.Wait()
	sr := SessionResult{SessionID: res.SessionID, Text: res.Text, CostUSD: res.CostUSD, Turns: res.Turns,
		Usage: res.Stream.Usage, Models: res.Models, ModelTurns: res.ModelTurns, Duration: res.Duration, Parked: parked}
	if ctx.Err() != nil {
		return sr, ctx.Err()
	}
	if err != nil {
		return sr, fmt.Errorf("claude: %s", harness.Redact(err.Error(), spec.Secrets...))
	}
	if waitErr != nil {
		msg := strings.TrimSpace(tail.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return sr, errors.New("claude: " + harness.Redact(lastLines(msg, 5), spec.Secrets...))
	}
	if !res.Success {
		return sr, errors.New("claude: the session ended with an error: " + harness.Redact(lastLines(res.Text, 5), spec.Secrets...))
	}
	return sr, nil
}

// loop feeds the live session until its input closes: the mailbox's
// turns and escalations as they come, a stall escalation when a turn
// goes quiet, and — once a turn ends with nothing more to do — the end
// of a one-shot job's input. The product answering the finish tool
// closes the input at once: the turn in flight still runs to its result.
// A thread whose session has had no turn in flight for its ParkAfter
// parks: the input closes and loop answers true.
func (c *ClaudeSession) loop(ctx context.Context, s *session.Session, spec SessionSpec, results <-chan harness.Event, lastEvent <-chan struct{}) bool {
	var mail <-chan Message
	var finished <-chan struct{}
	if spec.Mailbox != nil {
		mail, finished = spec.Mailbox.C(), spec.Mailbox.Finished()
	}
	stallAfter := c.StallAfter
	if stallAfter <= 0 {
		stallAfter = 10 * time.Minute
	}
	stall := time.NewTimer(stallAfter)
	defer stall.Stop()
	inFlight := 1    // the first message
	carryOn := false // an escalation in the turn in flight: the next turn says so
	// a thread's park timer runs while no turn is in flight
	var park <-chan time.Time
	var parkTimer *time.Timer
	quiet := func() {
		if spec.Thread && spec.ParkAfter > 0 {
			parkTimer = time.NewTimer(spec.ParkAfter)
			park = parkTimer.C
		}
	}
	busy := func() {
		if parkTimer != nil {
			parkTimer.Stop()
			parkTimer, park = nil, nil
		}
	}
	defer busy()
	send := func(text string) {
		if err := s.Send(text); err != nil {
			c.logf("nodev1: lease %s: a turn for the session: %v", spec.Label, err)
			return
		}
		inFlight++
		busy()
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-s.Done():
			return false
		case <-finished:
			s.CloseInput()
			return false
		case <-park:
			if inFlight > 0 {
				busy()
				continue
			}
			c.logf("nodev1: lease %s: the thread was quiet for %s; parking its session", spec.Label, spec.ParkAfter)
			s.CloseInput()
			return true
		case <-lastEvent:
			stall.Reset(stallAfter)
		case <-results:
			inFlight = max(inFlight-1, 0)
			if inFlight > 0 {
				continue
			}
			switch {
			case carryOn:
				carryOn = false
				send(fmt.Sprintf("The node moved this session to the model %s. Carry on with the job where you left off.", spec.Ladder.Model()))
			case !spec.Thread:
				s.CloseInput()
				return false
			default:
				busy()
				quiet()
			}
		case m := <-mail:
			switch m.Kind {
			case MsgTurn:
				send(m.Text)
			case MsgInterrupt:
				if err := c.control(ctx, func(ctx context.Context) error { return s.Interrupt(ctx) }); err != nil {
					c.logf("nodev1: lease %s: interrupt: %v", spec.Label, err)
				}
			case MsgEscalate:
				if c.escalate(ctx, s, spec, m.On, m.Text, false) && inFlight > 0 {
					carryOn = true
				}
			}
		case <-stall.C:
			stall.Reset(stallAfter)
			if inFlight == 0 || spec.Ladder == nil {
				continue
			}
			if _, err := spec.Ladder.Peek(escalate.OnStall); err != nil {
				continue // the supervisor's stalled hand-back is wave 3's
			}
			if c.escalate(ctx, s, spec, escalate.OnStall, fmt.Sprintf("no output for %s", stallAfter), true) {
				// the interrupted turn ends in its result; the next turn
				// carries on
				carryOn = true
			}
		}
	}
}

// escalate steps the lease's ladder for on and moves the live session to
// the next model with set_model — after interrupting the turn in flight
// when interrupt — and answers whether it moved. A step the session
// refuses is marked failed on the ladder and the session stays where it
// was. (The spike of PR 2.11 found set_model works over stream-json
// input on claude 2.1.312, so the fallback of an interrupt and a `claude
// --resume <id> --model <next>` is not built.)
func (c *ClaudeSession) escalate(ctx context.Context, s *session.Session, spec SessionSpec, on, why string, interrupt bool) bool {
	if spec.Ladder == nil {
		return false
	}
	step, err := spec.Ladder.Step(on, why)
	if err != nil {
		c.logf("nodev1: lease %s: no escalation on %s: %v", spec.Label, on, err)
		return false
	}
	if interrupt {
		if err := c.control(ctx, func(ctx context.Context) error { return s.Interrupt(ctx) }); err != nil {
			c.logf("nodev1: lease %s: interrupting the stalled turn: %v", spec.Label, err)
		}
	}
	if err := c.control(ctx, func(ctx context.Context) error { _, err := s.SetModel(ctx, step.To); return err }); err != nil {
		spec.Ladder.Failed(step.To, err.Error())
		c.logf("nodev1: lease %s: escalating to %s on %s: %v", spec.Label, step.To, on, err)
		return false
	}
	c.logf("nodev1: lease %s: escalated from %s to %s on %s", spec.Label, orDefault(step.From), step.To, on)
	return true
}

// control runs one control request under ControlTimeout.
func (c *ClaudeSession) control(ctx context.Context, do func(context.Context) error) error {
	d := c.ControlTimeout
	if d <= 0 {
		d = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return do(ctx)
}

func (c *ClaudeSession) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func orDefault(model string) string {
	if model == "" {
		return "the default model"
	}
	return model
}

// limited keeps the first n bytes written.
type limited struct {
	b *bytes.Buffer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
