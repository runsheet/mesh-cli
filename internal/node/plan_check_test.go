package node

import (
	"context"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// ADR-021's amendment: a re-plan never recreates a failed item at the
// same acceptance.

func TestNormaliseAcceptance(t *testing.T) {
	base := "RegisterNode pins the id to NodeID(owner, slug); `go test ./internal/node/...` passes."
	for _, same := range []string{
		"registernode pins the id to nodeid(owner, slug); go test ./internal/node/... passes",
		"  RegisterNode  pins the id\nto `NodeID(owner, slug)`;\n\n`go test ./internal/node/...` passes!  ",
		"REGISTERNODE PINS THE ID TO NODEID OWNER SLUG GO TEST INTERNAL NODE PASSES",
	} {
		if normaliseAcceptance(same) != normaliseAcceptance(base) {
			t.Errorf("%q normalises to %q, want %q", same, normaliseAcceptance(same), normaliseAcceptance(base))
		}
	}
	if changed := "RegisterNode pins the id to NodeID(owner, slug); `go test ./internal/gateway/...` passes."; normaliseAcceptance(changed) == normaliseAcceptance(base) {
		t.Errorf("a changed clause normalises equal: %q", normaliseAcceptance(changed))
	}
	if got := normaliseAcceptance("One `row`,  per node."); got != "one row per node" {
		t.Errorf("normalised = %q", got)
	}
}

const ownerAcceptance = "RegisterNode pins the aggregate id to NodeID(owner, slug); transfersNode applies ownsAs to the source; `go test ./...` passes."

func TestSameAcceptanceAsFailed(t *testing.T) {
	spec := func(seq int, acc, atomic string) harness.PlannedSpec {
		return harness.PlannedSpec{Seq: seq, Title: "Re-land the owner model", Acceptance: acc, Atomic: atomic}
	}
	failed := gateway.WorkItem{Seq: 14, Title: "Re-land the owner model", Status: "failed", Acceptance: ownerAcceptance, LineageRootSeq: 1, LineageAttempt: 3}

	flags := sameAcceptanceAsFailed([]harness.PlannedSpec{spec(20, "registernode pins the aggregate id to `NodeID(owner, slug)`. transfersNode applies ownsAs to the source; go test ./... passes", "")}, []gateway.WorkItem{failed})
	want := "item 20 (Re-land the owner model): its acceptance is the same as failed item 14's (lineage 1, attempt 3 of 3) — split it: partition the acceptance into two or more items, one PR each, naming the dependency"
	if len(flags) != 1 || flags[0] != want {
		t.Fatalf("flags = %q, want [%q]", flags, want)
	}

	// an older platform serves no lineage: the failed seq is its lineage
	old := failed
	old.LineageRootSeq, old.LineageAttempt = 0, 0
	flags = sameAcceptanceAsFailed([]harness.PlannedSpec{spec(20, ownerAcceptance, "")}, []gateway.WorkItem{old})
	if len(flags) != 1 || !strings.Contains(flags[0], "failed item 14's (lineage 14)") {
		t.Fatalf("flags = %q, want lineage 14", flags)
	}

	// superseded, and cancelled with its own error, count; a merged item,
	// a ready one and a dependent cancelled by a re-plan do not
	for _, c := range []struct {
		status, lastError string
		flagged           bool
	}{
		{"superseded", "", true},
		{"cancelled", "evaluation failed 3 times", true},
		{"cancelled", "", false},
		{"merged", "", false},
		{"ready", "", false},
	} {
		it := failed
		it.Status, it.LastError = c.status, c.lastError
		if got := len(sameAcceptanceAsFailed([]harness.PlannedSpec{spec(20, ownerAcceptance, "")}, []gateway.WorkItem{it})) > 0; got != c.flagged {
			t.Errorf("%s (%q): flagged %v, want %v", c.status, c.lastError, got, c.flagged)
		}
	}

	// a changed acceptance is not a re-land
	if flags := sameAcceptanceAsFailed([]harness.PlannedSpec{spec(20, "RegisterNode pins the aggregate id to NodeID(owner, slug); `go test ./internal/node/...` passes.", "")}, []gateway.WorkItem{failed}); len(flags) != 0 {
		t.Fatalf("a partition is flagged: %q", flags)
	}

	// atomic is exempt once: against a failed item that was not atomic it
	// passes; against one that was, the exemption is spent
	if flags := sameAcceptanceAsFailed([]harness.PlannedSpec{spec(20, ownerAcceptance, "one migration, cannot be halved")}, []gateway.WorkItem{failed}); len(flags) != 0 {
		t.Fatalf("an atomic item against a non-atomic failure is flagged: %q", flags)
	}
	wasAtomic := failed
	wasAtomic.Brief = "atomic: one migration, cannot be halved\n\nThe owner model."
	if flags := sameAcceptanceAsFailed([]harness.PlannedSpec{spec(20, ownerAcceptance, "one migration, cannot be halved")}, []gateway.WorkItem{wasAtomic}); len(flags) != 1 {
		t.Fatalf("an atomic item against an atomic failure: flags = %q, want one", flags)
	}
}

const (
	// the narrow re-plan as a-node-has-an-owner had it: item 2's scope
	// again, at its acceptance
	reLandPlan = "```json\n{\"items\": [{\"seq\": 3, \"title\": \"Retry of item 2 — make it pass\", \"brief\": \"Again.\", \"acceptance\": \"Go test passes.\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"
	// the split: item 2's acceptance partitioned, one PR each
	splitPlan = "```json\n{\"items\": [" +
		"{\"seq\": 3, \"title\": \"The owner's id\", \"brief\": \"Half.\", \"acceptance\": \"go test ./internal/node/... passes\", \"depends_on\": [], \"repo\": \"acme/widgets\"}," +
		"{\"seq\": 4, \"title\": \"The transfer\", \"brief\": \"The other half.\", \"acceptance\": \"go test ./internal/gateway/... passes\", \"depends_on\": [{\"seq\": 3, \"because\": \"uses the owner's id item 3 adds\"}], \"repo\": \"acme/widgets\"}]}\n```"
	atomicPlan = "```json\n{\"items\": [{\"seq\": 3, \"title\": \"The owner migration\", \"brief\": \"One migration.\", \"acceptance\": \"go test ./internal/store/... passes\", \"atomic\": \"one migration, cannot be halved\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"
)

// planReplan runs the planner on the seeded goal whose item 2 failed its
// evaluations, lineage 2 attempt 2, with the harness's answers in turn.
func planReplan(t *testing.T, answers ...string) (*gatewaytest.Fake, *fakeHarness, *strings.Builder, error) {
	t.Helper()
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.WorkItems[item2]["status"] = "failed"
	f.WorkItems[item2]["lastError"] = "evaluation failed 3 times"
	f.WorkItems[item2]["lineageRootSeq"] = 2
	f.WorkItems[item2]["lineageAttempt"] = 2
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: answers}
	var logged strings.Builder
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logged, "", 0)}
	err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0])
	return f, h, &logged, err
}

// A first answer that recreates the failed item at its acceptance is
// asked again, told to split and which lineage; the split is handed in.
func TestPlannerReasksToSplit(t *testing.T) {
	f, h, _, err := planReplan(t, reLandPlan, splitPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	// the items so far show the failed item's acceptance and lineage
	for _, want := range []string{"2. Make it pass — failed: evaluation failed 3 times\n  lineage 2, attempt 2 of 3\n  Acceptance: go test passes\n"} {
		if !strings.Contains(h.jobs[0].Prompt, want) {
			t.Fatalf("plan prompt lacks %q:\n%s", want, h.jobs[0].Prompt)
		}
	}
	want := "- item 3 (Retry of item 2 — make it pass): its acceptance is the same as failed item 2's (lineage 2, attempt 2 of 3) — split it: partition the acceptance into two or more items, one PR each, naming the dependency"
	if !strings.Contains(h.jobs[1].Prompt, want) {
		t.Fatalf("re-ask lacks %q:\n%s", want, h.jobs[1].Prompt)
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	items := calls[0].Input["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["title"] != "The owner's id" || items[1].(map[string]any)["title"] != "The transfer" {
		t.Fatalf("handed in: %v, want the split", items)
	}
}

// A second answer that still recreates it is not handed in: the error
// names the lineage, and the goal gets no fourth re-land from this node.
func TestPlannerRefusesAFourthReLand(t *testing.T) {
	f, h, logged, err := planReplan(t, reLandPlan, reLandPlan)
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	if err == nil || err.Error() != "plan would re-land failed item 2 (lineage 2) at the same acceptance; refused" {
		t.Fatalf("err = %v", err)
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 0 {
		t.Fatalf("a re-land was handed in: %+v", calls)
	}
	if !strings.Contains(logged.String(), "its acceptance is the same as failed item 2's") {
		t.Fatalf("the re-ask's flag is not logged:\n%s", logged)
	}
}

// A planned item's atomic reason rides its brief's first line to
// PlanGoal, and AtomicReason reads it back.
func TestAtomicReasonRidesTheBrief(t *testing.T) {
	f, h, _, err := planReplan(t, atomicPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("planner sessions = %d, want 1", len(h.jobs))
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	brief, _ := calls[0].Input["items"].([]any)[0].(map[string]any)["brief"].(string)
	if brief != "atomic: one migration, cannot be halved\n\nOne migration." {
		t.Fatalf("brief = %q", brief)
	}
	if got := gateway.AtomicReason(brief); got != "one migration, cannot be halved" {
		t.Fatalf("AtomicReason = %q", got)
	}
	for brief, want := range map[string]string{"": "", "One migration.": "", "Atomic:  a schema change \nmore": "a schema change", "not atomic: x": ""} {
		if got := gateway.AtomicReason(brief); got != want {
			t.Errorf("AtomicReason(%q) = %q, want %q", brief, got, want)
		}
	}
	if got := gateway.BriefWithAtomic("Body.", ""); got != "Body." {
		t.Errorf("no reason changed the brief: %q", got)
	}
}
