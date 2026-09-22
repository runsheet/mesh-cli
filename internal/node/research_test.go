package node

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// A project with a director gets a research session on the tick: the
// claim first, the repos fetched read-only, the context written, and
// the session's proposals handed in with its cost. A project with the
// director off is left alone, and so is one another node holds.
func TestLoopResearchesDirectedProjects(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Projects[projID]["directorEnabled"] = true
	f.Projects[projID]["direction"] = "Mesh is the place goals turn into merged pull requests."
	f.Projects[projID]["maxLiveGoals"] = 3
	const quiet = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	f.Projects[quiet] = map[string]any{"id": quiet, "orgId": orgID, "slug": "quiet", "name": "Quiet", "repoIds": []string{repoID}}

	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: 1, OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "I read the PRD and the open issues.\n\n```json\n{\"goals\": [" +
		"{\"slug\": \"One Sign-In!\", \"title\": \"One sign-in\", \"statement\": \"Everyone signs in once.\", \"acceptance\": \"a person signs in once and reaches every service\", \"why\": \"the direction asks for one front door\"}," +
		"{\"slug\": \"health-board\", \"title\": \"The health board\", \"statement\": \"Every service's health on one page.\", \"acceptance\": \"the page lists every service with its last check\", \"why\": \"nobody can see what is down\", \"budget_cents\": 3000}" +
		"]}\n```"}
	d := &Director{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(os.Stderr, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: d.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(ctx context.Context, p gateway.Project, sc Scope) { _ = d.Research(ctx, p, sc) }}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()

	claims := f.CallsNamed("claimResearch")
	if len(claims) != 1 || claims[0].Input["aggregateId"] != projID || claims[0].Input["nodeId"] != cfg.NodeID {
		t.Fatalf("the directed project alone is claimed: %+v", claims)
	}
	if claims[0].Input["at"] == "" {
		t.Fatal("a claim carries the time it is made")
	}
	calls := f.CallsNamed("recordResearch")
	if len(calls) != 1 || calls[0].Input["costCents"] != float64(125) {
		t.Fatalf("hand-in: %+v", calls)
	}
	proposals, _ := calls[0].Input["proposals"].([]any)
	if len(proposals) != 2 {
		t.Fatalf("two proposals: %+v", proposals)
	}
	first, _ := proposals[0].(map[string]any)
	if first["slug"] != "one-sign-in" {
		t.Fatalf("the slug is the platform's identity, cleaned: %v", first["slug"])
	}
	if first["why"] != "the direction asks for one front door" || first["title"] != "One sign-in" {
		t.Fatalf("proposal 1: %v", first)
	}
	second, _ := proposals[1].(map[string]any)
	if second["budgetCents"] != float64(3000) {
		t.Fatalf("a goal that names its own budget keeps it: %v", second)
	}

	// the session saw the direction, the repos, the context and the room
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindResearch || h.jobs[0].Workspace != ResearchDir(ws.root, "mesh") {
		t.Fatalf("job: %+v", h.jobs)
	}
	if len(ws.fetched) != 1 || !strings.HasPrefix(ws.fetched[0], "acme/widgets → "+filepath.Join(ResearchDir(ws.root, "mesh"), "repos", "widgets")) {
		t.Fatalf("fetched: %v", ws.fetched)
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{
		"Mesh is the place goals turn into merged pull requests.",
		"`acme/widgets` in `repos/widgets/`", ContextDir, "```json",
		"room for 2",          // three allowed, one goal running
		"`ship-it` — Ship it", // what the project has already taken on
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if len(ws.commits) != 0 || len(ws.pushed) != 0 {
		t.Fatal("the director changes nothing")
	}

	// the claim is let go, so the next window is another node's to take
	if held, _ := f.Projects[projID]["researchNodeId"].(string); held != "" {
		t.Fatalf("the claim is let go on the hand-in: %q", held)
	}
}

// A project already at its ceiling is not claimed at all: a session
// there would have every proposal dropped, and an unattended night
// should not pay for that.
func TestLoopSkipsAFullProject(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Projects[projID]["directorEnabled"] = true
	f.Projects[projID]["direction"] = "Go on then."
	f.Projects[projID]["maxLiveGoals"] = 2
	const second = "dddddddd-5555-4555-8555-dddddddddddd"
	f.Goals[second] = map[string]any{"id": second, "orgId": orgID, "projectId": projID, "slug": "blocked-one",
		"title": "Blocked one", "status": "blocked"}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: 1, OwnerID: f.UserID}
	ran := 0
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: log.New(os.Stderr, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(context.Context, gateway.Project, Scope) { ran++ }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimResearch")); n != 0 || ran != 0 {
		t.Fatalf("a running and a blocked goal fill a ceiling of two: %d claims, %d sessions", n, ran)
	}
	// with the ceiling raised there is room again
	f.Projects[projID]["maxLiveGoals"] = 3
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimResearch")); n != 1 || ran != 1 {
		t.Fatalf("room again: %d claims, %d sessions", n, ran)
	}
}

// A session whose proposals cannot be read hands in an empty research
// rather than leaving the project claimed — the cost is real and the
// claim would otherwise hold the project for 45 minutes.
func TestResearchUnreadableHandsInEmpty(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Projects[projID]["directorEnabled"] = true
	f.Projects[projID]["direction"] = "Go on then."
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: 1, OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "I had a good think and decided not to say."}
	d := &Director{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := gw.ClaimResearch(ctx, projID, cfg.NodeID); err != nil {
		t.Fatal(err)
	}
	err := d.Research(ctx, gateway.Project{ID: projID, Slug: "mesh", OrgID: orgID, RepoIDs: []string{repoID}, Direction: "Go on then.", DirectorEnabled: true}, scopesOf(cfg)[0])
	if err == nil {
		t.Fatal("an unreadable session is an error")
	}
	calls := f.CallsNamed("recordResearch")
	if len(calls) != 1 {
		t.Fatalf("the empty session is handed in anyway: %+v", calls)
	}
	if notes, _ := calls[0].Input["notes"].(string); !strings.Contains(notes, "could not be read") {
		t.Fatalf("the notes say why: %q", notes)
	}
	if calls[0].Input["costCents"] != float64(125) {
		t.Fatalf("the cost is real: %v", calls[0].Input["costCents"])
	}
	if held, _ := f.Projects[projID]["researchNodeId"].(string); held != "" {
		t.Fatalf("the claim is let go: %q", held)
	}
}

// The room the session is told about counts what is not over: a paused
// or blocked goal holds its place, a done one does not.
func TestResearchRoomCountsLiveGoals(t *testing.T) {
	for status, live := range map[string]bool{
		"draft": true, "planning": true, "awaiting_approval": true, "running": true,
		"paused": true, "blocked": true, "done": false, "cancelled": false,
	} {
		if got := isLiveGoal(status); got != live {
			t.Fatalf("%s: live=%v", status, got)
		}
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"One Sign-In!":     "one-sign-in",
		"  spaces  here  ": "spaces-here",
		"already-fine":     "already-fine",
		// anything but a–z and 0–9 is a separator, accents included: the
		// slug is an identity, not a title
		"Ünïcødé 42": "n-c-d-42",
		"--":         "",
	} {
		if got := slugify(in); got != want {
			t.Fatalf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
