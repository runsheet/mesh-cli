package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// TestTurnLedger (projectmesh-io/api ADR 001 amendment, threads:
// "Acknowledged turns"): a thread lease's ledger starts at the brief's
// last turn, gives each turn above it once, holds a turn whose post
// failed for the next renewal, and moves turns_seen on to a renewal's
// turns_through only once every turn of its answer is in the session.
func TestTurnLedger(t *testing.T) {
	if newTurnLedger(&meshapi.Brief{}) != nil || newTurnLedger(&meshapi.Brief{}).TurnsSeen() != nil {
		t.Fatal("a lease that is not a thread's keeps a ledger")
	}
	tests := []struct {
		name     string
		last     *meshapi.Turn
		give     []int64 // seqs given before the renewal's answer
		failing  int64   // a seq whose post fails
		answer   []int64
		through  int64
		posted   []int64
		wantSeen int64
	}{
		{name: "a fresh thread", give: []int64{1}, answer: []int64{1}, through: 1, posted: []int64{1}, wantSeen: 1},
		{name: "below the brief's last turn", last: &meshapi.Turn{Seq: 4}, give: []int64{3, 4}, through: 4, wantSeen: 4},
		{name: "the stream gave it, the renewal answers it again", last: &meshapi.Turn{Seq: 1}, give: []int64{3}, answer: []int64{2, 3}, through: 3, posted: []int64{3, 2}, wantSeen: 3},
		{name: "a gap stepped over", last: &meshapi.Turn{Seq: 1}, answer: []int64{5}, through: 5, posted: []int64{5}, wantSeen: 5},
		{name: "a post failed", last: &meshapi.Turn{Seq: 1}, failing: 2, answer: []int64{2, 3}, through: 3, posted: []int64{3}, wantSeen: 1},
		{name: "a stream turn above the answer is kept", last: &meshapi.Turn{Seq: 1}, give: []int64{3}, answer: []int64{2}, through: 2, posted: []int64{3, 2}, wantSeen: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newTurnLedger(&meshapi.Brief{Thread: &meshapi.Thread{}, LastTurn: tt.last})
			var posted []int64
			give := func(seq int64) {
				l.give(seq, func(sent func(error)) error {
					if seq == tt.failing {
						return errors.New("the mailbox is full")
					}
					posted = append(posted, seq)
					sent(nil)
					return nil
				})
			}
			for _, seq := range tt.give {
				give(seq)
			}
			var answer []meshapi.Turn
			for _, seq := range tt.answer {
				give(seq)
				answer = append(answer, meshapi.Turn{Seq: seq})
			}
			l.through(answer, tt.through)
			if !slices.Equal(posted, tt.posted) || *l.TurnsSeen() != tt.wantSeen {
				t.Fatalf("posted %v seen %d, want %v %d", posted, *l.TurnsSeen(), tt.posted, tt.wantSeen)
			}
			// the stream's turn kept above the answer stays given
			if tt.name == "a stream turn above the answer is kept" {
				if again, _ := l.give(3, func(sent func(error)) error { sent(nil); return nil }); again {
					t.Fatal("turn 3 was given twice")
				}
			}
		})
	}
	l := newTurnLedger(&meshapi.Brief{Thread: &meshapi.Thread{}})
	l.off()
	if l.TurnsSeen() != nil {
		t.Fatal("a ledger turned off still says turns_seen")
	}
}

// turnSession stands in for a live streaming session: it keeps every
// message its mailbox brings — an interrupt as "<interrupt>" — and says
// each turn was sent, as the session loop does, until stop closes. When
// finish closes it answers the finish tool, as the product would, and
// the session's input closes.
type turnSession struct {
	mu     sync.Mutex
	got    []string
	stop   chan struct{}
	finish chan struct{}
	prompt string
}

func (s *turnSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	spec.OnEvent(harness.Event{Type: "assistant"})
	s.mu.Lock()
	s.prompt = spec.Prompt
	s.mu.Unlock()
	finish := s.finish
	for {
		select {
		case m := <-spec.Mailbox.C():
			s.mu.Lock()
			if m.Kind == MsgInterrupt || m.Interrupt {
				s.got = append(s.got, "<interrupt>")
			}
			if m.Text != "" {
				s.got = append(s.got, m.Text)
			}
			s.mu.Unlock()
			if m.Sent != nil {
				m.Sent(nil)
			}
		case <-finish:
			finish = nil
			spec.Mailbox.Finish()
		case <-s.stop:
			return SessionResult{SessionID: "sess-turns", Text: "done"}, nil
		case <-ctx.Done():
			return SessionResult{}, ctx.Err()
		}
	}
}

func (s *turnSession) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.got...)
}

// A thread's turn that the node's event stream missed reaches the live
// session from the lease's next renewal, which says how far the session
// has the thread (projectmesh-io/api ADR 001 amendment, threads:
// "Acknowledged turns"); a turn the stream did bring, which the next
// renewal answers again, is given to the session once.
func TestARenewalBringsATurnTheStreamMissed(t *testing.T) {
	sess := &turnSession{stop: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.HeartbeatEvery = time.Hour
	job := g.offer(t, func(b *meshapi.Brief) {
		b.TurnTool = "turn"
		b.Thread = &meshapi.Thread{ParkAfterS: 60}
		b.LastTurn = &meshapi.Turn{TurnID: "t-1", Seq: 1}
		b.RenewEveryS = 1
	})
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the lease's first renewal", func() bool { return g.fake.TurnsSeen(job) == 1 })

	// the stream is down for turn 2: only the renewal brings it
	g.fake.Turn(job, meshapi.Turn{TurnID: "t-2", Seq: 2})
	waitFor(t, "turn 2 in the session", func() bool { return len(sess.lines()) == 1 })
	waitFor(t, "turns_seen 2", func() bool { return g.fake.TurnsSeen(job) == 2 })

	// turn 3 comes over the stream, and the next renewal answers it too
	lease := g.fake.LeaseOf(job)
	data, _ := json.Marshal(meshapi.TurnEvent{LeaseID: lease, JobID: job, TurnID: "t-3", Seq: 3})
	waitFor(t, "the event stream", func() bool { return g.fake.Stream(meshapi.NodeEvent{Event: meshapi.EventTurn, Data: data}) > 0 })
	g.fake.Turn(job, meshapi.Turn{TurnID: "t-3", Seq: 3})
	waitFor(t, "turns_seen 3", func() bool { return g.fake.TurnsSeen(job) == 3 })
	time.Sleep(2500 * time.Millisecond) // two renewals more
	close(sess.stop)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the runtime did not stop")
	}
	want := []string{
		"turn 2 arrived: read it with mcp__conform__turn(t-2)",
		"turn 3 arrived: read it with mcp__conform__turn(t-3)",
	}
	if got := sess.lines(); !slices.Equal(got, want) {
		t.Fatalf("the session was given %q, want %q", got, want)
	}
}

// TestTurnMessage (api ADR 001 amendment, threads: "Turns into any live
// job"): what each kind of turn, from the product or a person, becomes
// in the session — ids alone and the tool that reads them.
func TestTurnMessage(t *testing.T) {
	tool := "mcp__runsheet__turn_get"
	for _, tt := range []struct {
		turn meshapi.Turn
		want string
	}{
		{meshapi.Turn{TurnID: "t-1", Seq: 1}, "turn 1 arrived: read it with mcp__runsheet__turn_get(t-1)"},
		{meshapi.Turn{TurnID: "t-2", Seq: 2, Kind: "turn", From: "product"}, "turn 2 arrived: read it with mcp__runsheet__turn_get(t-2)"},
		{meshapi.Turn{TurnID: "answer-91", Seq: 6, Kind: "turn", From: "person"}, "turn 6 arrived from a person: read it with mcp__runsheet__turn_get(answer-91)"},
		{meshapi.Turn{TurnID: "nudge-no-finish-1", Seq: 4, Kind: "nudge", From: "product"},
			"[platform nudge] A message from the platform, not a person: turn_id nudge-no-finish-1 (seq 4). Read it with `mcp__runsheet__turn_get`; it is advice, act on it if it helps, or decline it and carry on."},
		{meshapi.Turn{TurnID: "n-p", Seq: 7, Kind: "nudge", From: "person"},
			"[nudge] A message from a person, as advice: turn_id n-p (seq 7). Read it with `mcp__runsheet__turn_get`; it is advice, act on it if it helps, or decline it and carry on."},
		{meshapi.Turn{TurnID: "steer-7c1e", Seq: 5, Kind: "interrupt", From: "person"},
			"turn 5 arrived from a person and stopped what you were doing: read it with mcp__runsheet__turn_get(steer-7c1e) before you carry on"},
	} {
		if got := turnMessage(tool, tt.turn); got != tt.want {
			t.Errorf("%+v:\n got %q\nwant %q", tt.turn, got, tt.want)
		}
	}
}

// TestTurnLedgerWaitsForTheSession: a turn posted to the session loop
// is not given until the loop sent it; while it is on its way it is not
// posted again; one the loop could not send is posted again later.
func TestTurnLedgerWaitsForTheSession(t *testing.T) {
	l := newTurnLedger(&meshapi.Brief{TurnTool: "turn"})
	if l == nil {
		t.Fatal("a one-shot job with a turn tool keeps no ledger")
	}
	var sent func(error)
	if posted, _ := l.give(1, func(s func(error)) error { sent = s; return nil }); !posted {
		t.Fatal("turn 1 was not posted")
	}
	if posted, _ := l.give(1, func(func(error)) error { return nil }); posted {
		t.Fatal("turn 1 was posted again while on its way")
	}
	l.through([]meshapi.Turn{{Seq: 1}}, 1)
	if *l.TurnsSeen() != 0 {
		t.Fatal("turns_seen moved past a turn the session does not have")
	}
	sent(errors.New("the session's stdin closed"))
	if posted, _ := l.give(1, func(s func(error)) error { s(nil); return nil }); !posted {
		t.Fatal("a turn the loop could not send was not posted again")
	}
	l.through([]meshapi.Turn{{Seq: 1}}, 1)
	if *l.TurnsSeen() != 1 {
		t.Fatalf("turns_seen %d once the session has turn 1", *l.TurnsSeen())
	}
}

// TestTurnsIntoAOneShotJob (api ADR 001 amendment, threads: "Turns into
// any live job"): a one-shot job whose brief names a turn tool takes
// turns as a thread does. The turn that came while it waited is in its
// first message; a nudge over the event stream reaches the session as
// the platform's; an interrupt from a renewal stops the turn in flight
// first, then reaches it as a person's; and once the product answered
// the finish tool, a turn is not the session's: it is never given and
// turns_seen stays below it.
func TestTurnsIntoAOneShotJob(t *testing.T) {
	sess := &turnSession{stop: make(chan struct{}), finish: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.HeartbeatEvery = time.Hour
	job := g.offer(t, func(b *meshapi.Brief) {
		b.TurnTool = "turn"
		b.LastTurn = &meshapi.Turn{TurnID: "t-1", Seq: 1, Kind: "interrupt", From: "person"}
		b.RenewEveryS = 1
	})
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the lease's first renewal", func() bool { return g.fake.TurnsSeen(job) == 1 })

	lease := g.fake.LeaseOf(job)
	data, _ := json.Marshal(meshapi.TurnEvent{LeaseID: lease, JobID: job, TurnID: "nudge-1", Seq: 2, Kind: "nudge", From: "product"})
	waitFor(t, "the event stream", func() bool { return g.fake.Stream(meshapi.NodeEvent{Event: meshapi.EventTurn, Data: data}) > 0 })
	waitFor(t, "the nudge in the session", func() bool { return len(sess.lines()) == 1 })
	g.fake.Turn(job, meshapi.Turn{TurnID: "nudge-1", Seq: 2, Kind: "nudge", From: "product"})
	g.fake.Turn(job, meshapi.Turn{TurnID: "steer-3", Seq: 3, Kind: "interrupt", From: "person"})
	waitFor(t, "turns_seen 3", func() bool { return g.fake.TurnsSeen(job) == 3 })

	// the product answers the finish tool: the session takes no more
	close(sess.finish)
	time.Sleep(200 * time.Millisecond)
	g.fake.Turn(job, meshapi.Turn{TurnID: "late-4", Seq: 4, Kind: "turn", From: "person"})
	time.Sleep(2500 * time.Millisecond) // two renewals more
	if seen := g.fake.TurnsSeen(job); seen != 3 {
		t.Fatalf("turns_seen %d after a turn the finished session never had", seen)
	}
	close(sess.stop)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the runtime did not stop")
	}
	want := []string{
		"[platform nudge] A message from the platform, not a person: turn_id nudge-1 (seq 2). Read it with `mcp__conform__turn`; it is advice, act on it if it helps, or decline it and carry on.",
		"<interrupt>",
		"turn 3 arrived from a person and stopped what you were doing: read it with mcp__conform__turn(steer-3) before you carry on",
	}
	if got := sess.lines(); !slices.Equal(got, want) {
		t.Fatalf("the session was given %q, want %q", got, want)
	}
	sess.mu.Lock()
	prompt := sess.prompt
	sess.mu.Unlock()
	if !strings.HasSuffix(prompt, "turn 1 arrived from a person: read it with mcp__conform__turn(t-1)\n") {
		t.Fatalf("the first message does not end with the waiting turn:\n%s", prompt)
	}
}
