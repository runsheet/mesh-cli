package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// A lease handed back for a reason no cause-specific log line already
// carries -- cannot_run found while preparing the lease, before any
// session runs, when the scheduler has no room for it -- still reaches
// the node's own log with the hand-back's reason text, not only Mesh's
// hand-back call.
func TestAHandBacksReasonReachesTheNodeLog(t *testing.T) {
	g := newRig(t, &stubSession{calls: []string{"conform/submit"}})
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, Slots: 1})
	// a trim holds the build cache past the lease's wait, so the
	// scheduler has no room left for it
	trim, err := g.rt.Scheduler.TryTake(scheduler.Ask{Owner: "trim", Cache: scheduler.CacheExclusive})
	if err != nil {
		t.Fatal(err)
	}
	defer trim.Release()

	var mu sync.Mutex
	var lines []string
	g.rt.Logf = func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		mu.Lock()
		lines = append(lines, msg)
		mu.Unlock()
		t.Log(msg)
	}

	job := g.offer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	run(t, g, ctx)

	calls := g.fake.Calls("hand-back")
	if len(calls) != 1 || calls[0].LeaseID != g.fake.LeaseOf(job) {
		t.Fatalf("hand-backs %+v", calls)
	}
	var hb meshapi.HandBackRequest
	if err := json.Unmarshal(calls[0].Body, &hb); err != nil {
		t.Fatal(err)
	}
	if hb.Cause != meshapi.CauseCannotRun || hb.Reason == "" {
		t.Fatalf("hand-back %+v", hb)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, line := range lines {
		if strings.Contains(line, hb.Reason) {
			return
		}
	}
	t.Fatalf("no node log line carries the hand-back's reason %q: %v", hb.Reason, lines)
}
