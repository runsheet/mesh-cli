package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/harness/codexsession"
)

// CodexSession is a streaming Codex session over `codex app-server`
// (internal/harness/codexsession): its notifications — item.started,
// agentMessage deltas, turn.completed, turn.failed and the rest — are
// rewritten into stream-json's line shape and harness.Event, the same
// shape ClaudeSession's own output carries, so a lease's transcript
// upload (stoprules.go) and liveness and account tracking (lease.go)
// read either adapter the same way (specs/mesh-v1-gaps-for-products.md
// §3, "a product parses one format"). A session's model ladder has no
// Codex equivalent — app-server has no set_model — so MsgEscalate and
// a stalled turn are logged and otherwise left alone.
type CodexSession struct {
	// Binary is the codex executable; "codex" on PATH when empty.
	Binary string
	// PodBinary is the codex executable in a lease's pod (InPod): the
	// node image's "codex" when empty.
	PodBinary string
	// ExtraArgs are added to every session.
	ExtraArgs []string
	// Stderr hears the process's stderr; discarded when nil.
	Stderr io.Writer
	// ControlTimeout bounds one control request (Interrupt); 30s when
	// zero.
	ControlTimeout time.Duration
	// Logf hears the session's notices; nothing is logged when nil.
	Logf func(format string, args ...any)
}

// Args is spec's command line after "app-server".
func (c *CodexSession) Args(spec SessionSpec) []string {
	var args []string
	if spec.Resume != "" {
		args = append(args, "resume", spec.Resume)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	return append(args, c.ExtraArgs...)
}

// Run implements Session.
func (c *CodexSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	var tail bytes.Buffer
	stderr := io.Writer(&limited{b: &tail, n: 8 << 10})
	if c.Stderr != nil {
		out := c.Stderr
		if spec.Injected != nil {
			// the session's stderr reaches the node's log without an
			// injected value (canary.go)
			sr := &streamRedact{v: spec.Injected, w: c.Stderr}
			defer sr.Flush()
			out = sr
		}
		stderr = io.MultiWriter(stderr, out)
	}

	tr := &codexTranslator{model: spec.Model}
	turnDone := make(chan struct{}, 16)
	lastEvent := make(chan struct{}, 1)
	loopDone := make(chan struct{})
	onNotification := func(n codexsession.Notification) {
		ended, line := tr.observe(n)
		if line != nil {
			if spec.OnLine != nil {
				spec.OnLine(line)
			}
			if spec.OnEvent != nil {
				var ev harness.Event
				if json.Unmarshal(line, &ev) == nil {
					spec.OnEvent(ev)
				}
			}
		}
		select {
		case lastEvent <- struct{}{}:
		default:
		}
		if ended {
			select {
			case turnDone <- struct{}{}:
			case <-loopDone: // nobody counts turns once the input is closed
			}
		}
	}
	opts := codexsession.Options{
		Binary: c.Binary, Args: c.Args(spec), Dir: spec.Workspace,
		Env:    harness.SessionEnviron(os.Environ(), harness.Job{Env: spec.Env, Unset: spec.Unset}),
		Stderr: stderr, OnEvent: onNotification,
	}
	if spec.InPod != nil {
		// the pod's environment is the session's own, never the node's
		opts.Binary, opts.Env = c.PodBinary, nil
		if opts.Binary == "" {
			opts.Binary = "codex"
		}
		opts.Spawn = func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
			return spec.InPod(ctx, argv, spec.Env, stdin, stdout, stderr)
		}
	}
	s, err := codexsession.Start(ctx, opts, spec.Prompt)
	if s == nil {
		return SessionResult{}, fmt.Errorf("codex: %s", spec.redact(err.Error()))
	}
	parked := false
	if err == nil {
		parked = c.loop(ctx, s, spec, turnDone, lastEvent)
	}
	close(loopDone)
	s.CloseInput()
	res, waitErr := s.Wait()
	sr := SessionResult{SessionID: res.ThreadID, Text: tr.lastText, CostUSD: tr.costUSD, Turns: tr.turns,
		Usage: tr.usage, Duration: res.Duration, Parked: parked}
	if ctx.Err() != nil {
		return sr, ctx.Err()
	}
	// a pod that ended before the first message went in says why in its
	// own end, not in the write that found the input closed
	if err != nil && (spec.InPod == nil || waitErr == nil) {
		return sr, fmt.Errorf("codex: %s", spec.redact(err.Error()))
	}
	if waitErr != nil {
		msg := strings.TrimSpace(tail.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		msg = "codex: " + spec.redact(lastLines(msg, 5))
		if spec.InPod != nil {
			// the pod's end (an *sandbox.ExitError, a *sandbox.LimitError)
			// stays readable: an exit 137 or 143 the actor did not cause
			// is pod_killed (podKilled)
			return sr, &podEnd{msg: msg, err: waitErr}
		}
		return sr, errors.New(msg)
	}
	if tr.failed {
		return sr, errors.New("codex: the session ended with an error: " + spec.redact(lastLines(tr.failReason, 5)))
	}
	return sr, nil
}

// loop feeds the live session until its input closes: the mailbox's
// turns as they come, sent as a fresh turn/start or, with one already
// in flight, a turn/steer (Session.Send chooses); an interrupt turn or
// a bare MsgInterrupt sends turn/interrupt first. MsgEscalate has
// nothing to act on — app-server has no set_model — and is only
// logged. Once a turn ends (turnDone, fed by turn.completed or
// turn.failed) with nothing more to do, a one-shot job's input closes;
// a thread whose session has had no turn in flight for its ParkAfter
// parks the same way ClaudeSession's loop does.
func (c *CodexSession) loop(ctx context.Context, s *codexsession.Session, spec SessionSpec, turnDone <-chan struct{}, lastEvent <-chan struct{}) bool {
	var mail <-chan Message
	var finished <-chan struct{}
	if spec.Mailbox != nil {
		mail, finished = spec.Mailbox.C(), spec.Mailbox.Finished()
		// no turn reaches the session once the loop stops reading
		defer spec.Mailbox.closeInput()
	}
	stallAfter := 10 * time.Minute
	stall := time.NewTimer(stallAfter)
	defer stall.Stop()
	inFlight := 1 // the first message
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
	send := func(text string) error {
		if err := s.Send(text); err != nil {
			c.logf("nodev1: lease %s: a turn for the session: %v", spec.Label, err)
			return err
		}
		inFlight++
		busy()
		return nil
	}
	handle := func(m Message) {
		switch m.Kind {
		case MsgTurn:
			// an interrupt turn stops the turn in flight, then goes in
			// at once: the interrupted turn's result then leaves this
			// one in flight
			if m.Interrupt && inFlight > 0 {
				if err := c.control(ctx, func(ctx context.Context) error { return s.Interrupt(ctx) }); err != nil {
					c.logf("nodev1: lease %s: interrupting for a turn: %v", spec.Label, err)
				}
			}
			err := send(m.Text)
			if m.Sent != nil {
				m.Sent(err)
			}
		case MsgInterrupt:
			// with no turn in flight there is nothing to stop
			if inFlight == 0 {
				return
			}
			if err := c.control(ctx, func(ctx context.Context) error { return s.Interrupt(ctx) }); err != nil {
				c.logf("nodev1: lease %s: interrupt: %v", spec.Label, err)
			}
		case MsgEscalate:
			c.logf("nodev1: lease %s: codex sessions have no model ladder; escalation on %s left alone", spec.Label, m.On)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-s.Done():
			return false
		case <-finished:
			// select picks at random among ready cases: what was posted
			// before Finish (an interrupt, a last turn) is handled before
			// the input closes, never dropped because Finish won the race
			drainMailbox(mail, handle)
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
		case <-turnDone:
			inFlight = max(inFlight-1, 0)
			if inFlight > 0 {
				continue
			}
			if !spec.Thread {
				s.CloseInput()
				return false
			}
			busy()
			quiet()
		case m := <-mail:
			handle(m)
		case <-stall.C:
			stall.Reset(stallAfter)
		}
	}
}

// control runs one control request under ControlTimeout.
func (c *CodexSession) control(ctx context.Context, do func(context.Context) error) error {
	d := c.ControlTimeout
	if d <= 0 {
		d = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return do(ctx)
}

func (c *CodexSession) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// codexTranslator turns app-server Notifications into stream-json
// lines (codexTranslator.observe's second answer) and keeps the
// session's running turns, cost, usage and text so CodexSession.Run
// can build its SessionResult without reusing harness.ParseStream (its
// Event.Usage carries an unexported type codexTranslator cannot name
// from this package; its own fields do not need it). A notification
// app-server adds with no stream-json equivalent — item.started,
// item.completed and the rest — answers no line, as ParseStream skips
// an event type it does not know. Not safe for concurrent use: it is
// only ever called from app-server's own single reading goroutine
// (codexsession's doc comment), and read back only after Session.Wait
// has returned, by when that goroutine is done.
type codexTranslator struct {
	model string

	text       string // the turn in flight's agentMessage text so far
	lastText   string // the last turn that ended
	turns      int
	costUSD    float64
	usage      *harness.Usage
	failed     bool
	failReason string
}

// observe updates tr for notification n and answers its equivalent
// stream-json line (nil for a notification with none) and whether it
// ended a turn (turn.completed or turn.failed) — the loop's cue to
// treat it as ClaudeSession's own "result" event does.
func (tr *codexTranslator) observe(n codexsession.Notification) (ended bool, line []byte) {
	switch n.Method {
	case "agentMessage":
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(n.Params, &p)
		tr.text += p.Text
		return false, assistantLine(tr.text)
	case "turn.completed":
		var p struct {
			Usage *harness.CodexUsage `json:"usage"`
		}
		_ = json.Unmarshal(n.Params, &p)
		tr.endTurn(p.Usage)
		return true, resultLine(true, tr.lastText, tr.turns, tr.costUSD, tr.usage)
	case "turn.failed":
		var p struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Usage *harness.CodexUsage `json:"usage"`
		}
		_ = json.Unmarshal(n.Params, &p)
		tr.endTurn(p.Usage)
		tr.failed = true
		tr.failReason = p.Error.Message
		return true, resultLine(false, tr.lastText, tr.turns, tr.costUSD, tr.usage)
	default:
		return false, nil
	}
}

// endTurn closes the turn in flight: its text becomes the session's
// last, its usage (docs/codex-events.md's shape) is added to the
// session's and priced at the model's rate (harness.CodexPrices), an
// unpriced model counted with no cost.
func (tr *codexTranslator) endTurn(u *harness.CodexUsage) {
	tr.turns++
	tr.lastText = tr.text
	tr.text = ""
	if u == nil {
		return
	}
	if tr.usage == nil {
		tr.usage = &harness.Usage{Model: tr.model}
	}
	tr.usage.InputTokens += u.InputTokens
	tr.usage.CachedInputTokens += u.CachedInputTokens
	tr.usage.CacheWriteInputTokens += u.CacheWriteInputTokens
	tr.usage.OutputTokens += u.OutputTokens
	tr.usage.ReasoningOutputTokens += u.ReasoningOutputTokens
	if price, ok := harness.CodexPrices[tr.model]; ok {
		tr.costUSD += price.Cost(*u)
	} else {
		tr.usage.Unpriced = true
	}
}

// assistantLine is an agentMessage's running text as a stream-json
// assistant event.
func assistantLine(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
	return b
}

// resultLine is a turn.completed or turn.failed as a stream-json
// result event, cost and usage the session's running total
// (ParseStream's own doc comment: "that turn's alone and its
// modelUsage and total_cost_usd the whole session's so far").
func resultLine(success bool, text string, turns int, costUSD float64, usage *harness.Usage) []byte {
	subtype := "success"
	if !success {
		subtype = "error"
	}
	m := map[string]any{
		"type":           "result",
		"subtype":        subtype,
		"is_error":       !success,
		"num_turns":      turns,
		"total_cost_usd": costUSD,
		"result":         text,
	}
	if usage != nil {
		m["usage"] = map[string]any{
			"input_tokens":                usage.InputTokens,
			"cache_read_input_tokens":     usage.CachedInputTokens,
			"cache_creation_input_tokens": usage.CacheWriteInputTokens,
			"output_tokens":               usage.OutputTokens,
		}
	}
	b, _ := json.Marshal(m)
	return b
}
