package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a test's settable time.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTest(total Capacity) (*Scheduler, *clock) {
	c := &clock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	s := New(total)
	s.Now = c.now
	return s, c
}

// Each ask fits or not beside what the ledger already holds, and the
// refusal names why (ADR-044: the scheduler alone owns capacity).
func TestFit(t *testing.T) {
	total := Capacity{CPUs: 8, MemGB: 16, Slots: 2, Accounts: map[string]int{"acct-a": 1, "acct-b": 0}, AccountSlots: 2}
	for _, tc := range []struct {
		name string
		held []Ask
		ask  Ask
		want string // "" fits; else a part of the refusal
	}{
		{"empty node", nil, Ask{CPUs: 4, Slot: true}, ""},
		{"cpus left", []Ask{{CPUs: 4}}, Ask{CPUs: 4}, ""},
		{"cpus short", []Ask{{CPUs: 6}}, Ask{CPUs: 4}, "4 cpus asked, 2 of 8 free"},
		{"bigger than the node runs alone", nil, Ask{CPUs: 12}, ""},
		{"bigger than the node waits for others", []Ask{{CPUs: 1}}, Ask{CPUs: 12}, "12 cpus asked"},
		{"memory short", []Ask{{MemGB: 12}}, Ask{MemGB: 8}, "8 GB asked, 4 of 16 GB free"},
		{"slots full", []Ask{{Slot: true}, {Slot: true}}, Ask{Slot: true}, "all 2 job slots held"},
		{"a resource actor takes no slot", []Ask{{Slot: true}, {Slot: true}}, Ask{CPUs: 1}, ""},
		{"account full", []Ask{{Account: "acct-a"}}, Ask{Account: "acct-a"}, "account acct-a has all 1 sessions held"},
		{"an unnamed account is unbounded", []Ask{{Account: "acct-b"}}, Ask{Account: "acct-b"}, ""},
		{"an unnamed account has AccountSlots", []Ask{{Account: "acct-c"}, {Account: "acct-c"}}, Ask{Account: "acct-c"}, "account acct-c has all 2 sessions held"},
		{"shared beside shared", []Ask{{Cache: CacheShared}}, Ask{Cache: CacheShared}, ""},
		{"exclusive while shared", []Ask{{Cache: CacheShared}}, Ask{Cache: CacheExclusive}, "1 leases build with the cache"},
		{"shared while exclusive", []Ask{{Cache: CacheExclusive}}, Ask{Cache: CacheShared}, "held exclusively"},
		{"exclusive on an idle cache", []Ask{{CPUs: 2}}, Ask{Cache: CacheExclusive}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTest(total)
			for _, h := range tc.held {
				if _, err := s.TryTake(h); err != nil {
					t.Fatalf("holding %+v: %v", h, err)
				}
			}
			_, err := s.TryTake(tc.ask)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("TryTake(%+v) = %v, want it to fit", tc.ask, err)
			case tc.want != "" && err == nil:
				t.Fatalf("TryTake(%+v) fit, want %q", tc.ask, tc.want)
			case tc.want != "" && (!errors.Is(err, ErrNoRoom) || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("TryTake(%+v) = %v, want ErrNoRoom naming %q", tc.ask, err, tc.want)
			}
		})
	}
}

// Free is the total less what the live leases hold: the node's `free`
// on a take.
func TestFree(t *testing.T) {
	s, _ := newTest(Capacity{CPUs: 8, MemGB: 16, Slots: 3, Accounts: map[string]int{"acct-a": 2}})
	a, _ := s.TryTake(Ask{Owner: "lease-1", CPUs: 2.5, MemGB: 4, Slot: true, Account: "acct-a"})
	if _, err := s.TryTake(Ask{Owner: "trim", CPUs: 1}); err != nil {
		t.Fatal(err)
	}
	f := s.Free()
	if f.CPUs != 4.5 || f.MemGB != 12 || f.Slots != 2 || f.Accounts["acct-a"] != 1 {
		t.Fatalf("Free = %+v", f)
	}
	a.Release()
	a.Release() // twice is nothing
	if f := s.Free(); f.CPUs != 7 || f.Slots != 3 || f.Accounts["acct-a"] != 2 {
		t.Fatalf("Free after release = %+v", f)
	}
}

// A holder that stops renewing gives its capacity back once its TTL
// passes; one that renews keeps it.
func TestLeasesLapseUnrenewed(t *testing.T) {
	var logged []string
	s, c := newTest(Capacity{CPUs: 4, Slots: 2})
	s.Logf = func(f string, a ...any) { logged = append(logged, f) }
	kept, _ := s.TryTake(Ask{Owner: "lease-kept", CPUs: 1, Slot: true, TTL: time.Minute})
	lost, _ := s.TryTake(Ask{Owner: "lease-lost", CPUs: 2, Slot: true, TTL: time.Minute})
	c.add(40 * time.Second)
	if !kept.Renew() {
		t.Fatal("a live lease would not renew")
	}
	c.add(40 * time.Second)
	if f := s.Free(); f.CPUs != 3 || f.Slots != 1 {
		t.Fatalf("Free = %+v, want lease-lost's capacity back", f)
	}
	if lost.Renew() || lost.Live() {
		t.Fatal("a lapsed lease renewed")
	}
	if !kept.Live() {
		t.Fatal("the renewed lease lapsed")
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d lapses, want 1", len(logged))
	}
	held := s.Leases()
	if len(held) != 1 || held[0].Owner != "lease-kept" {
		t.Fatalf("Leases = %+v", held)
	}
}

// Take waits for room: a release wakes it.
func TestTakeWaitsForARelease(t *testing.T) {
	s := New(Capacity{CPUs: 4, Slots: 1})
	first, _ := s.TryTake(Ask{Owner: "lease-1", Slot: true})
	got := make(chan error, 1)
	go func() {
		_, err := s.Take(context.Background(), Ask{Owner: "lease-2", Slot: true})
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("Take did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	first.Release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Take did not wake on the release")
	}
}

// Take gives up with its ctx, naming why the ask did not fit.
func TestTakeEndsWithItsContext(t *testing.T) {
	s := New(Capacity{Slots: 1})
	if _, err := s.TryTake(Ask{Slot: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := s.Take(ctx, Ask{Slot: true})
	if !errors.Is(err, ErrNoRoom) || !strings.Contains(err.Error(), "job slots") {
		t.Fatalf("Take = %v", err)
	}
}

// An exclusive ask that waits holds new shared asks back, so a trim is
// not starved by a stream of jobs; it gets the cache once the builds
// drain, and the shared asks go on after it.
func TestExclusiveCacheIsNotStarved(t *testing.T) {
	s := New(Capacity{})
	build, _ := s.TryTake(Ask{Owner: "lease-1", Cache: CacheShared})
	got := make(chan *Lease, 1)
	go func() {
		l, err := s.Take(context.Background(), Ask{Owner: "trim", Cache: CacheExclusive})
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		waiting := s.exclusiveWaiting
		s.mu.Unlock()
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the trim never waited")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := s.TryTake(Ask{Owner: "lease-2", Cache: CacheShared}); err == nil || !strings.Contains(err.Error(), "a trim waits") {
		t.Fatalf("a shared ask while the trim waited: %v", err)
	}
	build.Release()
	var trim *Lease
	select {
	case trim = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the trim never got the cache")
	}
	if _, err := s.TryTake(Ask{Cache: CacheShared}); err == nil {
		t.Fatal("a shared lease was granted while the trim held the cache")
	}
	trim.Release()
	if _, err := s.TryTake(Ask{Cache: CacheShared}); err != nil {
		t.Fatalf("after the trim: %v", err)
	}
}

// An exclusive ask that gives up lets shared asks through again.
func TestExclusiveWaitEnds(t *testing.T) {
	s := New(Capacity{})
	if _, err := s.TryTake(Ask{Cache: CacheShared}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.Take(ctx, Ask{Cache: CacheExclusive}); err == nil {
		t.Fatal("the trim got the cache past a build")
	}
	if _, err := s.TryTake(Ask{Cache: CacheShared}); err != nil {
		t.Fatalf("after the trim gave up: %v", err)
	}
}
