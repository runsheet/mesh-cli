package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Goal a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
// P2: inside the validation reserve a person's validation draws its
// person share, which no automated one may use; a repository holding
// none of the share goes ahead of an older waiter from a repository
// that holds some; and while a person's validation waits, no automated
// all-or-nothing take is granted ahead of it.
func TestPersonValidationsTakeTurnsByRepo(t *testing.T) {
	s := New(Capacity{CPUs: 16, MemGB: 64})
	s.SetNamedReserve("validations", Capacity{CPUs: 12, MemGB: 48}, Capacity{CPUs: 8, MemGB: 32})
	if _, err := s.TryTake(Ask{Owner: "build", CPUs: 4, Slot: true}); err != nil {
		t.Fatal(err)
	}
	auto, err := s.TryTake(Ask{Owner: "item", CPUs: 4, Reserve: "validations", Repo: "a"})
	if err != nil {
		t.Fatalf("an automated validation in the reserve's rest: %v", err)
	}
	if _, err := s.TryTake(Ask{Owner: "item-2", CPUs: 1, Reserve: "validations", Repo: "a"}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("an automated validation took the person share: err %v", err)
	}
	if _, err := s.TryTake(Ask{Owner: "pr-a", CPUs: 4, Reserve: "validations", Person: true, Repo: "a"}); err != nil {
		t.Fatalf("repo a's person validation: %v", err)
	}

	// repo a's second waits for 8 cpus; repo b's, younger, is admitted
	// into what is left of the share ahead of it
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	second := make(chan error, 1)
	go func() {
		_, err := s.TakeAll(ctx, []Ask{{Owner: "pr-a2/lane-1", CPUs: 8, Reserve: "validations", Person: true, Repo: "a"}})
		second <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(s.Waiting()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if w := s.Waiting(); len(w) != 1 || w[0].Reserve != "validations" || !w[0].Person || !strings.Contains(w[0].Room(), "person share") {
		t.Fatalf("waiting %+v, want repo a's second naming the person share", w)
	}
	if _, err := s.TryTakeAll([]Ask{{Owner: "pr-b/lane-1", CPUs: 4, Reserve: "validations", Person: true, Repo: "b"}}); err != nil {
		t.Fatalf("repo b's first person validation behind repo a's second: %v", err)
	}

	// room frees in the rest: an automated take still waits behind the
	// person's
	auto.Release()
	_, err = s.TryTakeAll([]Ask{{Owner: "item-3/lane-1", CPUs: 4, Reserve: "validations", Repo: "a"}})
	if !errors.Is(err, ErrNoRoom) || !strings.Contains(err.Error(), "a person's validation waits") {
		t.Fatalf("an automated take ahead of a waiting person's: err %v", err)
	}
	cancel()
	<-second
}

// Reserves together keep at most three quarters of the node, the
// assistant's at most a quarter; a validation's ask beyond its reserve
// draws the general room.
func TestReservesAreBounded(t *testing.T) {
	s := New(Capacity{CPUs: 64, MemGB: 200})
	s.SetReserve(Capacity{CPUs: 4, MemGB: 8})
	s.SetNamedReserve("validations", Capacity{CPUs: 60, MemGB: 190}, Capacity{})
	if r := s.ReserveOf("validations"); r.CPUs != 44 || r.MemGB != 142 {
		t.Fatalf("the validation reserve is %+v, want 44 cpus, 142 GB: three quarters less the assistant's", r)
	}
	if f := s.Free(); f.CPUs != 16 || f.MemGB != 50 {
		t.Fatalf("Free = %+v, want a quarter of the node", f)
	}
	l, err := s.TryTake(Ask{Owner: "v", CPUs: 50, MemGB: 150, Reserve: "validations"})
	if err != nil {
		t.Fatalf("a validation past its reserve: %v", err)
	}
	if f := s.Free(); f.CPUs != 10 || f.MemGB != 42 {
		t.Fatalf("Free = %+v after a validation of 50 cpus, 150 GB: want 6 cpus, 8 GB of the general room drawn", f)
	}
	l.Release()
	if f := s.ReserveFreeOf("validations"); f.CPUs != 44 {
		t.Fatalf("the reserve back: %+v", f)
	}
}
