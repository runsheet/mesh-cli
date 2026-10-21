package node

import (
	"context"
	"log"
	"regexp"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// shortReadRetry shortens the gateway client's read retry for one test
// — a window of window, tries of tryTimeout, 10ms between them — and
// puts the defaults back after it (ADR-028).
func shortReadRetry(t *testing.T, window, tryTimeout time.Duration) {
	t.Helper()
	was, wasBackoff, wasTry := gateway.ReadRetryFor, gateway.ReadBackoff, gateway.ReadTryTimeout
	gateway.ReadRetryFor, gateway.ReadBackoff, gateway.ReadTryTimeout = window, []time.Duration{10 * time.Millisecond}, tryTimeout
	t.Cleanup(func() { gateway.ReadRetryFor, gateway.ReadBackoff, gateway.ReadTryTimeout = was, wasBackoff, wasTry })
}

// builtResult is a build that ends by calling mesh_finish.
var builtResult = harness.Result{Success: true, WorkCalls: map[string]map[string]any{
	"mesh_finish": {"item_id": item1, "branch": "mesh/g-1", "head_sha": "0000000"},
}}

// A claim-time call the gateway stalls once — the claim-time read of the
// item's credentials, or the mint of its GitHub token, as a platform
// deploy rolling over did on 2026-10-10 — is tried again inside the
// window and the item runs: pushed, finished, never failed (ADR-028).
func TestGatewayRetryStalledClaimTimeCallStillRunsTheItem(t *testing.T) {
	for _, field := range []string{"itemCredentials", "issueItemRepoToken"} {
		t.Run(field, func(t *testing.T) {
			shortReadRetry(t, 5*time.Second, 200*time.Millisecond)
			f := seed(t)
			f.TransientFirst = map[string]int{field: 1}
			f.TransientAs = map[string]string{field: "stall"}
			r, ws, item, _ := runnerOnFake(t, f, &resultHarness{result: builtResult}, nil)
			logs := &syncBuf{}
			r.Log = log.New(logs, "", 0)
			if err := r.RunItem(context.Background(), item); err != nil {
				t.Fatalf("RunItem: %v\n%s", err, logs)
			}
			if n := len(f.CallsNamed("failWorkItem")); n != 0 {
				t.Fatalf("failWorkItem called %d times after one stalled %s\n%s", n, field, logs)
			}
			if n := len(f.CallsNamed("releaseWorkItem")); n != 0 {
				t.Fatalf("releaseWorkItem called %d times after one stalled %s\n%s", n, field, logs)
			}
			if len(ws.pushed) != 1 || len(f.CallsNamed("finishWorkItem")) != 1 {
				t.Fatalf("pushed %v, finishWorkItem %d: want the item built and finished\n%s", ws.pushed, len(f.CallsNamed("finishWorkItem")), logs)
			}
			if n := len(f.CallsNamed(field)); n != 1 {
				t.Fatalf("%s answered %d times, want the one retry after the stall", field, n)
			}
		})
	}
}

// A gateway that answers 503 to the claim-time read for longer than the
// window fails the item — FailWorkItem, as any other claim-time read
// error does (Runner.fail; a refused credential read is the item's, not
// the node's) — and the lastError counts the tries made (ADR-028).
func TestGatewayRetryDownPastTheWindowFailsTheItemCountingTries(t *testing.T) {
	shortReadRetry(t, 150*time.Millisecond, 100*time.Millisecond)
	f := seed(t)
	f.TransientFirst = map[string]int{"itemCredentials": 1 << 20}
	r, ws, item, _ := runnerOnFake(t, f, &resultHarness{result: builtResult}, nil)
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	if err := r.RunItem(context.Background(), item); err == nil {
		t.Fatalf("RunItem = nil, want the credentials read's error\n%s", logs)
	}
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 {
		t.Fatalf("failWorkItem called %d times, want 1\n%s", len(fails), logs)
	}
	reason, _ := fails[0].Input["error"].(string)
	if reason == "" {
		reason, _ = fails[0].Input["reason"].(string)
	}
	// the job's one exchange: whichever claim-time read asks it first
	if !regexp.MustCompile(`^(session credentials|github token): after [0-9]+ tries over [0-9.]+m?s: `).MatchString(reason) {
		t.Fatalf("failWorkItem reason = %q, want it to open `session credentials: after N tries over D: `", reason)
	}
	if len(ws.pushed) != 0 || len(f.CallsNamed("finishWorkItem")) != 0 {
		t.Fatalf("pushed %v, finished %d: a failed item is neither", ws.pushed, len(f.CallsNamed("finishWorkItem")))
	}
}
