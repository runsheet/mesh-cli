package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestReserveAdmitsAnAssistantOnAFullNode (goal
// one-assistant-dispatcher-per-org-answers-every-ask P1): a node whose
// general room is all held, with a validation waiting for more, grants
// an assistant's ask at once in its reserve; a validation's ask never
// fits the reserve; the reserve's room comes back when the assistant
// ends; and once the reserve is full an assistant's ask overflows into
// the general room like any other.
func TestReserveAdmitsAnAssistantOnAFullNode(t *testing.T) {
	s := New(Capacity{CPUs: 88, MemGB: 200})
	s.SetReserve(Capacity{CPUs: 4, MemGB: 8})

	// validations hold the whole general room
	if _, err := s.TryTake(Ask{Owner: "validate-1", CPUs: 84, MemGB: 100, Cache: CacheShared}); err != nil {
		t.Fatalf("validation in the general room: %v", err)
	}
	if f := s.Free(); f.CPUs != 0 {
		t.Fatalf("Free = %g cpus, want 0: the reserve is never reported free to other kinds", f.CPUs)
	}
	if f := s.ReserveFree(); f.CPUs != 4 || f.MemGB != 8 {
		t.Fatalf("ReserveFree = %+v, want 4 cpus, 8 GB", f)
	}

	// a validation's lanes wait for room; they do not get the reserve
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, err := s.TakeAll(ctx, []Ask{{Owner: "validate-2/lane-a", CPUs: 2, MemGB: 4}, {Owner: "validate-2/lane-b", CPUs: 2, MemGB: 4}})
		waiting <- err
	}()
	if _, err := s.TryTake(Ask{Owner: "validate-3", CPUs: 2, MemGB: 4, Cache: CacheShared}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a validation took the reserve: err %v", err)
	}

	// the assistant is admitted at once, in the reserve
	a, err := s.TryTake(Ask{Owner: "goal-chat-1", CPUs: 2, MemGB: 4, Account: "acct", Reserved: true})
	if err != nil {
		t.Fatalf("assistant on a full node: %v", err)
	}
	b, err := s.TryTake(Ask{Owner: "goal-chat-2", CPUs: 2, MemGB: 4, Reserved: true})
	if err != nil {
		t.Fatalf("second assistant on a full node: %v", err)
	}
	if f := s.ReserveFree(); f.CPUs != 0 || f.MemGB != 0 {
		t.Fatalf("ReserveFree = %+v, want none left", f)
	}
	if _, err := s.TryTake(Ask{Owner: "goal-chat-3", CPUs: 2, MemGB: 4, Reserved: true}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a third assistant past the reserve on a full node: err %v, want no room", err)
	}
	if err := <-waiting; err == nil {
		t.Fatal("the waiting validation was granted room it never had")
	}

	// the reserve's room comes back as the assistant ends
	a.Release()
	if f := s.ReserveFree(); f.CPUs != 2 {
		t.Fatalf("ReserveFree after a release = %g cpus, want 2", f.CPUs)
	}
	b.Release()

	// with the general room free, a full reserve overflows into it
	s2 := New(Capacity{CPUs: 16, MemGB: 32})
	s2.SetReserve(Capacity{CPUs: 4, MemGB: 8})
	for i := 0; i < 3; i++ {
		if _, err := s2.TryTake(Ask{Owner: "goal-chat", CPUs: 2, MemGB: 4, Reserved: true}); err != nil {
			t.Fatalf("assistant %d: %v", i, err)
		}
	}
	if f := s2.Free(); f.CPUs != 10 {
		t.Fatalf("Free = %g cpus, want 10 (12 general, 2 taken by the overflowing assistant)", f.CPUs)
	}
}

// TestReserveBoundedToAQuarter: a reserve bigger than a quarter of the
// node is cut to it, and a node with no reserve schedules as before.
func TestReserveBoundedToAQuarter(t *testing.T) {
	s := New(Capacity{CPUs: 8, MemGB: 16})
	s.SetReserve(Capacity{CPUs: 4, MemGB: 8})
	if r := s.Reserve(); r.CPUs != 2 || r.MemGB != 4 {
		t.Fatalf("Reserve = %+v, want 2 cpus, 4 GB (a quarter)", r)
	}
	if f := s.Free(); f.CPUs != 6 || f.MemGB != 12 {
		t.Fatalf("Free = %+v, want 6 cpus, 12 GB", f)
	}
	n := New(Capacity{CPUs: 8, MemGB: 16})
	if _, err := n.TryTake(Ask{Owner: "v", CPUs: 8, MemGB: 16}); err != nil {
		t.Fatalf("no reserve: %v", err)
	}
	if _, err := n.TryTake(Ask{Owner: "chat", CPUs: 1, Reserved: true}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("no reserve, a full node: err %v, want no room", err)
	}
}
