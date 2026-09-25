package node

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// The ledger sums an account's spend today and this month in its own
// time zone: a new day starts the day's sum again, a new month the
// month's. It is kept on disk, so a node that restarts reads back what
// it had spent.
func TestLedgerSumsTodayAndThisMonthAndSurvivesReload(t *testing.T) {
	sydney := time.FixedZone("AEST", 10*60*60)
	path := filepath.Join(t.TempDir(), "ledger.json")
	c := &clock{now: time.Date(2026, 9, 29, 20, 0, 0, 0, sydney)}
	l, err := OpenLedger(path, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	l.Location = sydney
	for _, usd := range []float64{1.5, 2.25, 0} {
		if err := l.Record("key", usd); err != nil {
			t.Fatal(err)
		}
	}
	_ = l.Record("other", 9)
	_ = l.AddSlotMinutes("key", 30)
	if d, m := l.Spent("key"); d != 3.75 || m != 3.75 {
		t.Fatalf("spent %v today, %v this month; want 3.75, 3.75", d, m)
	}

	// 23:30 the next day, local: still September there, a new day
	c.Set(time.Date(2026, 9, 30, 23, 30, 0, 0, sydney))
	_ = l.Record("key", 1)
	if d, m := l.Spent("key"); d != 1 || m != 4.75 {
		t.Fatalf("next day: spent %v today, %v this month; want 1, 4.75", d, m)
	}
	if got := l.Today("key").SlotMinutes; got != 0 {
		t.Fatalf("slot-minutes carried into a new day: %v", got)
	}

	// a restart reads the file back
	again, err := OpenLedger(path, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	again.Location = sydney
	if d, m := again.Spent("key"); d != 1 || m != 4.75 {
		t.Fatalf("after reload: spent %v today, %v this month; want 1, 4.75", d, m)
	}
	if d, _ := again.Spent("other"); d != 0 {
		t.Fatalf("other's yesterday counted today: %v", d)
	}
	if _, m := again.Spent("other"); m != 9 {
		t.Fatalf("other this month %v, want 9", m)
	}

	// midnight into October, local (still 30 September in UTC)
	c.Set(time.Date(2026, 10, 1, 0, 5, 0, 0, sydney))
	if d, m := again.Spent("key"); d != 0 || m != 0 {
		t.Fatalf("a new month: %v today, %v this month; want 0, 0", d, m)
	}
}

// A missing ledger file is an empty ledger.
func TestLedgerMissingFile(t *testing.T) {
	l, err := OpenLedger(filepath.Join(t.TempDir(), "none", "ledger.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d, m := l.Spent("key"); d != 0 || m != 0 {
		t.Fatalf("spent %v, %v", d, m)
	}
}

// Every session's list-price cost lands under the account its env runs on.
func TestObserveSpend(t *testing.T) {
	l := &Ledger{}
	h := ObserveSpend(harnessFunc(func(context.Context, harness.Job, func(harness.Progress)) (harness.Result, error) {
		return harness.Result{CostUSD: 0.4}, nil
	}), l, nil)
	for range 2 {
		if _, err := h.Run(context.Background(), harness.Job{Env: []string{AccountEnv + "=key"}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if d, _ := l.Spent("key"); d != 0.8 {
		t.Fatalf("key spent %v, want 0.8", d)
	}
}
