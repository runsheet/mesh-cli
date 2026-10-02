package node

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

const failWithClauses = "**Review: FAIL.** The schema is there; the e2e test skips.\n\n" +
	"FAILED: the e2e test runs || SATISFY: TestRigger does not skip before the flow\n" +
	"FAILED: the trd is amended || SATISFY: specs/trd.md §6a describes the process\n\n" +
	"VERDICT: FAIL"

// A failed verdict carries the clauses it failed, so the retry is told
// what to fix in a shape the platform stores rather than only in prose.
func TestFailedEvaluationRecordsItsClauses(t *testing.T) {
	f, ev, _, _ := secondFixture(t, "normal", failWithClauses, &fakeCodexH{})
	in := evaluateItem1(t, f, ev)
	if in["passed"] != false {
		t.Fatalf("verdict: %+v", in)
	}
	got, ok := in[gateway.FailedClausesField].([]any)
	if !ok || len(got) != 2 {
		t.Fatalf("failedClauses = %#v, want 2", in[gateway.FailedClausesField])
	}
	for i, want := range []gateway.FailedClause{
		{Clause: "the e2e test runs", Satisfy: "TestRigger does not skip before the flow"},
		{Clause: "the trd is amended", Satisfy: "specs/trd.md §6a describes the process"},
	} {
		c, _ := got[i].(map[string]any)
		if c["clause"] != want.Clause || c["satisfy"] != want.Satisfy {
			t.Errorf("clause %d = %+v, want %+v", i, c, want)
		}
	}
	// and the item's record keeps them for the next builder
	if f.WorkItems[item1]["failedClauses"] == nil {
		t.Error("the item kept no clauses")
	}
}

// A passed verdict sends none: there is nothing to fix.
func TestPassedEvaluationSendsNoClauses(t *testing.T) {
	f, ev, _, _ := secondFixture(t, "normal", "it holds\nFAILED: ignored || not a failure\nVERDICT: PASS", &fakeCodexH{})
	in := evaluateItem1(t, f, ev)
	if in["passed"] != true {
		t.Fatalf("verdict: %+v", in)
	}
	if _, ok := in[gateway.FailedClausesField]; ok {
		t.Errorf("a pass sent clauses: %#v", in[gateway.FailedClausesField])
	}
}

// A platform whose EvaluateWorkItemInput has no failedClauses gets the
// verdict without them, never a field it would refuse.
func TestOlderPlatformGetsNoClauses(t *testing.T) {
	f, ev, _, _ := secondFixture(t, "normal", failWithClauses, &fakeCodexH{})
	f.NoFailedClauses = true
	in := evaluateItem1(t, f, ev)
	if in["passed"] != false {
		t.Fatalf("verdict: %+v", in)
	}
	if _, ok := in[gateway.FailedClausesField]; ok {
		t.Errorf("clauses sent to a platform without the field: %#v", in[gateway.FailedClausesField])
	}
	if in["notes"] == "" {
		t.Error("the verdict's notes were dropped too")
	}
}
