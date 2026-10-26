package node

import (
	"context"
	"strings"
	"testing"
)

// A verdict the gateway refuses is logged with the gateway's own answer,
// naming the item and the call: the evaluator's verdict and its
// no-verdict alike. Each subtest drives the real evaluator against the
// fake, which refuses evaluateWorkItem (the no-verdict outcome is the
// same mutation, noVerdict set).
func TestHandInRefusalIsLoggedByTheEvaluator(t *testing.T) {
	for _, tc := range []struct {
		call  string
		calls map[string]map[string]any
	}{
		{
			call: "EvaluateWorkItem",
			calls: map[string]map[string]any{"mesh_verdict": {
				"item_id": item1, "passed": true, "notes": "Holds.",
			}},
		},
		{
			// no mesh_result call: the session reaches no verdict
			call: "EvaluateWorkItemNoVerdict",
		},
	} {
		t.Run(tc.call, func(t *testing.T) {
			refusal := "the gateway says no to " + tc.call + " (quokka-7)"
			f, ev, claude, _, _, logs := familyFixture(t, "normal", "sonnet", "", "VERDICT: PASS")
			ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
			claude.texts = []string{"Holds, I think."}
			claude.workCalls = tc.calls
			f.Refuse["evaluateWorkItem"] = refusal
			item, err := ev.GW.WorkItem(context.Background(), item1)
			if err != nil {
				t.Fatal(err)
			}
			err = ev.Evaluate(context.Background(), *item)
			if err == nil || !strings.Contains(err.Error(), refusal) {
				t.Fatalf("Evaluate = %v, want the refusal", err)
			}
			want := "item ship-it/1: " + tc.call + " refused: "
			var line string
			for _, l := range strings.Split(logs.String(), "\n") {
				if strings.Contains(l, want) {
					line = l
				}
			}
			if line == "" || !strings.Contains(line, refusal) {
				t.Fatalf("no line %q naming %q in node.log:\n%s", want, refusal, logs.String())
			}
		})
	}
}
