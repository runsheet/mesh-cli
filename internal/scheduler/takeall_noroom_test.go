package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// waitInLine waits until n all-or-nothing takes are in line.
func waitInLine(t *testing.T, s *Scheduler, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		got := len(s.allWaiting)
		s.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d takes in line, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Goal a-validation-needing-no-room-is-admitted-at-once, acceptance
// checks 1 and 2: the node holds 86 of 88 cpus and an older validation
// waits for 12; a lint whose one lane asks nothing beyond its lease's
// own hold is granted at once, while an ask that does need room (6 cpus,
// 2 free) still waits behind the older one.
func TestAZeroRoomTakeIsGrantedAheadOfAWaiter(t *testing.T) {
	s := New(Capacity{CPUs: 88, MemGB: 400})
	held, err := s.TryTakeAll([]Ask{{Owner: "running", CPUs: 86}})
	if err != nil {
		t.Fatal(err)
	}
	older := make(chan error, 1)
	go func() {
		_, err := s.TakeAll(context.Background(), []Ask{{Owner: "older/lane-checks", CPUs: 12}})
		older <- err
	}()
	waitInLine(t, s, 1)

	lint, err := s.TryTakeAll([]Ask{{Owner: "lint/lane-markdown", Cache: CacheShared}})
	if err != nil {
		t.Fatalf("a zero-room lane waited behind the older one: %v", err)
	}
	if _, err := s.TryTakeAll([]Ask{{Owner: "six/lane-unit", CPUs: 6}}); !errors.Is(err, ErrNoRoom) || !strings.Contains(err.Error(), "an older validation waits") {
		t.Fatalf("a 6-cpu ask with 2 free: err %v, want it behind the older one", err)
	}
	select {
	case err := <-older:
		t.Fatalf("the older take was granted with 2 cpus free: %v", err)
	default:
	}
	lint[0].Release()
	held[0].Release()
	select {
	case err := <-older:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the older take was not granted once room freed")
	}
}

// dl360, 2026-11-25 18:50–21:45 AEDT: every lease on the node held its
// own hold while its lanes waited for room only those holds filled, and
// nothing was admitted for three hours. A lease waiting for its lanes
// yields its own hold: the oldest waiter is granted its lanes from the
// yielded room and takes its own hold back with them; the rest wait,
// and no new single ask, nor Free, sees the yielded room as free.
func TestWaitingLeasesYieldTheirOwnHolds(t *testing.T) {
	s := New(Capacity{CPUs: 32, MemGB: 128})
	owns := make([]*Lease, 8)
	for i := range owns {
		l, err := s.TryTake(Ask{Owner: string(rune('a' + i)), CPUs: 4, MemGB: 8, Slot: true})
		if err != nil {
			t.Fatal(err)
		}
		owns[i] = l
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type res struct {
		i   int
		ls  []*Lease
		err error
	}
	got := make(chan res, len(owns))
	for i, own := range owns {
		go func(i int, own *Lease) {
			ls, err := s.TakeAllYielding(ctx, []Ask{{Owner: own.Owner() + "/lane-1", CPUs: 4}, {Owner: own.Owner() + "/lane-2", CPUs: 8}}, own)
			got <- res{i, ls, err}
		}(i, own)
		// in line or granted before the next starts, so the order is i's
		deadline := time.Now().Add(2 * time.Second)
		for {
			s.mu.Lock()
			n := len(s.allWaiting)
			s.mu.Unlock()
			if n+len(got) == i+1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("take %d neither waited nor was granted", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	first := <-got
	if first.err != nil {
		t.Fatalf("no waiting lease was admitted: every own hold still blocked every lane: %v", first.err)
	}
	if first.i != 0 {
		t.Fatalf("lease %d was admitted first, want the oldest (0)", first.i)
	}
	if owns[0].yielded {
		t.Fatal("the admitted lease did not take its own hold back")
	}
	if f := s.Free(); f.CPUs != 0 {
		t.Fatalf("Free = %g cpus with every lease waiting or running, want none: a yielded hold is still held to everyone else", f.CPUs)
	}
	if _, err := s.TryTake(Ask{Owner: "new-lease", CPUs: 1}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a new lease's hold was granted yielded room: %v", err)
	}
	cancel()
	for i := 1; i < len(owns); i++ {
		<-got
	}
	for _, l := range owns {
		if l.yielded {
			t.Fatal("a wait that ended left its own hold yielded")
		}
	}
}
