package gateway_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A plan clones with a token the platform issues for it, the same way a
// build does (adr-022): planGitHubToken(goalId) answers one row per repo
// enrolled in the goal's project, minted from each repo's own GitHub App
// installation.
func planGoalID() string { return "eeeeeeee-5555-4555-8555-eeeeeeeeeeee" }

func setUpTwoRepoProject(f *gatewaytest.Fake) (acctA, acctB, repoA, repoB string) {
	acctA, acctB = "11111111-aaaa-4aaa-8aaa-111111111111", "22222222-bbbb-4bbb-8bbb-222222222222"
	repoA, repoB = "33333333-cccc-4ccc-8ccc-333333333333", "44444444-dddd-4ddd-8ddd-444444444444"
	f.Accounts[acctA] = map[string]any{"id": acctA, "login": "runsheet"}
	f.Accounts[acctB] = map[string]any{"id": acctB, "login": "go-apis"}
	f.Repos[repoA] = map[string]any{"id": repoA, "accountId": acctA, "name": "mesh-cli", "defaultBranch": "main"}
	f.Repos[repoB] = map[string]any{"id": repoB, "accountId": acctB, "name": "loom", "defaultBranch": "main"}
	f.Projects[projID] = map[string]any{"id": projID, "orgId": orgID, "repoIds": []string{repoA, repoB}}
	return
}

func planTokenRow(token, expiresAt, repository string) map[string]any {
	return map[string]any{"token": token, "expiresAt": expiresAt, "repository": repository}
}

// The node holding a goal's plan claim reads planGitHubToken(goalId) and
// decodes every row — one per repo of the goal's project — whole.
func TestPlanGitHubTokensDecodesTwoRows(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	_, _, _, _ = setUpTwoRepoProject(f)
	goalID := planGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID, "plannerNodeId": nodeID}
	f.PlanGitHubTokens[goalID] = []map[string]any{
		planTokenRow("ghs_mesh", "2026-10-03T04:27:21Z", "runsheet/mesh-cli"),
		planTokenRow("ghs_loom", "2026-10-03T05:00:00Z", "go-apis/loom"),
	}
	gw := gateway.New(f.URL(), "rs_test")

	got, err := gw.PlanGitHubTokens(context.Background(), goalID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	want := []gateway.GitHubToken{
		{Token: "ghs_mesh", ExpiresAt: time.Date(2026, 10, 3, 4, 27, 21, 0, time.UTC), Repository: "runsheet/mesh-cli"},
		{Token: "ghs_loom", ExpiresAt: time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC), Repository: "go-apis/loom"},
	}
	if len(got) != len(want) {
		t.Fatalf("tokens = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Token != want[i].Token || !got[i].ExpiresAt.Equal(want[i].ExpiresAt) || got[i].Repository != want[i].Repository {
			t.Errorf("token %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	calls := f.CallsNamed("planGitHubToken")
	if len(calls) != 1 || calls[0].Input["goalId"] != goalID {
		t.Fatalf("calls = %+v", calls)
	}
}

// researchGitHubToken(directionId) is PlanGitHubTokens keyed by
// direction, for a research session's own fetch.
func TestResearchGitHubTokensKeyedByDirection(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	_, _, _, _ = setUpTwoRepoProject(f)
	directionID := "ffffffff-6666-4666-8666-ffffffffffff"
	f.Directions[directionID] = map[string]any{"id": directionID, "projectId": projID, "researchNodeId": nodeID}
	f.ResearchGitHubTokens[directionID] = []map[string]any{
		planTokenRow("ghs_mesh2", "2026-10-03T04:27:21Z", "runsheet/mesh-cli"),
		planTokenRow("ghs_loom2", "2026-10-03T05:00:00Z", "go-apis/loom"),
	}
	gw := gateway.New(f.URL(), "rs_test")

	got, err := gw.ResearchGitHubTokens(context.Background(), directionID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Repository != "runsheet/mesh-cli" || got[1].Repository != "go-apis/loom" {
		t.Fatalf("tokens = %+v", got)
	}
	calls := f.CallsNamed("researchGitHubToken")
	if len(calls) != 1 || calls[0].Input["directionId"] != directionID {
		t.Fatalf("calls = %+v", calls)
	}
}

// A platform without the field is told apart from one that refuses, as
// IssueItemRepoToken's: the node's cue to fall back to the operator's
// own gh login for the fetch.
func TestPlanGitHubTokensOldPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	goalID := planGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID, "plannerNodeId": nodeID}
	f.Unsupported["planGitHubToken"] = true
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.PlanGitHubTokens(context.Background(), goalID, nodeID)
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

func TestResearchGitHubTokensOldPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	directionID := "ffffffff-7777-4777-8777-ffffffffffff"
	f.Directions[directionID] = map[string]any{"id": directionID, "projectId": projID, "researchNodeId": nodeID}
	f.Unsupported["researchGitHubToken"] = true
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.ResearchGitHubTokens(context.Background(), directionID, nodeID)
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

// A project repo with no row among the goal's tokens is refused by
// name, not answered as a partial list.
func TestPlanGitHubTokensMissingRepoIsRefusedByName(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	_, _, _, _ = setUpTwoRepoProject(f)
	goalID := planGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID, "plannerNodeId": nodeID}
	// only one of the project's two repos has a row: no installation
	// reaches the other
	f.PlanGitHubTokens[goalID] = []map[string]any{
		planTokenRow("ghs_mesh", "2026-10-03T04:27:21Z", "runsheet/mesh-cli"),
	}
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.PlanGitHubTokens(context.Background(), goalID, nodeID)
	if !gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want a refusal that is not unsupported", err)
	}
	if !strings.Contains(err.Error(), "go-apis/loom") {
		t.Fatalf("err = %v, want it to name go-apis/loom", err)
	}
}

// Only the node holding the goal's plan claim is handed its tokens.
func TestPlanGitHubTokensRefusesANodeWithoutTheClaim(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	_, _, _, _ = setUpTwoRepoProject(f)
	goalID := planGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID, "plannerNodeId": "someone-else"}
	f.PlanGitHubTokens[goalID] = []map[string]any{
		planTokenRow("ghs_mesh", "2026-10-03T04:27:21Z", "runsheet/mesh-cli"),
		planTokenRow("ghs_loom", "2026-10-03T05:00:00Z", "go-apis/loom"),
	}
	f.TokenNodeID = nodeID
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.PlanGitHubTokens(context.Background(), goalID, nodeID)
	if !gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want a refusal that is not unsupported", err)
	}
}

// specs/contracts/planGitHubToken.json is the contract, not prose:
// served from the fake, the client parses every row it declares back
// out, byte for byte.
func TestPlanGitHubTokenAnswerFromFixture(t *testing.T) {
	raw, err := os.ReadFile("../../specs/contracts/planGitHubToken.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Description string           `json:"description"`
		Input       map[string]any   `json:"input"`
		Answer      []map[string]any `json:"answer"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.Description == "" {
		t.Fatal("planGitHubToken.json: no description")
	}
	if len(fx.Answer) < 2 {
		t.Fatalf("planGitHubToken.json: answer has %d rows, want at least 2", len(fx.Answer))
	}

	f := gatewaytest.New("rs_test")
	defer f.Close()
	goalID, _ := fx.Input["goalId"].(string)
	f.Goals[goalID] = map[string]any{"id": goalID, "plannerNodeId": nodeID}
	f.PlanGitHubTokens[goalID] = fx.Answer
	gw := gateway.New(f.URL(), "rs_test")

	got, err := gw.PlanGitHubTokens(context.Background(), goalID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(fx.Answer) {
		t.Fatalf("tokens = %+v, want %d rows", got, len(fx.Answer))
	}
	for i, row := range fx.Answer {
		wantExp, err := time.Parse(time.RFC3339Nano, row["expiresAt"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if got[i].Token != row["token"] || !got[i].ExpiresAt.Equal(wantExp) || got[i].Repository != row["repository"] {
			t.Errorf("token %d = %+v, want token %v, expiresAt %v, repository %v", i, got[i], row["token"], row["expiresAt"], row["repository"])
		}
	}
}

// Only the node holding the direction's research claim is handed its
// tokens.
func TestResearchGitHubTokensRefusesANodeWithoutTheClaim(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	_, _, _, _ = setUpTwoRepoProject(f)
	directionID := "ffffffff-8888-4888-8888-ffffffffffff"
	f.Directions[directionID] = map[string]any{"id": directionID, "projectId": projID, "researchNodeId": "someone-else"}
	f.ResearchGitHubTokens[directionID] = []map[string]any{
		planTokenRow("ghs_mesh", "2026-10-03T04:27:21Z", "runsheet/mesh-cli"),
		planTokenRow("ghs_loom", "2026-10-03T05:00:00Z", "go-apis/loom"),
	}
	f.TokenNodeID = nodeID
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.ResearchGitHubTokens(context.Background(), directionID, nodeID)
	if !gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want a refusal that is not unsupported", err)
	}
}
