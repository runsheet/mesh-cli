package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
				l.give(seq, func() error {
					if seq == tt.failing {
						return errors.New("the mailbox is full")
					}
					posted = append(posted, seq)
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
				if again, _ := l.give(3, func() error { return nil }); again {
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

// turnSession stands in for a thread's streaming session: it keeps
// every message its mailbox brings until stop closes.
type turnSession struct {
	mu   sync.Mutex
	got  []string
	stop chan struct{}
}

func (s *turnSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	spec.OnEvent(harness.Event{Type: "assistant"})
	for {
		select {
		case m := <-spec.Mailbox.C():
			s.mu.Lock()
			s.got = append(s.got, m.Text)
			s.mu.Unlock()
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
