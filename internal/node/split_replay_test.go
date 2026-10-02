package node

import (
	"context"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The replay of a-node-has-an-owner (ADR-021, runsheet ADR 023): on the
// real goal, item 1 "Schema and policy: a Node has an owner …" failed
// three evaluations on real defects, the narrow re-plan recreated it as
// item 8 "Retry of item 1 — schema and policy …" at the same scope,
// which failed three more, then item 14 "Re-land the owner model …",
// then item 20 — before items 27-30 landed it as four small pull
// requests, each on its first pass. Replayed on today's planner, the
// plan check refuses a fourth re-land of the same acceptance and the
// planner is told to split, both on a whole re-plan (ADR-021's
// amendment, item 2) and on a narrow re-plan of item 20 alone
// (ADR-021's narrow-mode amendment, item 4).

const (
	ownerGoalID = "99999999-9000-4900-8800-900000000001"
	ownerItem1  = "99999999-9001-4901-8801-900000000001"
	ownerItem2  = "99999999-9001-4901-8801-900000000002"
	ownerItem3  = "99999999-9001-4901-8801-900000000003"
	ownerItem8  = "99999999-9001-4901-8801-900000000008"
	ownerItem14 = "99999999-9001-4901-8801-900000000014"
	ownerItem20 = "99999999-9001-4901-8801-900000000020"

	// the real titles (mesh_items, goal a-node-has-an-owner)
	ownerItem1Title  = "Schema and policy: a Node has an owner (person | enterprise), one row per node, TransferNode and ShareNodeWithOrg, with the ADR"
	ownerItem8Title  = "Retry of item 1 — schema and policy: a Node has an owner (person | enterprise), one row per node, TransferNode and ShareNodeWithOrg, with ADR 033"
	ownerItem14Title = "Re-land the owner model: a Node has an owner (person | enterprise), NodeID per owner and slug with a legacy per-org registration kept, TransferNode, ShareNodeWithOrg, UnshareNodeWithOrg, ADR 033"
	ownerItem20Title = "Owner model in schema and aggregate: NodeOwnerKind, one owner row per node beside the legacy per-org row, TransferNode, ShareNodeWithOrg, UnshareNodeWithOrg, keeper-scoped policy rows, ADR 034"

	// the one acceptance items 1, 8, 14 and 20 share — the same scope
	// re-landed three times before the split (the goal statement's own
	// words for the two real defects: RegisterNode does not pin the
	// aggregate id to NodeID(owner, slug); transfersNode applies ownsAs
	// to the destination, not the source)
	ownerGoalAcceptance = "RegisterNode pins the aggregate id to NodeID(owner, slug); transfersNode applies ownsAs to the source; `go test ./...` passes."

	// item 1's evaluation notes, naming the real defects (its evaluation
	// rather than its lastError, so the prompt's "items so far" quotes
	// them, not just "evaluation failed 3 times")
	ownerItem1Evaluation = "Item 1 fails review a third time: RegisterNode does not pin the aggregate id to NodeID(owner, slug), so a re-registration keeps the legacy per-org id; and transfersNode applies ownsAs to the destination, not the source, so the source node keeps the old policy rows after a transfer.\n\nVERDICT: FAIL"

	// the split: item 20's acceptance partitioned into four small items,
	// each a pull request one evaluation can check, as items 27-30 of
	// the record did
	ownerSplitPlan = "```json\n{\"items\": [" +
		"{\"seq\": 27, \"title\": \"Owner column and RegisterNode pins the id\", \"brief\": \"The owner column and the id.\", \"acceptance\": \"go test ./internal/store/... passes\", \"depends_on\": [], \"repo\": \"acme/widgets\"}," +
		"{\"seq\": 28, \"title\": \"TransferNode policy\", \"brief\": \"The transfer command's policy.\", \"acceptance\": \"go test ./internal/policy/... passes\", \"depends_on\": [{\"seq\": 27, \"because\": \"uses the owner column item 27 adds\"}], \"repo\": \"acme/widgets\"}," +
		"{\"seq\": 29, \"title\": \"ownsAs on the source only\", \"brief\": \"Fix the transfer defect.\", \"acceptance\": \"go test ./internal/transfer/... passes\", \"depends_on\": [{\"seq\": 28, \"because\": \"uses TransferNode's policy item 28 adds\"}], \"repo\": \"acme/widgets\"}," +
		"{\"seq\": 30, \"title\": \"The gateway field\", \"brief\": \"Expose it to readers.\", \"acceptance\": \"go test ./gateway/... passes\", \"depends_on\": [{\"seq\": 29, \"because\": \"uses ownsAs on the source item 29 fixes\"}], \"repo\": \"acme/widgets\"}" +
		"]}\n```"

	// a fourth re-land: one new item at item 20's exact acceptance,
	// exactly as the narrow re-plans before it each answered
	ownerReLandPlan = "```json\n{\"items\": [{\"seq\": 21, \"title\": \"Re-land the owner model, again\", \"brief\": \"Again.\", \"acceptance\": \"" + ownerGoalAcceptance + "\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"
)

// seedOwnerGoal builds the fake as a-node-has-an-owner's record reads:
// item 1 failed after three evaluations naming the real defects; items
// 8 and 14 superseded at the same acceptance (the narrow re-plans that
// recreated it); item 20 failed at the same acceptance, lineage root 1
// attempt 3, its own two recorded evaluations the clauses it failed;
// and two dependents the re-plans cancelled with no error of their own.
func seedOwnerGoal(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := seed(t)
	delete(f.Goals, goalID)
	delete(f.WorkItems, item1)
	delete(f.WorkItems, item2)
	delete(f.GitHubTokens, item1)
	delete(f.GitHubTokens, item2)
	f.Goals[ownerGoalID] = map[string]any{"id": ownerGoalID, "orgId": orgID, "projectId": projID, "slug": "a-node-has-an-owner",
		"title":      "A node has an owner, a person or an enterprise, and serves that owner's orgs: one registration, visible on each org's Nodes page, handed over or shared by choice",
		"statement":  "A node registers to one owner, a person or an enterprise, and serves every org the platform derives from it.",
		"acceptance": "go test ./... passes", "budgetCents": 24000, "spentCents": 5938, "status": "planning"}
	f.WorkItems[ownerItem1] = map[string]any{"id": ownerItem1, "goalId": ownerGoalID, "orgId": orgID, "seq": 1, "title": ownerItem1Title,
		"brief": "The schema and the policy.", "acceptance": ownerGoalAcceptance, "repoId": repoID, "baseRef": "main",
		"status": "failed", "evaluation": ownerItem1Evaluation, "dependsOn": []string{}}
	f.WorkItems[ownerItem2] = map[string]any{"id": ownerItem2, "goalId": ownerGoalID, "orgId": orgID, "seq": 2,
		"title":      "The node's orgs are derived: NodeOrgIndex kept by process nodeOrgs, gateway nodes(orgId) and nodeOrgs(nodeId), and every node read answers across the derived orgs",
		"brief":      "Derive the node's orgs.",
		"acceptance": "go test ./... passes", "repoId": repoID, "baseRef": "main", "status": "cancelled", "dependsOn": []string{ownerItem1}}
	f.WorkItems[ownerItem3] = map[string]any{"id": ownerItem3, "goalId": ownerGoalID, "orgId": orgID, "seq": 3,
		"title":      "e2e: the goal's acceptance on the rig — person node follows membership, enterprise node follows attachment and shares, the Mini transfers with one row throughout",
		"brief":      "The rig e2e.",
		"acceptance": "go test ./e2e passes", "repoId": repoID, "baseRef": "main", "status": "cancelled", "dependsOn": []string{ownerItem1}}
	f.WorkItems[ownerItem8] = map[string]any{"id": ownerItem8, "goalId": ownerGoalID, "orgId": orgID, "seq": 8, "title": ownerItem8Title,
		"brief": "Retry of item 1.", "acceptance": ownerGoalAcceptance, "repoId": repoID, "baseRef": "main",
		"status": "superseded", "lastError": "evaluation failed 3 times", "dependsOn": []string{}}
	f.WorkItems[ownerItem14] = map[string]any{"id": ownerItem14, "goalId": ownerGoalID, "orgId": orgID, "seq": 14, "title": ownerItem14Title,
		"brief": "Re-land the owner model.", "acceptance": ownerGoalAcceptance, "repoId": repoID, "baseRef": "main",
		"status": "superseded", "lastError": "evaluation failed 3 times", "dependsOn": []string{}}
	f.WorkItems[ownerItem20] = map[string]any{"id": ownerItem20, "goalId": ownerGoalID, "orgId": orgID, "seq": 20, "title": ownerItem20Title,
		"brief": "Owner model in schema and aggregate.", "acceptance": ownerGoalAcceptance, "repoId": repoID, "baseRef": "main",
		"status": "failed", "lastError": "evaluation failed 3 times", "lineageRootSeq": 1, "lineageAttempt": 3, "dependsOn": []string{},
		"evaluations": []any{
			map[string]any{"attempt": 2, "notes": "VERDICT: FAIL", "failedClauses": []any{
				map[string]any{"clause": "transfersNode applies ownsAs to the source", "satisfy": "the source node's own policy rows are the ones ownsAs changes"},
			}},
			map[string]any{"attempt": 1, "notes": "VERDICT: FAIL", "failedClauses": []any{
				map[string]any{"clause": "RegisterNode pins the aggregate id to NodeID(owner, slug)", "satisfy": "a re-registration keeps NodeID(owner, slug), never the legacy per-org id"},
			}},
		},
	}
	f.Projects[projID]["repoIds"] = []string{repoID}
	return f
}

// plannerOn runs the planner on the owner-goal fixture's goal (whole, or
// narrow when replacing is set) with the harness's answers in turn.
func plannerOn(t *testing.T, f *gatewaytest.Fake, goal gateway.Goal, answers ...string) (*fakeHarness, *strings.Builder, error) {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: answers}
	var logged strings.Builder
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logged, "", 0)}
	err := pl.Plan(context.Background(), goal, scopesOf(cfg)[0])
	return h, &logged, err
}

// flaggedBullet is the "- …" line of prompt's "## Your last answer was
// flagged" section that names marker (an item's "item N"), failing the
// test if the section or that bullet is missing.
func flaggedBullet(t *testing.T, prompt, marker string) string {
	t.Helper()
	const header = "## Your last answer was flagged"
	i := strings.Index(prompt, header)
	if i < 0 {
		t.Fatalf("prompt has no %q section:\n%s", header, prompt)
	}
	section := prompt[i:]
	if j := strings.Index(section, "\n## "); j >= 0 {
		section = section[:j]
	}
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "- ") && strings.Contains(line, marker) {
			return line
		}
	}
	t.Fatalf("no flag bullet names %q in:\n%s", marker, section)
	return ""
}

// planGoalItems is the titles and acceptances PlanGoal was called with,
// requiring exactly one call.
func planGoalItems(t *testing.T, f *gatewaytest.Fake) []map[string]any {
	t.Helper()
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	var out []map[string]any
	for _, it := range calls[0].Input["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

// Replay A: the whole re-plan, as today's platform asks (no narrow
// mode yet on the platform side). The planner's first answer recreates
// item 20's acceptance as item 21; the node's check refuses it, naming
// lineage 1 and the split; the planner's second answer is the four
// small items, handed in with none at item 20's acceptance.
func TestReplayANodeHasAnOwnerEndsAtTheSplit(t *testing.T) {
	f := seedOwnerGoal(t)
	goal := gateway.Goal{ID: ownerGoalID, Slug: "a-node-has-an-owner", Title: "A node has an owner", ProjectID: projID, Status: "planning"}
	h, logged, err := plannerOn(t, f, goal, ownerReLandPlan, ownerSplitPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2 (one re-ask)", len(h.jobs))
	}
	// item 1's evaluation notes, quoted in "the items so far"
	for _, want := range []string{ownerItem1Title, "RegisterNode does not pin the aggregate id to NodeID(owner, slug)", "transfersNode applies ownsAs to the destination"} {
		if !strings.Contains(h.jobs[0].Prompt, want) {
			t.Fatalf("plan prompt lacks %q:\n%s", want, h.jobs[0].Prompt)
		}
	}
	// the first prompt carries no flagged section: nothing has been
	// rejected yet
	if strings.Contains(h.jobs[0].Prompt, "## Your last answer was flagged") {
		t.Fatalf("first prompt is already flagged:\n%s", h.jobs[0].Prompt)
	}
	// the re-ask's flagged section names item 21, lineage 1, and tells
	// the planner to split — the bullet itself, not the whole prompt
	// (planSplitRule's own "split" sits lower in every plan prompt, so
	// a whole-prompt search for "split" would pass even with the
	// bullet's own "split it: partition…" removed)
	reask := h.jobs[1].Prompt
	if !strings.Contains(reask, "## Your last answer was flagged") {
		t.Fatalf("re-ask has no flagged section:\n%s", reask)
	}
	bullet := flaggedBullet(t, reask, "item 21")
	for _, want := range []string{"item 21", "lineage 1", "split"} {
		if !strings.Contains(bullet, want) {
			t.Fatalf("the flag bullet for item 21 does not name %q:\n%s", want, bullet)
		}
	}
	if !strings.Contains(logged.String(), "lineage 1") {
		t.Fatalf("the re-ask's flag is not logged:\n%s", logged)
	}

	items := planGoalItems(t, f)
	if len(items) != 4 {
		t.Fatalf("handed in %d items, want 4: %v", len(items), items)
	}
	failedAcc := normaliseAcceptance(ownerGoalAcceptance)
	for _, it := range items {
		if normaliseAcceptance(it["acceptance"].(string)) == failedAcc {
			t.Fatalf("an item at item 20's acceptance was handed in: %v", it)
		}
	}
}

// Replay B: the narrow mode, replacingItemId = item 20. The same first
// answer is refused and re-asked; the second is handed in as the
// replacement alone.
func TestReplayANodeHasAnOwnerNarrow(t *testing.T) {
	f := seedOwnerGoal(t)
	f.Goals[ownerGoalID]["replacingItemId"] = ownerItem20
	goal := gateway.Goal{ID: ownerGoalID, Slug: "a-node-has-an-owner", Title: "A node has an owner", ProjectID: projID, Status: "planning",
		ReplacingItemID: ownerItem20, NarrowReplans: 1}
	h, logged, err := plannerOn(t, f, goal, ownerReLandPlan, ownerSplitPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2 (one re-ask)", len(h.jobs))
	}
	for _, want := range []string{
		"## Split this item",
		"your plan replaces item 20 alone",
		"It is lineage 1, attempt 3 of 3.",
		"Its acceptance:\n\n" + ownerGoalAcceptance,
		"RegisterNode pins the aggregate id to NodeID(owner, slug)",
		"transfersNode applies ownsAs to the source",
	} {
		if !strings.Contains(h.jobs[0].Prompt, want) {
			t.Fatalf("plan prompt lacks %q:\n%s", want, h.jobs[0].Prompt)
		}
		if !strings.Contains(h.jobs[1].Prompt, want) {
			t.Fatalf("re-ask lacks %q:\n%s", want, h.jobs[1].Prompt)
		}
	}
	if !strings.Contains(logged.String(), "narrow re-plan replacing item 20 (lineage 1, attempt 3 of 3): asking for a split") {
		t.Fatalf("log lacks the narrow re-plan line:\n%s", logged)
	}

	items := planGoalItems(t, f)
	if len(items) != 4 {
		t.Fatalf("handed in %d items, want the 4 replacements only: %v", len(items), items)
	}
	wantTitles := map[string]bool{"Owner column and RegisterNode pins the id": true, "TransferNode policy": true, "ownsAs on the source only": true, "The gateway field": true}
	for _, it := range items {
		title := it["title"].(string)
		if !wantTitles[title] {
			t.Fatalf("handed in item %q, not one of the replacements", title)
		}
		delete(wantTitles, title)
	}
	if len(wantTitles) != 0 {
		t.Fatalf("missing replacements: %v", wantTitles)
	}
}

// Replay C: a planner that answers the same acceptance twice. PlanGoal
// is never called, and the error names lineage 1: the goal gets no
// fourth re-land from this node.
func TestReplayANodeHasAnOwnerRefusesAFourthReLand(t *testing.T) {
	f := seedOwnerGoal(t)
	goal := gateway.Goal{ID: ownerGoalID, Slug: "a-node-has-an-owner", Title: "A node has an owner", ProjectID: projID, Status: "planning"}
	h, logged, err := plannerOn(t, f, goal, ownerReLandPlan, ownerReLandPlan)
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	if err == nil || !strings.Contains(err.Error(), "lineage 1") {
		t.Fatalf("err = %v, want a refusal naming lineage 1", err)
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 0 {
		t.Fatalf("a fourth re-land was handed in: %+v", calls)
	}
	if !strings.Contains(logged.String(), "lineage 1") {
		t.Fatalf("the re-ask's flag is not logged:\n%s", logged)
	}
}
