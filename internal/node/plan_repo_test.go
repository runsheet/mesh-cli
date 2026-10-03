package node

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// ADR-023 (d), planRepoRule: an item naming a repo other than the
// goal's own is a different goal.

const (
	// an item naming the project's other repo, not the goal's own
	otherGoalRepoPlan = "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Console route\", \"brief\": \"The route.\", \"acceptance\": \"go test ./... passes\", \"depends_on\": [], \"repo\": \"acme/mesh-specs\"}]}\n```"
	// the same item, on the goal's own repo
	ownGoalRepoPlan = "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Console route\", \"brief\": \"The route.\", \"acceptance\": \"go test ./... passes\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"
)

// planGoalRepoOnce runs the planner on the seeded goal, in a project of
// two repos (widgets and mesh-specs), the goal's own repoId set to
// repoID (widgets).
func planGoalRepoOnce(t *testing.T, repoIDOnGoal string, projectRepos []string, answers ...string) (*gatewaytest.Fake, *fakeHarness, error) {
	t.Helper()
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Repos[repo2] = map[string]any{"id": repo2, "accountId": acctID, "name": "mesh-specs", "defaultBranch": "main"}
	f.Projects[projID]["repoIds"] = projectRepos
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: answers}
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning", RepoID: repoIDOnGoal}, scopesOf(cfg)[0])
	return f, h, err
}

// A plan whose item names the project's other repo, not the goal's own
// (repoId mesh-cli, here "widgets"), is flagged; the planner is asked
// once more, told it is a different goal, and only its clean second
// plan is handed in.
func TestPlannerReasksOnADifferentGoalRepo(t *testing.T) {
	f, h, err := planGoalRepoOnce(t, repoID, []string{repoID, repo2}, otherGoalRepoPlan, ownGoalRepoPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	if strings.Contains(h.jobs[0].Prompt, "Your last answer") {
		t.Fatal("the first ask flags nothing")
	}
	for _, want := range []string{
		"item 1 (Console route): names repo acme/mesh-specs, not the goal's acme/widgets — a different goal: leave it out, or make it a draft of a split",
		"An item naming a repo other than the goal's repo is a different goal",
	} {
		if !strings.Contains(h.jobs[1].Prompt, want) {
			t.Fatalf("re-ask lacks %q:\n%s", want, h.jobs[1].Prompt)
		}
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	items := calls[0].Input["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["repoId"] != repoID {
		t.Fatalf("handed in: %v, want the clean second plan on the goal's repo", items)
	}
}

// A second answer that still names the other repo is refused outright,
// like a re-land: nothing is handed in.
func TestPlannerRefusesASecondDifferentGoalRepo(t *testing.T) {
	f, h, err := planGoalRepoOnce(t, repoID, []string{repoID, repo2}, otherGoalRepoPlan, otherGoalRepoPlan)
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	if err == nil || !strings.Contains(err.Error(), "a different goal") {
		t.Fatalf("err = %v, want it to name 'a different goal'", err)
	}
	if !strings.Contains(err.Error(), "acme/mesh-specs") || !strings.Contains(err.Error(), "acme/widgets") {
		t.Fatalf("err = %v, want both repos named", err)
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 0 {
		t.Fatalf("a plan outside the goal's repo was handed in: %+v", calls)
	}
}

// goalRepo (ADR-023 (d)): the goal's own repoId among the project's
// repos when the platform names one, else the project's one repo when
// it has exactly one, else none — the rule does not apply.
func TestGoalRepoOtherRepo(t *testing.T) {
	widgets := gateway.Repo{ID: repoID, Name: "widgets", Owner: "acme"}
	specs := gateway.Repo{ID: repo2, Name: "mesh-specs", Owner: "acme"}
	repos := map[string]gateway.Repo{"acme/widgets": widgets, "acme/mesh-specs": specs}

	t.Run("the goal's own repoId", func(t *testing.T) {
		goal := gateway.Goal{RepoID: repoID}
		project := &gateway.Project{RepoIDs: []string{repoID, repo2}}
		got, ok := goalRepo(goal, project, repos)
		if !ok || got.ID != repoID {
			t.Fatalf("goalRepo = %+v, %v; want widgets, true", got, ok)
		}
	})

	t.Run("falls back to the project's one repo", func(t *testing.T) {
		goal := gateway.Goal{}
		project := &gateway.Project{RepoIDs: []string{repo2}}
		got, ok := goalRepo(goal, project, repos)
		if !ok || got.ID != repo2 {
			t.Fatalf("goalRepo = %+v, %v; want mesh-specs, true", got, ok)
		}
	})

	t.Run("no repoId and more than one project repo: none", func(t *testing.T) {
		goal := gateway.Goal{}
		project := &gateway.Project{RepoIDs: []string{repoID, repo2}}
		if _, ok := goalRepo(goal, project, repos); ok {
			t.Fatal("goalRepo resolved one; want none, the rule does not apply")
		}
	})
}

// End to end: a goal with no repoId on a one-repo project is checked
// against that one repo (an item on it is clean), and on a two-repo
// project is not checked at all (an item on the other repo is not
// flagged as a different goal).
func TestPlannerOtherRepoRuleAppliesOnlyWhenAGoalRepoResolves(t *testing.T) {
	t.Run("one repo project, no repoId: that repo is the goal's", func(t *testing.T) {
		f, h, err := planGoalRepoOnce(t, "", []string{repoID}, ownGoalRepoPlan)
		if err != nil {
			t.Fatal(err)
		}
		if len(h.jobs) != 1 {
			t.Fatalf("planner sessions = %d, want 1 (clean)", len(h.jobs))
		}
		if len(f.CallsNamed("planGoal")) != 1 {
			t.Fatal("the clean plan is handed in")
		}
	})

	t.Run("two repo project, no repoId: not flagged", func(t *testing.T) {
		f, h, err := planGoalRepoOnce(t, "", []string{repoID, repo2}, otherGoalRepoPlan)
		if err != nil {
			t.Fatal(err)
		}
		if len(h.jobs) != 1 {
			t.Fatalf("planner sessions = %d, want 1 (no re-ask: the repo rule does not apply)", len(h.jobs))
		}
		if len(f.CallsNamed("planGoal")) != 1 {
			t.Fatal("the plan is handed in")
		}
	})
}
