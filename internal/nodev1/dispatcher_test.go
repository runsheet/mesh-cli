package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

func askTurn(seq int64) meshapi.Turn {
	return meshapi.Turn{TurnID: fmt.Sprintf("ask-%d", seq), Seq: seq, Kind: meshapi.TurnKindAsk, From: meshapi.TurnFromProduct}
}

// TestAskQueue (goal one-assistant-dispatcher-per-org-answers-every-ask
// P4, P5): a dispatcher's queue starts at most its cap of sub-agents,
// oldest ask first, the rest waiting in order; it takes an ask once,
// whichever way it comes, and no turn that is not an ask; and its
// turns_seen never passes an ask not finished — one queued, running, or
// whose finish Mesh never took — so Mesh acks none of them.
func TestAskQueue(t *testing.T) {
	q := newAskQueue(4, 0, nil)
	for seq := int64(1); seq <= 10; seq++ {
		if !q.offer(askTurn(seq)) {
			t.Fatalf("ask %d was not queued", seq)
		}
	}
	if q.offer(askTurn(3)) || q.offer(meshapi.Turn{TurnID: "t-11", Seq: 11, Kind: meshapi.TurnKindTurn}) {
		t.Fatal("an ask queued twice, or a turn that is no ask queued")
	}
	var started []int64
	for {
		t, ok := q.next()
		if !ok {
			break
		}
		started = append(started, t.Seq)
	}
	if !slices.Equal(started, []int64{1, 2, 3, 4}) {
		t.Fatalf("started %v, want the four oldest", started)
	}
	q.raise(10)
	if seen := *q.TurnsSeen(); seen != 0 {
		t.Fatalf("turns_seen %d with ask 1 unfinished", seen)
	}
	// ask 2 finishes first: its turn alone is done, turns_seen stays
	// below ask 1, and the fifth ask starts in its place
	q.done(askTurn(2), true)
	if n, ok := q.next(); !ok || n.Seq != 5 {
		t.Fatalf("after a finish the next is %+v %v, want ask 5", n, ok)
	}
	if seen := *q.TurnsSeen(); seen != 0 {
		t.Fatalf("turns_seen %d with ask 1 unfinished", seen)
	}
	// ask 1 ends but Mesh never took its finish: it stays unacked
	q.done(askTurn(1), false)
	if seen := *q.TurnsSeen(); seen != 0 {
		t.Fatalf("turns_seen %d past an ask whose finish Mesh never took", seen)
	}
	if q.offer(askTurn(2)) {
		t.Fatal("a finished ask was queued again")
	}
	if _, _, out := q.counts(); out != 9 {
		t.Fatalf("%d outstanding, want 9", out)
	}

	// a fresh lease after a restart starts at the brief's acked_through
	q = newAskQueue(0, 6, nil)
	if q.cap != DefaultDispatcherCap || q.offer(askTurn(5)) || !q.offer(askTurn(7)) || *q.TurnsSeen() != 6 {
		t.Fatalf("a restarted lease's queue: cap %d seen %d", q.cap, *q.TurnsSeen())
	}
	q.off()
	if q.TurnsSeen() != nil {
		t.Fatal("a queue turned off still says turns_seen")
	}
	for _, c := range []struct{ kind, node, want int }{{0, 0, 4}, {2, 0, 2}, {8, 0, 4}, {8, 6, 6}, {3, 6, 3}, {0, 2, 2}} {
		if got := dispatcherCap(c.kind, c.node); got != c.want {
			t.Errorf("dispatcherCap(%d, %d) = %d, want %d", c.kind, c.node, got, c.want)
		}
	}
}

// TestADispatcherAnswersTenAsksFourAtOnce (acceptance check 2, P2 and
// P4): ten asks for one tenant arrive at once on one dispatcher whose
// cap is four. One dispatcher answers all ten; four sub-agents run at
// once and no more; the rest start, oldest first, as others finish; and
// each is finished through Mesh once.
func TestADispatcherAnswersTenAsksFourAtOnce(t *testing.T) {
	q := newAskQueue(4, 0, nil)
	var running, most atomic.Int32
	var mu sync.Mutex
	var order, finished []string
	release := make(chan struct{})
	d := &dispatcher{lease: "lease-1", q: q, logf: t.Logf,
		run: func(ctx context.Context, a meshapi.Turn) (askEnd, error) {
			n := running.Add(1)
			for {
				m := most.Load()
				if n <= m || most.CompareAndSwap(m, n) {
					break
				}
			}
			<-release
			running.Add(-1)
			return askEnd{end: meshapi.AskEndAnswered, result: json.RawMessage(`{}`)}, nil
		},
		finish: func(ctx context.Context, a meshapi.Turn, e askEnd) error {
			mu.Lock()
			finished = append(finished, a.TurnID)
			mu.Unlock()
			return nil
		},
		started: func(a meshapi.Turn) {
			mu.Lock()
			order = append(order, a.TurnID)
			mu.Unlock()
		},
	}
	for seq := int64(1); seq <= 10; seq++ {
		q.offer(askTurn(seq))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { _, err := d.serve(ctx); served <- err }()
	waitFor(t, "four sub-agents", func() bool { return running.Load() == 4 })
	time.Sleep(50 * time.Millisecond)
	if r, qd, _ := q.counts(); r != 4 || qd != 6 {
		t.Fatalf("%d running and %d queued, want 4 and 6", r, qd)
	}
	close(release)
	waitFor(t, "every ask finished", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(finished) == 10
	})
	cancel()
	<-served
	if most.Load() != 4 {
		t.Fatalf("%d sub-agents ran at once, want 4", most.Load())
	}
	want := []string{"ask-1", "ask-2", "ask-3", "ask-4"}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order[:4], want) || !slices.Equal(order[4:], []string{"ask-5", "ask-6", "ask-7", "ask-8", "ask-9", "ask-10"}) {
		t.Fatalf("the asks started %v, want in the order they came", order)
	}
	if *q.TurnsSeen() != 0 {
		// Mesh never looked through any seq for this lease (no renewal
		// raised it): every ask finished, none left outstanding
		t.Fatalf("turns_seen %d", *q.TurnsSeen())
	}
	if _, _, out := q.counts(); out != 0 {
		t.Fatalf("%d asks outstanding after every finish", out)
	}
}

// A dispatcher with no ask for its park time parks; one whose lease ends
// returns its context's error, leaving the asks its sub-agents had not
// finished outstanding.
func TestADispatcherParksWhenQuiet(t *testing.T) {
	q := newAskQueue(2, 0, nil)
	d := &dispatcher{lease: "lease-1", q: q, logf: t.Logf, parkAfter: 100 * time.Millisecond,
		run: func(context.Context, meshapi.Turn) (askEnd, error) {
			return askEnd{end: meshapi.AskEndAnswered}, nil
		},
		finish: func(context.Context, meshapi.Turn, askEnd) error { return nil },
	}
	q.offer(askTurn(1))
	q.raise(1)
	parked, err := d.serve(context.Background())
	if !parked || err != nil {
		t.Fatalf("serve: parked %v, %v", parked, err)
	}
	q = newAskQueue(2, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d = &dispatcher{lease: "lease-2", q: q, logf: t.Logf,
		run: func(ctx context.Context, a meshapi.Turn) (askEnd, error) {
			<-ctx.Done()
			return askEnd{}, errAskNotRun
		},
		finish: func(context.Context, meshapi.Turn, askEnd) error {
			t.Error("an unanswered ask was finished")
			return nil
		},
	}
	q.offer(askTurn(1))
	q.offer(askTurn(2))
	go func() {
		waitFor(t, "two sub-agents", func() bool { r, _, _ := q.counts(); return r == 2 })
		cancel()
	}()
	if parked, err := d.serve(ctx); parked || err == nil {
		t.Fatalf("a lease ending under the dispatcher: parked %v, %v", parked, err)
	}
	if _, _, out := q.counts(); out != 2 || *q.TurnsSeen() != 0 {
		t.Fatalf("%d outstanding, seen %d: want both asks left for the next lease", out, *q.TurnsSeen())
	}
}

// askSession stands in for each sub-agent's claude: it calls the
// product's brief tool through the servers its spec names, then holds
// until every one of want sub-agents has started, so the test sees them
// run at once.
type askSession struct {
	want  int
	mu    sync.Mutex
	specs []SessionSpec
	all   chan struct{}
}

func (s *askSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	if len(s.specs) == s.want {
		close(s.all)
	}
	s.mu.Unlock()
	if err := callThroughProxy(ctx, spec.Servers["conform"], "brief"); err != nil {
		return SessionResult{}, err
	}
	select {
	case <-s.all:
	case <-ctx.Done():
		return SessionResult{}, ctx.Err()
	}
	return SessionResult{SessionID: "sess-" + spec.Label, Turns: 1}, nil
}

// TestTwoAsksOfOneTenantShareOneDispatcherLease (acceptance checks 2
// and 3; P2, P3): two asks for one tenant reach the node on one
// dispatcher lease — one take, no job per ask — and run at once, each
// through a sub-agent of its own: its own Latchkey session, opened by
// Mesh for that ask on its own key, its own sessionproxy socket and its
// own directory. Every product call a sub-agent makes rides its ask's
// session, never the dispatcher's own; each ask is finished answered
// through the lease, which acks it; and the lease parks once it is
// quiet.
func TestTwoAsksOfOneTenantShareOneDispatcherLease(t *testing.T) {
	sess := &askSession{want: 2, all: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.HeartbeatEvery = time.Hour
	var jktMu sync.Mutex
	jkts := map[string]string{}
	g.fake.AskSession = func(lease, ask, jkt string) (*meshapi.SessionCredential, int) {
		jktMu.Lock()
		jkts[ask] = jkt
		jktMu.Unlock()
		return g.latchkey.open(lease+"|ask:"+ask, jkt)
	}
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Kind = "conform/assistant"
		b.Tenant = "acme"
		b.Git, b.Code = nil, nil
		b.TurnTool = "turn"
		b.Thread = &meshapi.Thread{ParkAfterS: 1}
		b.Dispatcher = &meshapi.BriefDispatcher{Serves: []string{"goal-chat"}, Cap: 4, ThreadIdleS: 900}
		b.Guidance = "You are the acme assistant."
		b.RenewEveryS = 1
	})
	// two asks, as POST /v1/dispatch sends them: turns of kind ask
	g.fake.Turn(job, askTurn(1))
	g.fake.Turn(job, askTurn(2))
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	lease := g.fake.LeaseOf(job)
	if takes := len(g.fake.Calls("take")); takes < 1 || lease == "" {
		t.Fatalf("no dispatcher lease was taken (%d takes)", takes)
	}
	for _, a := range []string{"ask-1", "ask-2"} {
		e, ok := g.fake.AskEnded(a)
		if !ok || e.LeaseID != lease || e.End != meshapi.AskEndAnswered {
			t.Fatalf("%s ended %+v (%v), want answered on lease %s", a, e, ok, lease)
		}
	}
	if got := g.fake.Ended(lease); got != "hand-back" {
		t.Fatalf("the quiet dispatcher's lease ended %q, want a parked hand-back", got)
	}
	if seen := g.fake.TurnsSeen(job); seen != 2 {
		t.Fatalf("turns_seen %d, want 2 once both asks finished", seen)
	}

	// one sub-agent per ask, run at once, each its own
	sess.mu.Lock()
	specs := append([]SessionSpec(nil), sess.specs...)
	sess.mu.Unlock()
	if len(specs) != 2 {
		t.Fatalf("%d sub-agents ran, want 2", len(specs))
	}
	socks, dirs := map[string]bool{}, map[string]bool{}
	for _, s := range specs {
		sock, _, _ := harness.ProxiedOf(s.Servers["conform"])
		socks[sock], dirs[s.Workspace] = true, true
		if !strings.HasPrefix(s.Prompt, "You are the acme assistant.\n\n") || !strings.Contains(s.Prompt, "mcp__conform__turn(ask-") {
			t.Fatalf("a sub-agent's prompt does not start with the shared guidance: %q", s.Prompt)
		}
		if s.Mailbox != nil || s.Thread || s.Resume != "" {
			t.Fatalf("a sub-agent is not one-shot: %+v", s)
		}
	}
	if len(socks) != 2 || len(dirs) != 2 {
		t.Fatalf("sub-agents shared a socket or a directory: %v %v", socks, dirs)
	}
	jktMu.Lock()
	if len(jkts) != 2 || jkts["ask-1"] == jkts["ask-2"] {
		t.Fatalf("the asks' sessions were not opened on keys of their own: %v", jkts)
	}
	jktMu.Unlock()
	// every product call rode an ask's own session
	g.product.mu.Lock()
	by := append([]string(nil), g.product.by...)
	g.product.mu.Unlock()
	asks := map[string]int{}
	for _, b := range by {
		if !strings.HasPrefix(b, lease+"|ask:") {
			t.Fatalf("a product call rode %q, not an ask's own session", b)
		}
		asks[b]++
	}
	if len(asks) != 2 {
		t.Fatalf("product calls by session %v, want one session per ask", asks)
	}
}

// TestAnUnfinishedAskComesBackAfterARestart (acceptance check 5, P5):
// a dispatcher's lease ends — the node restarts — while one ask's
// sub-agent still works and another was finished. The unfinished ask is
// never finished nor acked: turns_seen stays below it. The next lease of
// the dispatcher, whose brief starts at acked_through, is brought it
// again by its renewal and answers it; the finished one is never run
// again.
func TestAnUnfinishedAskComesBackAfterARestart(t *testing.T) {
	block := make(chan struct{})
	var mu sync.Mutex
	var ran []string
	sess := sessionFunc(func(ctx context.Context, spec SessionSpec) (SessionResult, error) {
		mu.Lock()
		ran = append(ran, spec.Label)
		mu.Unlock()
		if strings.HasSuffix(spec.Label, "/ask/ask-1") {
			select {
			case <-block:
			case <-ctx.Done():
				return SessionResult{}, ctx.Err()
			}
		}
		return SessionResult{SessionID: "s"}, nil
	})
	g := newRig(t, sess)
	g.rt.HeartbeatEvery = time.Hour
	g.fake.AskSession = func(lease, ask, jkt string) (*meshapi.SessionCredential, int) {
		return g.latchkey.open(lease+"|ask:"+ask, jkt)
	}
	dispatcherBrief := func(through int64) func(b *meshapi.Brief) {
		return func(b *meshapi.Brief) {
			b.Kind = "conform/assistant"
			b.Git, b.Code = nil, nil
			b.TurnTool = "turn"
			b.Thread = &meshapi.Thread{ParkAfterS: 1}
			b.Dispatcher = &meshapi.BriefDispatcher{Cap: 4, AckedThrough: through}
			b.RenewEveryS = 1
		}
	}
	job := g.offer(t, dispatcherBrief(0))
	g.fake.Turn(job, askTurn(1))
	g.fake.Turn(job, askTurn(2))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	waitFor(t, "ask 2 finished", func() bool { _, ok := g.fake.AskEnded("ask-2"); return ok })
	waitFor(t, "a renewal after it", func() bool { return g.fake.TurnsSeen(job) == 0 })
	cancel() // the node stops with ask 1 unanswered
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the runtime did not stop")
	}
	if _, ok := g.fake.AskEnded("ask-1"); ok {
		t.Fatal("an ask the node stopped under was finished")
	}
	if seen := g.fake.TurnsSeen(job); seen != 0 {
		t.Fatalf("turns_seen %d: an unanswered ask was acked", seen)
	}

	// the dispatcher's next lease (Mesh offers the job again: here, the
	// same turns on a new job of the fake) has ask 1 back, not ask 2
	close(block)
	g2 := newRig(t, sess)
	g2.rt.HeartbeatEvery = time.Hour
	g2.fake.AskSession = func(lease, ask, jkt string) (*meshapi.SessionCredential, int) {
		return g2.latchkey.open(lease+"|ask:"+ask, jkt)
	}
	job2 := g2.offer(t, dispatcherBrief(0))
	g2.fake.Turn(job2, askTurn(1))
	g2.fake.Turn(job2, askTurn(2))
	g2.fake.MarkAskEnded("ask-2", meshapi.AskEndAnswered)
	mu.Lock()
	ran = nil
	mu.Unlock()
	if err := run(t, g2, context.Background()); err != nil {
		t.Fatal(err)
	}
	if e, ok := g2.fake.AskEnded("ask-1"); !ok || e.End != meshapi.AskEndAnswered {
		t.Fatalf("ask 1 after the restart: %+v %v", e, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || !strings.HasSuffix(ran[0], "/ask/ask-1") {
		t.Fatalf("after the restart the sub-agents ran %v, want ask 1 alone", ran)
	}
}

// sessionFunc is a Session of a function.
type sessionFunc func(ctx context.Context, spec SessionSpec) (SessionResult, error)

func (f sessionFunc) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	return f(ctx, spec)
}
