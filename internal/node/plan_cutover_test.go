package node

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// release-tag-mirrors item 14 as it was planned: the cleanup after the
// flip, gated on a STEP 0 reading and on the operator deleting the
// secret — a gate no builder could open, failed three times.
const (
	mirrorsItem14Title = "The cleanup after the flip: scripts/mirror-token.sh, release-tag.yml and the docs run on the exchanged token alone, RELEASE_TOKEN gone"
	mirrorsItem14Gate  = "A session whose STEP 0 reading is not `exchanged`/`absent` changed no file and ended saying the gate is closed with the output quoted"
	mirrorsItem14Owner = "the operator deletes the secret so it reads `secret: absent`"
	mirrorsItem14Acc   = "`bash scripts/mirror-token.sh --check` exits 0 on the exchanged token; " + mirrorsItem14Gate + "; " + mirrorsItem14Owner + "."
)

// ADR-023 (c): release-tag-mirrors item 14's acceptance is flagged,
// naming each sentence that waits on a person or production; a clause a
// pull request can satisfy is not.
func TestCutOverAcceptanceReleaseTagMirrorsItem14(t *testing.T) {
	flags := cutOverAcceptance([]harness.PlannedSpec{{Seq: 14, Title: mirrorsItem14Title, Acceptance: mirrorsItem14Acc}})
	if len(flags) != 1 {
		t.Fatalf("flags = %v, want 1", flags)
	}
	want := "item 14 (" + mirrorsItem14Title + `): its acceptance waits on a person or production ("` + mirrorsItem14Gate + `", "` + mirrorsItem14Owner + `"), a gate no builder can open; make it ` + "`precondition`" + ` on the item that needs it, or a ` + "`pr`" + ` checkpoint on the goal`
	if flags[0] != want {
		t.Fatalf("flag =\n%s\nwant\n%s", flags[0], want)
	}
	if strings.Contains(flags[0], "mirror-token.sh --check") {
		t.Errorf("the checkable sentence is named: %s", flags[0])
	}
}

// Each pattern flags; `go test ./... passes` and a Measure: item do not.
func TestCutOverAcceptancePatterns(t *testing.T) {
	for _, acc := range []string{
		"`go test ./... passes`",
		"go test ./... passes; the README names the flag.",
		"the runner reads the token from the environment.",
	} {
		if flags := cutOverAcceptance([]harness.PlannedSpec{{Seq: 1, Title: "T", Acceptance: acc}}); len(flags) != 0 {
			t.Errorf("%q flagged: %v", acc, flags)
		}
	}
	for _, acc := range []string{
		"a person confirms the mirror is live.",
		"the owner approves the release.",
		"it is seen in production.",
		"once the release is cut the mirror updates.",
		"a v* run reads exchanged.",
		"a live run shows the new token.",
		"the gate is open.",
		"after the platform half lands the field is sent.",
	} {
		if flags := cutOverAcceptance([]harness.PlannedSpec{{Seq: 1, Title: "T", Acceptance: acc}}); len(flags) != 1 {
			t.Errorf("%q not flagged", acc)
		}
	}
	if flags := cutOverAcceptance([]harness.PlannedSpec{{Seq: 1, Title: "Measure: the mirror's first live run", Acceptance: "a live run shows the new token, recorded."}}); len(flags) != 0 {
		t.Errorf("a Measure: item flagged: %v", flags)
	}
}

// The precondition line rides beside atomic's: both round-trip, atomic
// first.
func TestBriefWithPreconditionRoundTrips(t *testing.T) {
	clause := "a v* run reads exchanged and the operator has deleted RELEASE_TOKEN"
	b := gateway.BriefWithPrecondition("Drop RELEASE_TOKEN.", clause)
	if b != "precondition: "+clause+"\n\nDrop RELEASE_TOKEN." {
		t.Fatalf("brief = %q", b)
	}
	if got := gateway.PreconditionOf(b); got != clause {
		t.Fatalf("PreconditionOf = %q", got)
	}
	both := gateway.BriefWithAtomic(b, "one workflow file")
	if gateway.AtomicReason(both) != "one workflow file" || gateway.PreconditionOf(both) != clause {
		t.Fatalf("atomic %q, precondition %q from %q", gateway.AtomicReason(both), gateway.PreconditionOf(both), both)
	}
	if gateway.PreconditionOf("Drop RELEASE_TOKEN.") != "" || gateway.BriefWithPrecondition("x", " ") != "x" {
		t.Error("an empty clause is not left out")
	}
}

const cutOverFirst = "```json\n" + `{"items": [
  {"seq": 1, "title": "mirror-token.sh exchanges the token", "brief": "Exchange.", "acceptance": "` + "`go test ./...`" + ` passes; scripts/mirror-token.sh exchanges the token.", "depends_on": [], "repo": "acme/widgets", "base_ref": "main"},
  {"seq": 2, "title": "` + mirrorsItem14Title + `", "brief": "Clean up.", "acceptance": "` + "`bash scripts/mirror-token.sh --check` exits 0 on the exchanged token; A session whose STEP 0 reading is not `exchanged`/`absent` changed no file and ended saying the gate is closed with the output quoted; the operator deletes the secret so it reads `secret: absent`." + `", "depends_on": [{"seq": 1, "because": "runs on the token item 1 exchanges"}], "repo": "acme/widgets", "base_ref": "main"}
]}` + "\n```"

const cutOverClause = "a v* run reads exchanged and the operator has deleted RELEASE_TOKEN"

const cutOverSecond = "```json\n" + `{"items": [
  {"seq": 1, "title": "mirror-token.sh exchanges the token", "brief": "Exchange.", "acceptance": "` + "`go test ./...`" + ` passes; scripts/mirror-token.sh exchanges the token.", "depends_on": [], "repo": "acme/widgets", "base_ref": "main"},
  {"seq": 2, "title": "` + mirrorsItem14Title + `", "brief": "Clean up.", "acceptance": "` + "`bash scripts/mirror-token.sh --check`" + ` exits 0 on the exchanged token alone and release-tag.yml names no RELEASE_TOKEN.", "precondition": "` + cutOverClause + `", "depends_on": [{"seq": 1, "because": "runs on the token item 1 exchanges"}], "repo": "acme/widgets", "base_ref": "main"}
], "checkpoints": ["pr"]}` + "\n```"

// planCutOver runs the planner on the release-tag-mirrors answers, on a
// platform whose PlannedItemInput takes precondition or not, for a goal
// with the checkpoints given; it returns node.log too.
func planCutOver(t *testing.T, takesPrecondition bool, checkpoints []string, answers ...string) (*gatewaytest.Fake, *fakeHarness, string) {
	t.Helper()
	f := seed(t)
	f.ItemPrecondition = takesPrecondition
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "44444444-4444-4444-8444-444444444444", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: answers}
	var logs bytes.Buffer
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	goal := gateway.Goal{ID: goalID, Slug: "release-tag-mirrors", Title: "Release tags mirror", ProjectID: projID, Status: "planning", Checkpoints: checkpoints}
	if err := pl.Plan(context.Background(), goal, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	return f, h, logs.String()
}

// mutationIndex is the position of the first recorded mutation named name,
// or -1.
func mutationIndex(f *gatewaytest.Fake, name string) int {
	for i, c := range f.Calls {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// A first answer holding item 14 is re-asked with the flag and
// planCutOverRule; the second's precondition rides the brief and, on a
// platform that takes it, the item's field, and its `pr` checkpoint is
// set on the goal before planGoal.
func TestPlannerCutOverReasksAndHandsInPrecondition(t *testing.T) {
	f, h, logs := planCutOver(t, true, nil, cutOverFirst, cutOverSecond)
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	re := h.jobs[1].Prompt
	for _, want := range []string{
		"## Your last answer was flagged",
		"item 2 (" + mirrorsItem14Title + "): its acceptance waits on a person or production",
		"STEP 0 reading",
		"the operator deletes the secret",
		// planCutOverRule's own text
		"A clause that waits on a person's confirmation or a production observation is never an item",
	} {
		if !strings.Contains(re, want) {
			t.Fatalf("re-ask lacks %q:\n%s", want, re)
		}
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	items := calls[0].Input["items"].([]any)
	cleanup := items[1].(map[string]any)
	brief, _ := cleanup["brief"].(string)
	if !strings.HasPrefix(brief, "precondition: "+cutOverClause+"\n\n") || gateway.PreconditionOf(brief) != cutOverClause {
		t.Fatalf("brief = %q", brief)
	}
	if cleanup[gateway.ItemPreconditionField] != cutOverClause {
		t.Fatalf("precondition field = %#v", cleanup[gateway.ItemPreconditionField])
	}
	if _, sent := items[0].(map[string]any)[gateway.ItemPreconditionField]; sent {
		t.Errorf("an item without one sent precondition: %v", items[0])
	}
	cps := f.CallsNamed("setGoalCheckpoints")
	if len(cps) != 1 {
		t.Fatalf("setGoalCheckpoints calls: %+v", cps)
	}
	if got, _ := cps[0].Input["checkpoints"].([]any); len(got) != 1 || got[0] != "pr" {
		t.Fatalf("checkpoints = %#v", cps[0].Input["checkpoints"])
	}
	if i, j := mutationIndex(f, "setGoalCheckpoints"), mutationIndex(f, "planGoal"); i < 0 || i > j {
		t.Fatalf("setGoalCheckpoints at %d, planGoal at %d", i, j)
	}
	if !strings.Contains(logs, "goal release-tag-mirrors: pr checkpoint set: a person approves each pull request") {
		t.Errorf("log lacks the checkpoint line:\n%s", logs)
	}
}

// A platform whose PlannedItemInput has no precondition gets the clause
// on the brief alone; a goal that already has pr is not set again.
func TestPlannerPreconditionOnlyWhereTaken(t *testing.T) {
	f, _, _ := planCutOver(t, false, []string{"graph", "pr"}, cutOverSecond)
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	cleanup := calls[0].Input["items"].([]any)[1].(map[string]any)
	if _, sent := cleanup[gateway.ItemPreconditionField]; sent {
		t.Fatalf("precondition sent to a platform without it: %v", cleanup)
	}
	if gateway.PreconditionOf(cleanup["brief"].(string)) != cutOverClause {
		t.Fatalf("brief = %q", cleanup["brief"])
	}
	if cps := f.CallsNamed("setGoalCheckpoints"); len(cps) != 0 {
		t.Fatalf("setGoalCheckpoints on a goal that has pr: %+v", cps)
	}
}

// A goal with other checkpoints keeps them beside pr; an unknown value
// is said and dropped.
func TestPlannerCutOverPRCheckpointKeepsTheGoalsOwn(t *testing.T) {
	answer := strings.Replace(cutOverSecond, `"checkpoints": ["pr"]`, `"checkpoints": ["pr", "deploy"]`, 1)
	f, _, logs := planCutOver(t, true, []string{"graph"}, answer)
	cps := f.CallsNamed("setGoalCheckpoints")
	if len(cps) != 1 {
		t.Fatalf("setGoalCheckpoints calls: %+v", cps)
	}
	if got, _ := cps[0].Input["checkpoints"].([]any); len(got) != 2 || got[0] != "graph" || got[1] != "pr" {
		t.Fatalf("checkpoints = %#v", cps[0].Input["checkpoints"])
	}
	if !strings.Contains(logs, `plan checkpoint "deploy" is not one the node sets (pr); dropped`) {
		t.Errorf("the unknown checkpoint is not said:\n%s", logs)
	}
}
