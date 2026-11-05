package commands

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/nodev1"
)

// TestNodeAsksReport builds a ledger spanning two kinds (build,
// evaluate) and two repos (acme/one, acme/two), mixing granted and
// refused asks, and mixing finished and not-finished outcomes among
// the jobs that got a grant, then checks aggregateAsks reports the
// right counts for each.
func TestNodeAsksReport(t *testing.T) {
	ask := func(jobID, leaseID, kind, repo string, granted bool) nodev1.AskRecord {
		return nodev1.AskRecord{Type: "ask", JobID: jobID, LeaseID: leaseID, Kind: kind, Repo: repo, Granted: granted}
	}
	end := func(jobID, leaseID, report string) nodev1.AskRecord {
		return nodev1.AskRecord{Type: "end", JobID: jobID, LeaseID: leaseID, Report: report}
	}

	records := []nodev1.AskRecord{
		// build / acme/one: granted, lease finished.
		ask("job-1", "lease-1", "build", "acme/one", true),
		end("job-1", "lease-1", actor.ReportFinish),

		// build / acme/one: a second ask on another job, refused.
		ask("job-2", "lease-2", "build", "acme/one", false),
		end("job-2", "lease-2", actor.ReportFail),

		// build / acme/two: granted, but the lease failed, not finished.
		ask("job-3", "lease-3", "build", "acme/two", true),
		end("job-3", "lease-3", actor.ReportFail),

		// evaluate / acme/two: granted, lease still running (no end yet).
		ask("job-4", "lease-4", "evaluate", "acme/two", true),

		// evaluate / acme/two: refused.
		ask("job-5", "lease-5", "evaluate", "acme/two", false),

		// evaluate / acme/one: granted, lease finished, with the end
		// line arriving in the ledger before its own ask line, to
		// prove order does not matter.
		end("job-6", "lease-6", actor.ReportFinish),
		ask("job-6", "lease-6", "evaluate", "acme/one", true),

		// a line of a type aggregateAsks does not know should be
		// ignored, not mistaken for an ask or an end.
		{Type: "other", JobID: "job-7", LeaseID: "lease-7"},
	}

	report := aggregateAsks(records)

	wantKind := map[string]AskTally{
		"build":    {Name: "build", Asks: 3, Granted: 2, Refused: 1},
		"evaluate": {Name: "evaluate", Asks: 3, Granted: 2, Refused: 1},
	}
	if len(report.ByKind) != len(wantKind) {
		t.Fatalf("ByKind has %d entries, want %d: %+v", len(report.ByKind), len(wantKind), report.ByKind)
	}
	for _, got := range report.ByKind {
		want, ok := wantKind[got.Name]
		if !ok {
			t.Fatalf("ByKind has unexpected kind %q", got.Name)
		}
		if got != want {
			t.Errorf("ByKind[%q] = %+v, want %+v", got.Name, got, want)
		}
	}

	wantRepo := map[string]AskTally{
		"acme/one": {Name: "acme/one", Asks: 3, Granted: 2, Refused: 1},
		"acme/two": {Name: "acme/two", Asks: 3, Granted: 2, Refused: 1},
	}
	if len(report.ByRepo) != len(wantRepo) {
		t.Fatalf("ByRepo has %d entries, want %d: %+v", len(report.ByRepo), len(wantRepo), report.ByRepo)
	}
	for _, got := range report.ByRepo {
		want, ok := wantRepo[got.Name]
		if !ok {
			t.Fatalf("ByRepo has unexpected repo %q", got.Name)
		}
		if got != want {
			t.Errorf("ByRepo[%q] = %+v, want %+v", got.Name, got, want)
		}
	}

	// Granted jobs: job-1, job-3, job-4, job-6 (4). Finished among
	// them: job-1 and job-6 (2). job-3 failed, job-4 has no end yet.
	if report.GrantedJobs != 4 {
		t.Errorf("GrantedJobs = %d, want 4", report.GrantedJobs)
	}
	if report.GrantedFinished != 2 {
		t.Errorf("GrantedFinished = %d, want 2", report.GrantedFinished)
	}

	lines := report.Lines()
	if len(lines) == 0 {
		t.Fatal("Lines() returned nothing")
	}
}

// An empty ledger reports plainly, not a zero-filled table.
func TestNodeAsksReportEmpty(t *testing.T) {
	report := aggregateAsks(nil)
	if len(report.ByKind) != 0 || len(report.ByRepo) != 0 {
		t.Fatalf("expected no tallies, got %+v", report)
	}
	lines := report.Lines()
	if len(lines) != 1 || lines[0] != "asks: none recorded" {
		t.Fatalf("Lines() = %v, want a single 'none recorded' line", lines)
	}
}
