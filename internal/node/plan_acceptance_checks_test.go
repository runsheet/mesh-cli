package node

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// runsheet ADR 056: the platform refuses a plan any of whose items has
// no acceptance_checks. The node's plan check flags such an item before
// the hand-in and asks the planner again; the second answer's checks are
// sent; a second answer still without one gets one observable made of
// its prose, so the plan is never refused for the list alone.

const checksFirst = "```json\n" + `{"items": [
  {"seq": 1, "title": "Write the failing test", "brief": "A test.", "acceptance": "` + "`go test ./internal/widgets/`" + ` fails on the new case.", "depends_on": [], "repo": "acme/widgets", "base_ref": "main"},
  {"seq": 2, "title": "Make it pass", "brief": "Implement.", "acceptance": "` + "`go test ./internal/widgets/`" + ` passes.", "depends_on": [{"seq": 1, "because": "makes item 1's test pass"}], "repo": "acme/widgets", "base_ref": "main"}
]}` + "\n```"

const checksSecond = "```json\n" + `{"items": [
  {"seq": 1, "title": "Write the failing test", "brief": "A test.", "acceptance": "` + "`go test ./internal/widgets/`" + ` fails on the new case.", "acceptance_checks": [{"kind": "Test", "check": " go test ./internal/widgets/ ", "expected": "fails on TestNewCase"}], "depends_on": [], "repo": "acme/widgets", "base_ref": "main"},
  {"seq": 2, "title": "Make it pass", "brief": "Implement.", "acceptance": "` + "`go test ./internal/widgets/`" + ` passes.", "acceptance_checks": [{"kind": "test", "check": "go test ./internal/widgets/", "expected": "passes"}, {"kind": "command", "check": "go vet ./...", "expected": "exit 0"}], "depends_on": [{"seq": 1, "because": "makes item 1's test pass"}], "repo": "acme/widgets", "base_ref": "main"}
]}` + "\n```"

// planChecks runs the planner on a platform that takes acceptanceChecks
// with the answers given, in turn; it returns the fake, the harness and
// node.log.
func planChecks(t *testing.T, answers ...string) (*gatewaytest.Fake, *fakeHarness, string) {
	t.Helper()
	f := seed(t)
	f.ItemAcceptanceChecks = true
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "44444444-4444-4444-8444-444444444444", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: answers}
	var logs bytes.Buffer
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	goal := gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}
	if err := pl.Plan(context.Background(), goal, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	return f, h, logs.String()
}

func plannedChecks(t *testing.T, f *gatewaytest.Fake, item int) []map[string]any {
	t.Helper()
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	items, _ := calls[0].Input["items"].([]any)
	if len(items) <= item {
		t.Fatalf("items: %+v", items)
	}
	row, _ := items[item].(map[string]any)
	list, _ := row[gateway.ItemAcceptanceChecksField].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		m, _ := c.(map[string]any)
		out = append(out, m)
	}
	return out
}

func TestPlannerReasksForAcceptanceChecksAndSendsThem(t *testing.T) {
	f, h, logs := planChecks(t, checksFirst, checksSecond)
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	re := h.jobs[1].Prompt
	for _, want := range []string{
		"## Your last answer was flagged",
		"item 1 (Write the failing test): no acceptance_checks: each item carries at least one {kind, check, expected}",
		"item 2 (Make it pass): no acceptance_checks",
		"`acceptance_checks`: at least one",
	} {
		if !strings.Contains(re, want) {
			t.Errorf("the re-ask lacks %q:\n%s", want, re)
		}
	}
	if !strings.Contains(logs, "asking again: item 1 (Write the failing test): no acceptance_checks") {
		t.Errorf("the flag is not logged:\n%s", logs)
	}
	one := plannedChecks(t, f, 0)
	if len(one) != 1 || one[0]["kind"] != "test" || one[0]["check"] != "go test ./internal/widgets/" || one[0]["expected"] != "fails on TestNewCase" {
		t.Fatalf("item 1's checks = %v (kind lower-cased, check trimmed)", one)
	}
	two := plannedChecks(t, f, 1)
	if len(two) != 2 || two[1]["kind"] != "command" || two[1]["check"] != "go vet ./..." {
		t.Fatalf("item 2's checks = %v", two)
	}
	if f.Goals[goalID]["status"] != "awaiting_approval" {
		t.Fatalf("goal is %v", f.Goals[goalID]["status"])
	}
}

// The second answer still names no check: the item is handed in with one
// observable made of its prose, said in the log, rather than refused.
func TestPlannerMakesAnObservableOfTheProseOnTheFinalAnswer(t *testing.T) {
	f, h, logs := planChecks(t, checksFirst, checksFirst)
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	one := plannedChecks(t, f, 0)
	if len(one) != 1 || one[0]["kind"] != "observable" || one[0]["check"] != "`go test ./internal/widgets/` fails on the new case." || one[0]["expected"] == "" {
		t.Fatalf("item 1's checks = %v", one)
	}
	if !strings.Contains(logs, `item 1 "Write the failing test": no acceptance_checks on the final answer; one observable made of its prose`) {
		t.Errorf("the fallback is not said:\n%s", logs)
	}
	if f.Goals[goalID]["status"] != "awaiting_approval" {
		t.Fatalf("goal is %v", f.Goals[goalID]["status"])
	}
}

// A check with a kind the platform does not know, or no expected, is
// flagged by name on the first answer and dropped from the hand-in.
func TestMissingAcceptanceChecksNamesTheBadCheck(t *testing.T) {
	specs, err := planOf("```json\n" + `{"items": [
  {"seq": 1, "title": "A", "brief": "a", "acceptance": "a", "acceptance_checks": [{"kind": "manual", "check": "look", "expected": "fine"}], "depends_on": [], "repo": "acme/widgets"},
  {"seq": 2, "title": "B", "brief": "b", "acceptance": "b", "acceptance_checks": [{"kind": "test", "check": "go test ./...", "expected": " "}], "depends_on": [], "repo": "acme/widgets"},
  {"seq": 3, "title": "C", "brief": "c", "acceptance": "c", "acceptance_checks": [{"kind": "command", "check": "", "expected": "0"}, {"kind": "observable", "check": "the doc", "expected": "has a section"}], "depends_on": [], "repo": "acme/widgets"}
]}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	flags := missingAcceptanceChecks(specs)
	want := []string{
		`item 1 (A): acceptance_checks[0]: kind "manual" is not one of command, test, observable`,
		`item 2 (B): acceptance_checks[0] (test) has no expected result`,
		`item 3 (C): acceptance_checks[0] (command) has no check`,
	}
	if strings.Join(flags, "\n") != strings.Join(want, "\n") {
		t.Fatalf("flags:\n%s\nwant:\n%s", strings.Join(flags, "\n"), strings.Join(want, "\n"))
	}
	if got := acceptanceChecksOf(specs[2]); len(got) != 1 || got[0].Kind != "observable" {
		t.Fatalf("item 3's sendable checks = %+v", got)
	}
	if got := acceptanceChecksOf(specs[0]); len(got) != 0 {
		t.Fatalf("item 1's sendable checks = %+v", got)
	}
}
