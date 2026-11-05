package nodev1

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// askTestBrief is a minimal Brief naming request_time, for a lease that
// asks for more time.
func askTestBrief() *meshapi.Brief {
	return &meshapi.Brief{
		JobID:     "job-1",
		LeaseID:   "lease-1",
		Kind:      "build",
		Tenant:    "acme",
		NodeTools: []string{RequestTimeTool},
		Harness:   meshapi.BriefHarness{Model: meshapi.ModelPolicy{Default: "sonnet"}, Budget: meshapi.Budget{Minutes: 10}},
		Git:       &meshapi.Git{Repo: "runsheet/mesh-cli"},
	}
}

// A granted ask is recorded with what it asked for and what it got.
func TestAskRecordedOnGrant(t *testing.T) {
	r := &Runtime{WorkRoot: t.TempDir(), Logf: t.Logf}
	b := askTestBrief()

	tb := newTimeBudget(context.Background(), time.Hour, nil)
	defer tb.Stop()
	tool := r.recordRequestTime(b, ladderModel(nil, b), requestTimeTool(tb, 10*time.Minute))

	out, err := tool.Call(context.Background(), map[string]any{
		"minutes": 5.0, "reason": "the suite is slow today", "progress": "three of five steps are green",
	})
	if err != nil {
		t.Fatalf("granted call refused: %v", err)
	}
	_ = out

	recs, err := askRecordsFor(r.asksPath(), b.JobID, b.LeaseID)
	if err != nil {
		t.Fatalf("askRecordsFor: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	rec := recs[0]
	if rec.Type != askKindAsk {
		t.Fatalf("Type = %q, want %q", rec.Type, askKindAsk)
	}
	if !rec.Granted {
		t.Fatalf("Granted = false, want true: %+v", rec)
	}
	if rec.MinutesAsked != 5 {
		t.Fatalf("MinutesAsked = %d, want 5", rec.MinutesAsked)
	}
	if rec.MinutesGranted != 5 {
		t.Fatalf("MinutesGranted = %d, want 5", rec.MinutesGranted)
	}
	if rec.Kind != "build" || rec.Tenant != "acme" || rec.Repo != "runsheet/mesh-cli" || rec.Model != "sonnet" {
		t.Fatalf("record does not name the lease it came from: %+v", rec)
	}
	if rec.Reason != "the suite is slow today" {
		t.Fatalf("Reason = %q", rec.Reason)
	}
	if rec.Progress != "three of five steps are green" {
		t.Fatalf("Progress = %q", rec.Progress)
	}
	if rec.Refusal != "" {
		t.Fatalf("Refusal = %q, want none on a grant", rec.Refusal)
	}
}

// A refusal is recorded whether it comes from the kind's cap or from a
// missing argument requesttimetool.go refuses before ever touching the
// time budget.
func TestAskRecordedOnRefusal(t *testing.T) {
	r := &Runtime{WorkRoot: t.TempDir(), Logf: t.Logf}
	b := askTestBrief()

	tb := newTimeBudget(context.Background(), time.Hour, nil)
	defer tb.Stop()
	tool := r.recordRequestTime(b, ladderModel(nil, b), requestTimeTool(tb, 10*time.Minute))

	// Past the cap: no error, the answer itself says refused.
	if _, err := tool.Call(context.Background(), map[string]any{
		"minutes": 30.0, "reason": "the migration needs more time", "progress": "schema changes are done",
	}); err != nil {
		t.Fatalf("a call past the cap returned an error, want none: %v", err)
	}

	// Missing reason: an error, never reaching the time budget.
	if _, err := tool.Call(context.Background(), map[string]any{
		"minutes": 5.0, "progress": "did some work",
	}); err == nil {
		t.Fatal("a call with no reason was not refused")
	}

	recs, err := askRecordsFor(r.asksPath(), b.JobID, b.LeaseID)
	if err != nil {
		t.Fatalf("askRecordsFor: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(recs), recs)
	}
	for i, rec := range recs {
		if rec.Granted {
			t.Fatalf("record %d: Granted = true, want false: %+v", i, rec)
		}
		if rec.Refusal == "" {
			t.Fatalf("record %d: no refusal reason recorded: %+v", i, rec)
		}
	}
	if recs[0].MinutesAsked != 30 {
		t.Fatalf("record 0: MinutesAsked = %d, want 30", recs[0].MinutesAsked)
	}
}

// A record is durable at the moment of the ask: reading it back through
// a brand-new Runtime over the same WorkRoot — nothing carried in
// memory — still finds it, as if the session that made the ask had been
// cut off immediately afterward.
func TestAskRecordSurvivesCutOff(t *testing.T) {
	root := t.TempDir()
	r := &Runtime{WorkRoot: root, Logf: t.Logf}
	b := askTestBrief()

	tb := newTimeBudget(context.Background(), time.Hour, nil)
	defer tb.Stop()
	tool := r.recordRequestTime(b, ladderModel(nil, b), requestTimeTool(tb, 10*time.Minute))

	if _, err := tool.Call(context.Background(), map[string]any{
		"minutes": 5.0, "reason": "need more time", "progress": "most of it is done",
	}); err != nil {
		t.Fatalf("granted call refused: %v", err)
	}

	// A fresh Runtime, as a new process reading the same WorkRoot after
	// the session was cut off would see.
	fresh := &Runtime{WorkRoot: root, Logf: t.Logf}
	recs, err := askRecordsFor(fresh.asksPath(), b.JobID, b.LeaseID)
	if err != nil {
		t.Fatalf("askRecordsFor: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records after the cut-off, want 1: %+v", len(recs), recs)
	}
	if !recs[0].Granted {
		t.Fatalf("surviving record: %+v", recs[0])
	}
}

// Once the lease that made an ask concludes, the ledger carries an "end"
// record alongside its asks, naming how the job ended — joinable by job
// id and lease id even though the lease itself is long gone.
func TestAskRecordNamesHowTheJobEnded(t *testing.T) {
	r := &Runtime{WorkRoot: t.TempDir(), Logf: t.Logf}
	b := askTestBrief()

	tb := newTimeBudget(context.Background(), time.Hour, nil)
	defer tb.Stop()
	tool := r.recordRequestTime(b, ladderModel(nil, b), requestTimeTool(tb, 10*time.Minute))
	if _, err := tool.Call(context.Background(), map[string]any{
		"minutes": 5.0, "reason": "need more time", "progress": "most of it is done",
	}); err != nil {
		t.Fatalf("granted call refused: %v", err)
	}

	end := r.report(b.LeaseID, b, outcome{cause: errLeaseGone})
	if end.Report != actor.ReportNone {
		t.Fatalf("report() = %+v, want Report %q", end, actor.ReportNone)
	}

	recs, err := askRecordsFor(r.asksPath(), b.JobID, b.LeaseID)
	if err != nil {
		t.Fatalf("askRecordsFor: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (one ask, one end): %+v", len(recs), recs)
	}
	if recs[0].Type != askKindAsk {
		t.Fatalf("recs[0].Type = %q, want %q (ask before end)", recs[0].Type, askKindAsk)
	}
	last := recs[len(recs)-1]
	if last.Type != askKindEnd {
		t.Fatalf("last record's Type = %q, want %q", last.Type, askKindEnd)
	}
	if last.Report != actor.ReportNone {
		t.Fatalf("end record Report = %q, want %q", last.Report, actor.ReportNone)
	}
	if last.EndReason != errLeaseGone.Error() {
		t.Fatalf("end record EndReason = %q, want %q", last.EndReason, errLeaseGone.Error())
	}
}
