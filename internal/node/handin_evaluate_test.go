package node

import (
	"context"
	"strings"
	"testing"
)

// A close the product refuses — the session handed no verdict to its
// tool — is logged with the product's own answer, naming the item and
// the call, and is the evaluation's error.
func TestHandInRefusalIsLoggedByTheEvaluator(t *testing.T) {
	refusal := "finishJob: call mesh_verdict first (quokka-7)"
	f, ev, claude, _, _, logs := familyFixture(t, "normal", "sonnet", "", "VERDICT: PASS")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	claude.texts = []string{"Holds, I think."}
	f.EvaluationNotHandedIn = refusal
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	err = ev.Evaluate(context.Background(), *item)
	if err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("Evaluate = %v, want the refusal", err)
	}
	want := "item ship-it/1: FinishJob refused: "
	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, want) {
			line = l
		}
	}
	if line == "" || !strings.Contains(line, refusal) {
		t.Fatalf("no line %q naming %q in node.log:\n%s", want, refusal, logs.String())
	}
	if n := len(f.CallsNamed("evaluateWorkItem")); n != 0 {
		t.Fatalf("the node sent %d verdicts of its own", n)
	}
}
