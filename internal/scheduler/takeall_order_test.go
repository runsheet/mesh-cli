package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A validation waiting for many lanes' room is not passed over by a
// younger one asking for fewer: room freed goes to the oldest waiter
// first (dl360, 2026-11-22).
func TestTakeAllAdmitsTheOldestWaiterFirst(t *testing.T) {
	s := New(Capacity{CPUs: 88, MemGB: 400})
	held, err := s.TryTakeAll([]Ask{{Owner: "running", CPUs: 54}})
	if err != nil {
		t.Fatal(err)
	}
	big := make(chan error, 1)
	go func() {
		_, err := s.TakeAll(context.Background(), []Ask{{Owner: "big", CPUs: 64}})
		big <- err
	}()
	// the big take is in line
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.allWaiting)
		s.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the big take never waited")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 34 cpus are free: a younger 24-cpu take would fit, but waits
	if _, err := s.TryTakeAll([]Ask{{Owner: "small", CPUs: 24}}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a younger take was granted ahead of the waiting one: %v", err)
	}
	small := make(chan error, 1)
	go func() {
		_, err := s.TakeAll(context.Background(), []Ask{{Owner: "small", CPUs: 24}})
		small <- err
	}()
	select {
	case err := <-small:
		t.Fatalf("the younger take jumped the line: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	held[0].Release()
	select {
	case err := <-big:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the oldest waiter was not admitted on the release")
	}
	select {
	case err := <-small:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the younger take was not admitted once the older one was")
	}
}

// A take that needs no extra room is admitted at once even behind an
// older waiter: it takes nothing the waiter is waiting for (dl360/
// mesh-specs#227). An ask that does need room, though, still waits
// behind the older one: ordering among room-needing asks is unchanged.
func TestAZeroRoomTakeIsAdmittedAheadOfAnOlderWaiter(t *testing.T) {
	s := New(Capacity{CPUs: 88, MemGB: 400})
	if _, err := s.TryTakeAll([]Ask{{Owner: "running", CPUs: 86}}); err != nil {
		t.Fatal(err)
	}
	older := make(chan error, 1)
	go func() {
		_, err := s.TakeAll(context.Background(), []Ask{{Owner: "older", CPUs: 12}})
		older <- err
	}()
	// the older take is in line
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.allWaiting)
		s.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the older take never waited")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// a take asking for no room at all is admitted at once, the older
	// waiter still waiting
	zero, err := s.TryTakeAll([]Ask{{CPUs: 0, MemGB: 0}})
	if err != nil {
		t.Fatalf("a zero-room take was refused: %v", err)
	}
	select {
	case err := <-older:
		t.Fatalf("the older take was admitted by the zero-room take's grant: %v", err)
	default:
	}
	zero[0].Release()

	// an ask that does need room still waits behind the older one: only
	// 2 cpus are free, and it asks for 6
	if _, err := s.TryTakeAll([]Ask{{CPUs: 6}}); !errors.Is(err, ErrNoRoom) || !strings.Contains(err.Error(), "an older validation waits for its lanes' room first") {
		t.Fatalf("a room-needing take jumped the line: %v", err)
	}
}

// A waiter that gives up leaves the line, so it holds no one up.
func TestTakeAllWaiterThatGivesUpLeavesTheLine(t *testing.T) {
	s := New(Capacity{CPUs: 8})
	if _, err := s.TryTakeAll([]Ask{{Owner: "running", CPUs: 6}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.TakeAll(ctx, []Ask{{Owner: "big", CPUs: 8}}); err == nil {
		t.Fatal("the big take fit")
	}
	if _, err := s.TryTakeAll([]Ask{{Owner: "small", CPUs: 2}}); err != nil {
		t.Fatalf("a gone waiter still holds the line: %v", err)
	}
}
