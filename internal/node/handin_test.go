package node

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

// Every hand-in the gateway refuses is logged with the gateway's own
// answer, naming the item and the call. Each row drives the real runner
// path to one finish against the fake, which refuses its mutation.
func TestHandInRefusalIsLogged(t *testing.T) {
	for _, tc := range []struct {
		call     string // the call the log line names, and the subtest's name
		mutation string // the fake's mutation that refuses
		calls    map[string]map[string]any
		// changesets: the platform takes changesets, so the finish is
		// recorded from a real checkout of origin
		changesets bool
		// exits: the session ends without success, so the node fails the
		// item; refuse: other mutations the fake refuses, each with its
		// own text; the refusal then does not reach RunItem's error
		exits  bool
		refuse map[string]string
	}{
		{
			call:     "FinishWorkItem",
			mutation: "finishWorkItem",
			calls: map[string]map[string]any{
				"mesh_finish": {"item_id": item1, "branch": "mesh/g-1", "head_sha": "abc123def456"},
			},
		},
		{
			call:     "FinishWorkItemAlreadyDone",
			mutation: "finishWorkItem",
			calls: map[string]map[string]any{
				"mesh_finish": {"item_id": item1, "outcome": "already_done", "evidence": "https://github.com/acme/widgets/pull/3 merged it"},
			},
		},
		{
			call:       "FinishWorkItemChangeset",
			mutation:   "finishWorkItem",
			changesets: true,
			calls: map[string]map[string]any{
				"mesh_finish": {"item_id": item1, "branch": "mesh/g-1", "head_sha": "abc123def456"},
			},
		},
		{
			call:     "FailWorkItem",
			mutation: "failWorkItem",
			exits:    true,
		},
		{
			// a refused fail is followed by a release naming the reason,
			// which the fake refuses too
			call:     "ReleaseWorkItem",
			mutation: "releaseWorkItem",
			exits:    true,
			refuse:   map[string]string{"failWorkItem": "the gateway says no to the fail (wombat-3)"},
		},
	} {
		t.Run(tc.call, func(t *testing.T) {
			refusal := "the gateway says no to " + tc.call + " (quokka-7)"
			f := seed(t)
			f.Refuse[tc.mutation] = refusal
			for m, text := range tc.refuse {
				f.Refuse[m] = text
			}
			f.Changesets = tc.changesets
			var h harness.Harness = &resultHarness{result: harness.Result{Success: true, WorkCalls: tc.calls}}
			if tc.exits {
				shortReportRetry(t)
				h = exitsAtOnce
			}
			r, ws, item, _ := runnerOnFake(t, f, h, nil)
			if tc.changesets {
				r.WS = &gitWS{fakeWS: ws, t: t, origin: originRepo(t, nil)}
			}
			logs := &syncBuf{}
			r.Log = log.New(logs, "", 0)
			if err := r.RunItem(context.Background(), item); tc.exits {
				if err == nil {
					t.Fatalf("RunItem = nil, want the session's failure")
				}
			} else if err == nil || !strings.Contains(err.Error(), refusal) {
				t.Fatalf("RunItem = %v, want the refusal", err)
			}
			var line string
			for _, l := range strings.Split(logs.String(), "\n") {
				if strings.Contains(l, " "+tc.call+" refused: ") {
					line = l
					break
				}
			}
			if line == "" {
				t.Fatalf("no %s refused line:\n%s", tc.call, logs)
			}
			if want := fmt.Sprintf("item %s/%d: %s refused: ", f.Goals[goalID]["slug"], item.Seq, tc.call); !strings.Contains(line, want) {
				t.Errorf("line %q does not name the item", line)
			}
			if !strings.Contains(line, refusal) {
				t.Errorf("line %q does not carry the gateway's answer %q", line, refusal)
			}
		})
	}
}
