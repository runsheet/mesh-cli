package node

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// shortReportRetry shortens the fail's and release's retry for a test.
func shortReportRetry(t *testing.T) {
	t.Helper()
	within, back, try := ReportRetryFor, ReportBackoff, ReportTryTimeout
	ReportRetryFor, ReportBackoff, ReportTryTimeout = 5*time.Second, []time.Duration{10 * time.Millisecond}, 2*time.Second
	t.Cleanup(func() { ReportRetryFor, ReportBackoff, ReportTryTimeout = within, back, try })
}

// exitsAtOnce is a harness whose session ends at once without success.
var exitsAtOnce = harnessFunc(func(context.Context, harness.Job, func(harness.Progress)) (harness.Result, error) {
	return harness.Result{Success: false, Text: "the runsheet server failed to start"}, nil
})

// A FailWorkItem the gateway answers transiently once is sent again and
// lands: the item is failed with the session's text, no release sent.
func TestSessionThatExitsAtOnceFailIsRetriedOnATransientAnswer(t *testing.T) {
	shortReportRetry(t)
	f := seed(t)
	f.TransientFirst = map[string]int{"failWorkItem": 1}
	r, _, item, row := runnerOnFake(t, f, exitsAtOnce, nil)
	var logs syncBuf
	r.Log = log.New(&logs, "", 0)
	r.RunItem(context.Background(), item)

	calls := f.CallsNamed("failWorkItem")
	if len(calls) != 1 {
		t.Fatalf("failWorkItem recorded %d times, want 1 (the transient answer is not recorded): %v", len(calls), calls)
	}
	if reason, _ := calls[0].Input["error"].(string); !strings.Contains(reason, "the runsheet server failed to start") {
		t.Fatalf("fail reason = %q", reason)
	}
	if row(item1)["status"] != "failed" {
		t.Fatalf("row: %v", row(item1))
	}
	if rel := f.CallsNamed("releaseWorkItem"); len(rel) != 0 {
		t.Fatalf("a fail that landed was released too: %v", rel)
	}
	if strings.Contains(logs.String(), "fail refused") {
		t.Fatalf("a transient answer logged as a refusal:\n%s", logs.String())
	}
}

// A FailWorkItem the gateway refuses is logged `refused` with the
// gateway's own words and followed by a release naming the reason, so the
// item is never left claimed for the platform's stall sweep.
func TestSessionThatExitsAtOnceFailRefusedIsReleased(t *testing.T) {
	shortReportRetry(t)
	f := seed(t)
	f.Refuse["failWorkItem"] = "failWorkItem: the item is not running"
	r, _, item, row := runnerOnFake(t, f, exitsAtOnce, nil)
	var logs syncBuf
	r.Log = log.New(&logs, "", 0)
	r.RunItem(context.Background(), item)

	if !strings.Contains(logs.String(), "fail refused: failWorkItem: the item is not running") {
		t.Fatalf("the refusal is not logged with the gateway's answer:\n%s", logs.String())
	}
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 {
		t.Fatalf("releaseWorkItem recorded %d times, want 1", len(rel))
	}
	if note, _ := rel[0].Input["note"].(string); !strings.Contains(note, "session ended without success: the runsheet server failed to start") {
		t.Fatalf("release note = %q, want the reason", note)
	}
	if st := row(item1)["status"]; st != "ready" {
		t.Fatalf("item left %v, want ready", st)
	}
}

// A release refused after a refused fail is logged with its answer too.
func TestSessionThatExitsAtOnceReleaseRefusedIsLogged(t *testing.T) {
	shortReportRetry(t)
	f := seed(t)
	f.Refuse["failWorkItem"] = "failWorkItem: the item is not running"
	f.Refuse["releaseWorkItem"] = "releaseWorkItem: not held by this node"
	r, _, item, _ := runnerOnFake(t, f, exitsAtOnce, nil)
	var logs syncBuf
	r.Log = log.New(&logs, "", 0)
	r.RunItem(context.Background(), item)
	if !strings.Contains(logs.String(), "release refused: releaseWorkItem: not held by this node") {
		t.Fatalf("the release's refusal is not logged with its answer:\n%s", logs.String())
	}
}
