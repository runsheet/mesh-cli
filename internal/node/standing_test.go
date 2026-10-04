package node

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// WriteStanding then Standing round-trips two AccountStanding records;
// an empty WriteStanding removes the file, and Stopped removes it too,
// as it does node.scaling (goal
// every-subscription-lands-full-at-its-reset, item 6).
func TestRunStateStanding(t *testing.T) {
	s := RunState{Dir: t.TempDir()}
	want := []AccountStanding{
		{Label: "max-a", Kind: AccountSubscription, SevenDay: WindowStanding{Utilization: 0.10, Target: 0.45, Distance: -0.35}, Distance: -0.35},
		{Label: "max-b", Kind: AccountSubscription, SevenDay: WindowStanding{Utilization: 0.70, Target: 0.45, Distance: 0.25}, Distance: 0.25, Push: true},
	}
	if err := s.WriteStanding(want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Standing()
	if !ok {
		t.Fatal("Standing: not ok")
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Standing = %+v, want %+v", got, want)
	}
	if err := s.WriteStanding(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Standing(); ok {
		t.Fatal("an empty WriteStanding left the file behind")
	}

	pid := 12345
	if err := s.Started(pid); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteStanding(want); err != nil {
		t.Fatal(err)
	}
	s.Stopped(pid)
	if _, ok := s.Standing(); ok {
		t.Fatal("Stopped left node.standing behind")
	}
}

// A Loop whose Standing hook is set receives each scheduled account's
// clock standing, computed from its own reading against the
// time-proportional target line, on Tick (goal
// every-subscription-lands-full-at-its-reset, item 6).
func TestLoopWritesStanding(t *testing.T) {
	p := newPriorityFake(t)
	loop, _ := p.loop(1)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	loop.Now = func() time.Time { return now }

	known := &KnownAccounts{}
	for _, a := range []AccountConfig{{Label: "max-a", Kind: AccountSubscription}, {Label: "max-b", Kind: AccountSubscription}} {
		if err := known.Note(a, now); err != nil {
			t.Fatal(err)
		}
	}
	loop.Known = known

	// both windows' seven-day resets are 84h out: 50% of the week
	// elapsed, a target of 45% (the 5% reserve) for either account.
	limits := &Limits{}
	limits.Observe("max-a", &harness.RateLimit{ObservedAt: now, SevenDay: harness.RateLimitWindow{Utilization: 0.10, ResetsAt: now.Add(84 * time.Hour)}})
	limits.Observe("max-b", &harness.RateLimit{ObservedAt: now, SevenDay: harness.RateLimitWindow{Utilization: 0.70, ResetsAt: now.Add(84 * time.Hour)}})
	loop.Limits = limits

	var got []AccountStanding
	loop.Standing = func(s []AccountStanding) { got = s }
	loop.Tick(context.Background())

	if len(got) != 2 {
		t.Fatalf("Standing callback got %+v, want 2 accounts", got)
	}
	if got[0].Label != "max-a" || got[1].Label != "max-b" {
		t.Fatalf("Standing order = %+v, want max-a, max-b", got)
	}
	if d := got[0].Distance; d > -0.34 || d < -0.36 {
		t.Fatalf("max-a distance = %v, want about -0.35 (10%% used, 45%% target at 50%% elapsed)", d)
	}
	if d := got[1].Distance; d < 0.24 || d > 0.26 {
		t.Fatalf("max-b distance = %v, want about 0.25", d)
	}
}
