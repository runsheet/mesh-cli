package nodev1

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// mailSession stands in for a streaming session: it makes calls through
// the proxy as stubSession does, emits an event, then answers once the
// mailbox brings a message or the finish arrives.
type mailSession struct {
	calls []string

	mu       sync.Mutex
	spec     SessionSpec
	got      []Message
	finished bool
	started  chan struct{}
}

func (s *mailSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	s.mu.Lock()
	s.spec = spec
	s.mu.Unlock()
	for _, c := range s.calls {
		server, tool, _ := strings.Cut(c, "/")
		if err := callThroughProxy(ctx, spec.Servers[server], tool); err != nil {
			return SessionResult{}, err
		}
	}
	spec.OnEvent(harness.Event{Type: "assistant"})
	close(s.started)
	select {
	case m := <-spec.Mailbox.C():
		s.mu.Lock()
		s.got = append(s.got, m)
		s.mu.Unlock()
	case <-spec.Mailbox.Finished():
		s.mu.Lock()
		s.finished = true
		s.mu.Unlock()
	case <-ctx.Done():
		return SessionResult{}, ctx.Err()
	case <-time.After(10 * time.Second):
	}
	return SessionResult{SessionID: "sess-mail", Text: "done"}, nil
}

// A turn on the node's event stream reaches the lease's live session
// through its mailbox: ids only, with the brief's turn tool to read it.
// The session's output is the lease's progress.
func TestAStreamTurnReachesTheLiveSession(t *testing.T) {
	sess := &mailSession{started: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.HeartbeatEvery = time.Hour
	job := g.offer(t, func(b *meshapi.Brief) {
		b.TurnTool = "conform.turn"
		b.Thread = &meshapi.Thread{ParkAfterS: 60}
	})
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	select {
	case <-sess.started:
	case <-time.After(20 * time.Second):
		t.Fatal("the session never started")
	}
	lease := g.fake.LeaseOf(job)
	g.rt.mu.Lock()
	progress := g.rt.live[lease].progressAt
	g.rt.mu.Unlock()
	data, _ := json.Marshal(meshapi.TurnEvent{LeaseID: lease, JobID: job, TurnID: "turn-7", Seq: 3})
	deadline := time.Now().Add(10 * time.Second)
	for g.fake.Stream(meshapi.NodeEvent{Event: meshapi.EventTurn, Data: data}) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the runtime did not stop")
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.got) != 1 || sess.got[0].Kind != MsgTurn || !strings.Contains(sess.got[0].Text, "turn_id turn-7 (seq 3)") || !strings.Contains(sess.got[0].Text, "`conform.turn`") {
		t.Fatalf("the session got %+v", sess.got)
	}
	if !sess.spec.Thread || sess.spec.Ladder == nil || progress.IsZero() {
		t.Fatalf("spec thread %v ladder %v progress %v", sess.spec.Thread, sess.spec.Ladder, progress)
	}
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("ended %q", g.fake.Ended(lease))
	}
}

// The product answering the finish tool closes the one-shot job's
// input: the mailbox's finish fires.
func TestTheFinishToolClosesTheInput(t *testing.T) {
	sess := &mailSession{calls: []string{"conform/brief", "conform/submit"}, started: make(chan struct{})}
	g := newRig(t, sess)
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if !sess.finished || sess.spec.Thread {
		t.Fatalf("finished %v, thread %v", sess.finished, sess.spec.Thread)
	}
}

// The escalate node tool answers the next model and leaves the step for
// the session's loop; a kind that does not escalate when asked refuses.
func TestTheEscalateTool(t *testing.T) {
	mb := NewMailbox()
	tool := escalateTool(escalate.New(meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet"}, On: []string{escalate.OnAsked}}), mb)
	if _, err := tool.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("no reason was no error")
	}
	out, err := tool.Call(context.Background(), map[string]any{"reason": "the refactor is too big"})
	if err != nil || !strings.Contains(out, `"to":"sonnet"`) {
		t.Fatalf("%s %v", out, err)
	}
	if m := <-mb.C(); m.Kind != MsgEscalate || m.On != escalate.OnAsked || m.Text != "the refactor is too big" {
		t.Fatalf("%+v", m)
	}
	tool = escalateTool(escalate.New(meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet"}, On: []string{escalate.OnRed}}), mb)
	if _, err := tool.Call(context.Background(), map[string]any{"reason": "please"}); err == nil || !strings.Contains(err.Error(), "does not escalate on asked") {
		t.Fatalf("%v", err)
	}
}

// The lease's usage carries the session's escalations.
func TestUsageCarriesEscalations(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s", CostUSD: 0.1, Models: map[string]harness.ModelSpend{"claude-haiku": {CostUSD: 0.1}}}}
	sess.inspect = func(spec SessionSpec) { spec.Ladder.Step(escalate.OnRed, "red suite") }
	g := newRig(t, sess)
	g.offer(t, func(b *meshapi.Brief) {
		b.Harness.Model = meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet"}, On: []string{escalate.OnRed}}
	})
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	var u meshapi.UsageRequest
	json.Unmarshal(g.fake.Calls("usage")[0].Body, &u)
	if len(u.Escalations) != 1 || u.Escalations[0].To != "sonnet" || u.Escalations[0].On != "red" || len(u.Usage) != 1 || u.Usage[0].Model != "claude-haiku" {
		t.Fatalf("usage %s", g.fake.Calls("usage")[0].Body)
	}
}
