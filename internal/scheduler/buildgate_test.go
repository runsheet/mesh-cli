package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A Gate ask waiting for room is not starved by Build asks claiming the
// general room as it frees piece by piece: a new Build ask is refused
// until the room has, all at once, enough free for the waiting Gate
// ask, which is then admitted ahead of any further build.
func TestANewBuildIsRefusedBehindAWaitingGateValidation(t *testing.T) {
	s, _ := newTest(Capacity{CPUs: 16})
	var builds []*Lease
	for i := range 4 {
		l, err := s.TryTake(Ask{Owner: fmt.Sprintf("build-%d", i), CPUs: 4, Build: true})
		if err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		builds = append(builds, l)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gate := make(chan error, 1)
	go func() {
		_, err := s.Take(ctx, Ask{Owner: "gate", CPUs: 16, Gate: true})
		gate <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(s.Waiting()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(s.Waiting()) == 0 {
		t.Fatal("the gate validation never waited")
	}

	// two builds release: 8 of the 16 cpus free, short of the gate's need
	builds[0].Release()
	builds[1].Release()
	if _, err := s.TryTake(Ask{Owner: "build-new", CPUs: 4, Build: true}); !errors.Is(err, ErrNoRoom) || !strings.Contains(err.Error(), "behind gate validation") {
		t.Fatalf("a new build ahead of the waiting gate: %v", err)
	}

	// two more release: all 16 cpus free at once, for the gate alone
	builds[2].Release()
	builds[3].Release()
	select {
	case err := <-gate:
		if err != nil {
			t.Fatalf("the gate validation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the gate validation was not admitted once the room was free at once")
	}
}

// A Build ask held behind a waiting Gate ask for longer than
// Scheduler.BuildAging is admitted anyway, even though the Gate ask
// still waits, and Logf hears the aging.
func TestABuildPastItsAgingIsAdmittedAheadOfAWaitingGateValidation(t *testing.T) {
	var logged []string
	s, c := newTest(Capacity{CPUs: 16})
	s.BuildAging = 20 * time.Minute
	s.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }

	if _, err := s.TryTake(Ask{Owner: "build-0", CPUs: 4, Build: true, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gate := make(chan error, 1)
	go func() {
		_, err := s.Take(ctx, Ask{Owner: "gate", CPUs: 16, Gate: true})
		gate <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(s.Waiting()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(s.Waiting()) == 0 {
		t.Fatal("the gate validation never waited")
	}

	if _, err := s.TryTake(Ask{Owner: "build-new", CPUs: 2, Build: true}); !errors.Is(err, ErrNoRoom) || !strings.Contains(err.Error(), "behind gate validation") {
		t.Fatalf("a new build ahead of the waiting gate: %v", err)
	}

	c.add(21 * time.Minute)
	if _, err := s.TryTake(Ask{Owner: "build-new", CPUs: 2, Build: true}); err != nil {
		t.Fatalf("a build past its aging: %v", err)
	}

	found := false
	for _, line := range logged {
		if strings.Contains(line, "aging") {
			found = true
		}
	}
	if !found {
		t.Fatalf("logged %v, want a line naming the aging", logged)
	}

	cancel()
	<-gate
}
