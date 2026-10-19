package node

import (
	"context"
	"testing"
	"time"
)

// The work sweep runs hourly on the loop's clock (mesh-cli ADR 008, The
// work sweep): the first tick follows the start-up pass and starts the
// hour, ticks inside it call nothing, and the tick past it calls
// WorkSweep once.
func TestWorkSweepHourly(t *testing.T) {
	p := newPriorityFake(t)
	l, _ := p.loop(1)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }
	calls := 0
	l.WorkSweep = func(context.Context) { calls++ }
	ctx := context.Background()
	steps := []struct {
		after time.Duration // since the step before
		calls int           // WorkSweep's calls after the tick
	}{
		{0, 0},                // the first tick: the start-up pass just ran
		{time.Minute, 0},      // inside the hour
		{58 * time.Minute, 0}, // still inside it
		{2 * time.Minute, 1},  // past it: once
		{time.Second, 1},      // not twice within the hour
		{59 * time.Minute, 1}, // inside the next hour
		{time.Minute, 2},      // past it
	}
	for i, s := range steps {
		now = now.Add(s.after)
		l.Tick(ctx)
		l.wg.Wait()
		if calls != s.calls {
			t.Fatalf("step %d (+%s): WorkSweep called %d times, want %d", i, s.after, calls, s.calls)
		}
	}
}

// WorkSweepEvery sets the period.
func TestWorkSweepHourlyEveryOverrides(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	calls := 0
	l := &Loop{Now: func() time.Time { return now }, WorkSweepEvery: 10 * time.Minute, WorkSweep: func(context.Context) { calls++ }}
	ctx := context.Background()
	l.runWorkSweep(ctx)
	now = now.Add(9 * time.Minute)
	l.runWorkSweep(ctx)
	if calls != 0 {
		t.Fatalf("called %d times within the period", calls)
	}
	now = now.Add(time.Minute)
	l.runWorkSweep(ctx)
	if calls != 1 {
		t.Fatalf("called %d times at the period, want 1", calls)
	}
}
