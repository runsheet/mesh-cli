package nodev1

import (
	"context"
	"errors"
	"time"

	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/stoprules"
	"projectmesh.io/mesh-cli/internal/transcript"
)

// A lease's session runs under its kind's stop rules, is counted, and —
// when the brief names the product's location — has its transcript sent
// there, redacted (projectmesh-io/api docs/adr/adr-001-amendment-
// learnings-and-transcripts.md). All three read the session's own
// stream, on the node; none of it reaches Mesh but the counts and the
// upload's {bytes, digest, chunks, failed}, in the usage report.
//
// A rule whose action is end stops the session at once: the lease fails
// with cause stopped_by_rule, the rule's name first in the reason. One
// whose action is escalate interrupts the turn in flight and moves the
// session up its kind's model ladder with set_model; with no model left
// to move to, it ends the session as end would.

// ruleTick is how often the no_progress rules are checked.
const ruleTick = 5 * time.Second

// ruleStop is the cause a rule's end cancels the session with.
type ruleStop struct{ fired stoprules.Fired }

func (e *ruleStop) Error() string { return "the stop rule " + e.fired.Reason() }

// stoppedBy is the rule that ended the session ctx ran, or nil.
func stoppedBy(ctx context.Context) *ruleStop {
	var rs *ruleStop
	if errors.As(context.Cause(ctx), &rs) {
		return rs
	}
	return nil
}

// leaseWatch is one lease's stop rules, counts and transcript upload.
type leaseWatch struct {
	r      *Runtime
	lease  string
	watch  *stoprules.Watch
	upload *transcript.Uploader
	stop   context.CancelCauseFunc
	mb     *Mailbox
	ladder *escalate.Ladder
	done   chan struct{}
}

// watchLease puts the lease's session under its watch: spec's events
// and raw lines reach it, and the context answered is the session's,
// which a rule's end cancels. close the watch once the session ends.
func (r *Runtime) watchLease(ctx context.Context, b *meshapi.Brief, workspace string, mb *Mailbox, ladder *escalate.Ladder,
	spec *SessionSpec, tokens *sessionTokens, grant *gitrelay.Grant, account *sessionAccount) (context.Context, *leaseWatch) {
	sctx, stop := context.WithCancelCause(ctx)
	w := &leaseWatch{r: r, lease: b.LeaseID, watch: stoprules.New(b.StopRules, workspace, time.Now()),
		stop: stop, mb: mb, ladder: ladder, done: make(chan struct{})}
	if t := b.Transcript; t != nil && t.UploadURL != "" {
		w.upload = transcript.New(*t, b.LeaseID, leaseSecrets(tokens, grant, account, spec), transcript.Options{Logf: r.logf})
		spec.OnLine = w.upload.Line
	}
	onEvent := spec.OnEvent
	spec.OnEvent = func(ev harness.Event) {
		if onEvent != nil {
			onEvent(ev)
		}
		for _, f := range w.watch.Observe(ev, time.Now()) {
			w.act(f)
		}
	}
	for _, rule := range b.StopRules {
		if rule.NoProgress != nil {
			go w.tick()
			break
		}
	}
	return sctx, w
}

// tick checks the no_progress rules until the watch closes.
func (w *leaseWatch) tick() {
	t := time.NewTicker(ruleTick)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case now := <-t.C:
			for _, f := range w.watch.Tick(now) {
				w.act(f)
			}
		}
	}
}

// act does what a rule that fired asks: escalate when the ladder has a
// model left, else end.
func (w *leaseWatch) act(f stoprules.Fired) {
	if f.Rule.Action == meshapi.StopActionEscalate {
		if w.ladder != nil {
			if to, err := w.ladder.Peek(escalate.OnRule); err == nil {
				err := w.mb.Post(Message{Kind: MsgEscalate, On: escalate.OnRule, Text: f.Reason(), Interrupt: true})
				if err == nil {
					w.r.logf("nodev1: lease %s: the stop rule %s escalates the session to %s", w.lease, f.Reason(), to)
					return
				}
				w.r.logf("nodev1: lease %s: the stop rule %s could not escalate (%v); ending the session", w.lease, f.Reason(), err)
			} else {
				w.r.logf("nodev1: lease %s: the stop rule %s has no model to escalate to (%v); ending the session", w.lease, f.Reason(), err)
			}
		}
	}
	w.r.logf("nodev1: lease %s: the stop rule %s ends the session", w.lease, f.Reason())
	w.stop(&ruleStop{fired: f})
}

// close stops the watch and answers the lease's counts, with what its
// transcript upload sent when it had one.
func (w *leaseWatch) close() meshapi.Counts {
	close(w.done)
	c := w.watch.Counts()
	if w.upload != nil {
		t := w.upload.Close()
		c.Transcript = &t
	}
	return c
}

// leaseSecrets are the lease's own secrets as they stand when asked: its
// Latchkey session's request credential and token, every git token its
// relay has minted, the model credential its relay holds (which never
// reaches the session, so is redacted only in case), and what the
// session's spec masks. A transcript line is redacted of every one.
func leaseSecrets(tokens *sessionTokens, grant *gitrelay.Grant, account *sessionAccount, spec *SessionSpec) transcript.Secrets {
	return func() []string {
		var out []string
		if tokens != nil {
			out = append(out, tokens.secrets()...)
		}
		for _, t := range grant.Minted() {
			out = append(out, t.Value)
		}
		out = append(out, account.secrets()...)
		return append(out, spec.Secrets...)
	}
}

// secrets are the model credentials the account's relay holds.
func (a *sessionAccount) secrets() []string {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{a.own}
	if a.cred != nil {
		out = append(out, a.cred.Secret)
	}
	return out
}

// secrets are the session's request credential and its token of the
// moment.
func (s *sessionTokens) secrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{s.token}
	if s.cred != nil {
		out = append(out, s.cred.RequestToken)
	}
	return out
}
