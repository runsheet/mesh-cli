package node

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Run runs every check and returns every finding, a panicking check
// caught and turned into one Finding naming it — the other checks'
// findings still returned — and a second Run inside Every returns
// without running the checks again (goal
// the-quartermaster-keeps-the-nodes-fit).
func TestQuartermasterRun(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var calls int
	ok := func(ctx context.Context, now time.Time) []Finding {
		calls++
		return []Finding{{Kind: "disk", Severity: "ok", Subject: "/", Detail: "fine", At: now}}
	}
	panicker := func(ctx context.Context, now time.Time) []Finding {
		calls++
		panic("boom")
	}
	q := &Quartermaster{Checks: []Check{ok, panicker}, Every: 10 * time.Minute}

	got := q.Run(context.Background(), now)
	if len(got) != 2 {
		t.Fatalf("Run() = %+v, want 2 findings (one from each check)", got)
	}
	if got[0].Kind != "disk" || got[0].Severity != "ok" {
		t.Fatalf("first finding = %+v, want the ok check's", got[0])
	}
	if got[1].Severity != "critical" || !strings.Contains(got[1].Detail, "boom") {
		t.Fatalf("panicking check's finding = %+v, want a critical finding naming the panic", got[1])
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}

	// Inside Every: no check runs again, the same findings come back.
	again := q.Run(context.Background(), now.Add(5*time.Minute))
	if calls != 2 {
		t.Fatalf("calls after a second Run inside Every = %d, want still 2", calls)
	}
	if len(again) != 2 {
		t.Fatalf("second Run inside Every = %+v, want the same 2 findings", again)
	}

	// Past Every: the checks run again.
	q.Run(context.Background(), now.Add(11*time.Minute))
	if calls != 4 {
		t.Fatalf("calls after a Run past Every = %d, want 4", calls)
	}
}
