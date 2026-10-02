package node

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// A live direction gets a research session on the tick: the claim
// first, the repos of its project fetched read-only, the context
// written, and the session's proposals handed in against the direction
// with its cost. A direction switched off is left alone, so is an
// archived one, and so is a project with no direction at all.
func TestLoopResearchesDirectedProjects(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	direction(f, dirID, projID, "overnight", "Mesh is the place goals turn into merged pull requests.", 3)
	const off = "eeeeeeee-2222-4222-8222-eeeeeeeeeeee"
	d := direction(f, off, projID, "mornings", "Chores before the day starts.", 3)
	d["enabled"] = false
	const gone = "eeeeeeee-3333-4333-8333-eeeeeeeeeeee"
	d = direction(f, gone, projID, "old-one", "What we used to want.", 3)
	d["status"] = "archived"
	const quiet = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	f.Projects[quiet] = map[string]any{"id": quiet, "orgId": orgID, "slug": "quiet", "name": "Quiet", "repoIds": []string{repoID}}

	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "I read the PRD and the open issues.\n\n```json\n{\"goals\": [" +
		"{\"slug\": \"One Sign-In!\", \"title\": \"One sign-in\", \"statement\": \"Everyone signs in once.\", \"acceptance\": \"a person signs in once and reaches every service\", \"why\": \"the direction asks for one front door\"}," +
		"{\"slug\": \"health-board\", \"title\": \"The health board\", \"statement\": \"Every service's health on one page.\", \"acceptance\": \"the page lists every service with its last check\", \"why\": \"nobody can see what is down\", \"budget_cents\": 3000}" +
		"]}\n```"}
	dir := &Director{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(os.Stderr, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: dir.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(ctx context.Context, dn gateway.Direction, p gateway.Project, sc Scope) {
			_ = dir.Research(ctx, dn, p, sc)
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()

	claims := f.CallsNamed("claimDirectionResearch")
	if len(claims) != 1 || claims[0].Input["aggregateId"] != dirID || claims[0].Input["nodeId"] != cfg.NodeID {
		t.Fatalf("the live direction alone is claimed: %+v", claims)
	}
	if claims[0].Input["at"] == "" {
		t.Fatal("a claim carries the time it is made")
	}
	calls := f.CallsNamed("recordDirectionResearch")
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
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindResearch || h.jobs[0].Workspace != ResearchDir(ws.root, "mesh", "overnight") {
		t.Fatalf("job: %+v", h.jobs)
	}
	if len(ws.fetched) != 1 || !strings.HasPrefix(ws.fetched[0], "acme/widgets → "+filepath.Join(ResearchDir(ws.root, "mesh", "overnight"), "repos", "widgets")) {
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
	if len(ws.pushed) != 0 {
		t.Fatal("the director changes nothing")
	}

	// the hand-in is against the direction, not the project
	if calls[0].Input["aggregateId"] != dirID {
		t.Fatalf("the research belongs to the direction: %v", calls[0].Input["aggregateId"])
	}
	// the claim is let go, so the next window is another node's to take
	if held, _ := f.Directions[dirID]["researchNodeId"].(string); held != "" {
		t.Fatalf("the claim is let go on the hand-in: %q", held)
	}
}

// A direction whose project is at its *total* ceiling is not claimed at
// all, however much room the direction has of its own: a session there
// would have every proposal dropped, and an unattended night should not
// pay for that. The total is the project's, over every direction on it
// (runsheet trd §6c), so a goal a person set fills it too.
func TestLoopSkipsAFullProject(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Projects[projID]["maxLiveGoals"] = 2
	direction(f, dirID, projID, "overnight", "Go on then.", 5) // its own share is free
	const second = "dddddddd-5555-4555-8555-dddddddddddd"
	f.Goals[second] = map[string]any{"id": second, "orgId": orgID, "projectId": projID, "slug": "blocked-one",
		"title": "Blocked one", "status": "blocked"}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ran := 0
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: log.New(os.Stderr, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(context.Context, gateway.Direction, gateway.Project, Scope) { ran++ }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 0 || ran != 0 {
		t.Fatalf("a running and a blocked goal fill a total of two: %d claims, %d sessions", n, ran)
	}
	// with the project's total raised there is room again
	f.Projects[projID]["maxLiveGoals"] = 3
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 1 || ran != 1 {
		t.Fatalf("room again: %d claims, %d sessions", n, ran)
	}
}

// fullProjectForDirection is TestLoopSkipsAFullProject's fixture — a
// project whose maxLiveGoals is 2 with two live goals on it — with the
// direction in mode (none when empty), a loop whose sessions run through
// a Director on a fake harness, and the harness.
func fullProjectForDirection(t *testing.T, mode string) (*gatewaytest.Fake, *Loop, *fakeHarness) {
	t.Helper()
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Projects[projID]["maxLiveGoals"] = 2
	d := direction(f, dirID, projID, "scout", "Go on then.", 5) // its own share is free
	if mode != "" {
		d["mode"] = mode
	}
	const second = "dddddddd-5555-4555-8555-dddddddddddd"
	f.Goals[second] = map[string]any{"id": second, "orgId": orgID, "projectId": projID, "slug": "blocked-one",
		"title": "Blocked one", "status": "blocked"}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "Nothing worth pitching tonight.\n\n```json\n{\"goals\": []}\n```"}
	if mode == "propose" {
		h.text = "Nothing worth pitching tonight.\n\n```json\n{\"pitches\": [], \"answers\": []}\n```"
	}
	dir := &Director{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(os.Stderr, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: dir.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(ctx context.Context, dn gateway.Direction, p gateway.Project, sc Scope) {
			_ = dir.Research(ctx, dn, p, sc)
		}}
	return f, loop, h
}

// A propose-mode direction is offered whatever the project's ceiling
// says (runsheet ADR 022): what it proposes waits for a person and takes
// no live-goal slot. The room still reaches its session, as information.
func TestLoopResearchesAProposeDirectionOnAFullProject(t *testing.T) {
	f, loop, h := fullProjectForDirection(t, "propose")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 1 || len(h.jobs) != 1 {
		t.Fatalf("a propose direction on a full project: %d claims, %d sessions", n, len(h.jobs))
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{
		"This direction proposes rather than sets",
		"the room is information, not a cap",
		"The project has room for 0 right now",
		"a proposal that would wait for room should say so in its why",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Propose nothing") {
		t.Fatalf("a propose direction is not told to propose nothing:\n%s", p)
	}
	calls := f.CallsNamed("recordDirectionResearch")
	if len(calls) != 1 || calls[0].Input["notes"] != "0 pitched, 0 answered" {
		t.Fatalf("the research is handed in, read as pitches: %+v", calls)
	}
}

// A set-mode direction on the same full project keeps the guard: every
// goal it set would be dropped, so it is not offered at all.
func TestLoopSkipsASetDirectionOnAFullProject(t *testing.T) {
	f, loop, h := fullProjectForDirection(t, "set")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 0 || len(h.jobs) != 0 {
		t.Fatalf("a set direction on a full project: %d claims, %d sessions", n, len(h.jobs))
	}
}

// A platform older than ADR 022 refuses mode: the directions are still
// listed, the read retried without it, and a direction whose row says
// propose reads as mode-less — guarded by the room.
func TestLoopGuardsDirectionsOnAPlatformWithoutMode(t *testing.T) {
	f, loop, h := fullProjectForDirection(t, "propose")
	f.UnknownFields["mode"] = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	reads := f.ReadsNamed("directionIndexs")
	if len(reads) < 2 {
		t.Fatalf("the read is retried without mode: %d reads", len(reads))
	}
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 0 || len(h.jobs) != 0 {
		t.Fatalf("a mode-less direction on a full project: %d claims, %d sessions", n, len(h.jobs))
	}
	// the same platform with room: the direction was listed, and is offered
	f.Projects[projID]["maxLiveGoals"] = 3
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 1 {
		t.Fatalf("with room the listed direction is claimed: %d claims", n)
	}
}

// A direction at its own share is not claimed either, though its
// project has room to spare — and the share is counted per stream, so a
// goal another direction set spends that direction's share, not this
// one's.
func TestLoopSkipsADirectionAtItsShare(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Projects[projID]["maxLiveGoals"] = 9
	direction(f, dirID, projID, "overnight", "Go on then.", 1)
	f.Goals[goalID]["directionId"] = dirID // its one goal is still running
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ran := 0
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: log.New(os.Stderr, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(context.Context, gateway.Direction, gateway.Project, Scope) { ran++ }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 0 || ran != 0 {
		t.Fatalf("a share of one with one goal live in it is full: %d claims, %d sessions", n, ran)
	}
	// the same goal, set by the other direction: this one's share is free
	f.Goals[goalID]["directionId"] = "eeeeeeee-4444-4444-8444-eeeeeeeeeeee"
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 1 || ran != 1 {
		t.Fatalf("another direction's goal is not this one's share: %d claims, %d sessions", n, ran)
	}
}

// The platform judges the window, the gap, the claim and whether the
// direction is on — so being refused is being told to wait, and the
// node says nothing about it and offers the direction again next tick.
func TestLoopKeepsQuietOnARefusedDirection(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	direction(f, dirID, projID, "overnight", "Go on then.", 3)
	f.Refuse["claimDirectionResearch"] = "the direction is outside its window"
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	var logged strings.Builder
	ran := 0
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: log.New(&logged, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(context.Context, gateway.Direction, gateway.Project, Scope) { ran++ }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	if ran != 0 {
		t.Fatalf("a refused claim runs no session: %d", ran)
	}
	if strings.Contains(logged.String(), "claim the research") {
		t.Fatalf("a refusal is the ordinary answer, not an error: %q", logged.String())
	}

	// and the direction is offered again: the window comes round
	f.Refuse = map[string]string{}
	loop.Tick(ctx)
	loop.wg.Wait()
	if ran != 1 || len(f.CallsNamed("claimDirectionResearch")) != 1 {
		t.Fatalf("the next tick offers it again: %d sessions", ran)
	}
}

// A session whose proposals cannot be read hands in an empty research
// rather than leaving the direction claimed — the cost is real and the
// claim would otherwise hold the direction for 45 minutes.
func TestResearchUnreadableHandsInEmpty(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	direction(f, dirID, projID, "overnight", "Go on then.", 3)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "I had a good think and decided not to say."}
	d := &Director{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := gw.ClaimDirectionResearch(ctx, dirID, cfg.NodeID); err != nil {
		t.Fatal(err)
	}
	dn := gateway.Direction{ID: dirID, ProjectID: projID, OrgID: orgID, Slug: "overnight",
		Direction: "Go on then.", Enabled: true, Status: "active", MaxLiveGoals: 3}
	err := d.Research(ctx, dn, gateway.Project{ID: projID, Slug: "mesh", OrgID: orgID, RepoIDs: []string{repoID}}, scopesOf(cfg)[0])
	if err == nil {
		t.Fatal("an unreadable session is an error")
	}
	calls := f.CallsNamed("recordDirectionResearch")
	if len(calls) != 1 {
		t.Fatalf("the empty session is handed in anyway: %+v", calls)
	}
	if notes, _ := calls[0].Input["notes"].(string); !strings.Contains(notes, "could not be read") {
		t.Fatalf("the notes say why: %q", notes)
	}
	if calls[0].Input["costCents"] != float64(125) {
		t.Fatalf("the cost is real: %v", calls[0].Input["costCents"])
	}
	if held, _ := f.Directions[dirID]["researchNodeId"].(string); held != "" {
		t.Fatalf("the claim is let go: %q", held)
	}
}

// dirID is the direction seeded on the test project: the platform
// derives it from (project, slug), and the node only ever reads it off
// the DirectionIndex row.
const dirID = "eeeeeeee-1111-4111-8111-eeeeeeeeeeee"

// direction seeds one DirectionIndex row and returns it, so a test can
// switch it off or archive it.
func direction(f *gatewaytest.Fake, id, projectID, slug, words string, ceiling int) map[string]any {
	row := map[string]any{
		"id": id, "projectId": projectID, "orgId": orgID, "slug": slug, "name": slug,
		"direction": words, "enabled": true, "fromMinute": 0, "toMinute": 0,
		"timezone": "UTC", "gapMinutes": 45, "maxLiveGoals": ceiling, "status": "active",
	}
	f.Directions[id] = row
	return row
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

// barrierHarness answers with a proposal named after the direction the
// session was given, and holds every session until two are in flight —
// so a run that serialises the two directions, or lets one shut the
// other out, fails here rather than passing by accident.
type barrierHarness struct {
	mu   sync.Mutex
	jobs []harness.Job
	both chan struct{}
	once sync.Once
}

func (h *barrierHarness) Name() string { return "claude-code" }

func (h *barrierHarness) Run(ctx context.Context, job harness.Job, _ func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.jobs = append(h.jobs, job)
	n := len(h.jobs)
	h.mu.Unlock()
	if n >= 2 {
		h.once.Do(func() { close(h.both) })
	}
	select {
	case <-h.both:
	case <-ctx.Done():
		return harness.Result{}, ctx.Err()
	case <-time.After(5 * time.Second):
		return harness.Result{}, fmt.Errorf("%s ran alone: the other direction got no session", job.Label)
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(job.Label, "mesh/"), " research")
	text := "I read it.\n\n```json\n{\"goals\": [{\"slug\": \"" + slug + "-idea\", \"title\": \"An idea\"," +
		" \"statement\": \"Do the thing.\", \"acceptance\": \"the thing is done\", \"why\": \"the direction says so\"}]}\n```"
	return harness.Result{Success: true, Text: text, CostUSD: 1.25, SessionID: "s-" + slug}, nil
}

// Two directions on one project research independently: each is claimed
// on its own, each gets its own session in its own checkout on its own
// words, and each hands in against itself. The in-flight map is keyed on
// the direction, so neither shuts the other out.
func TestLoopResearchesTwoDirectionsOfOneProject(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Projects[projID]["maxLiveGoals"] = 9
	direction(f, dirID, projID, "overnight", "Features while everyone sleeps.", 3)
	const mornings = "eeeeeeee-5555-4555-8555-eeeeeeeeeeee"
	direction(f, mornings, projID, "mornings", "Chores before the day starts.", 3)

	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &barrierHarness{both: make(chan struct{})}
	dir := &Director{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(os.Stderr, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: dir.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(ctx context.Context, dn gateway.Direction, p gateway.Project, sc Scope) {
			if err := dir.Research(ctx, dn, p, sc); err != nil {
				t.Errorf("%s: %v", dn.Slug, err)
			}
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()

	claimed := map[string]bool{}
	for _, c := range f.CallsNamed("claimDirectionResearch") {
		claimed[fmt.Sprint(c.Input["aggregateId"])] = true
	}
	if len(claimed) != 2 || !claimed[dirID] || !claimed[mornings] {
		t.Fatalf("both directions are claimed, each on its own: %v", claimed)
	}
	// each session read its own direction, in its own checkout
	want := map[string]string{
		ResearchDir(ws.root, "mesh", "overnight"): "Features while everyone sleeps.",
		ResearchDir(ws.root, "mesh", "mornings"):  "Chores before the day starts.",
	}
	if len(h.jobs) != 2 {
		t.Fatalf("one session each: %d", len(h.jobs))
	}
	for _, job := range h.jobs {
		words, ok := want[job.Workspace]
		if !ok {
			t.Fatalf("a session outside the direction's own checkout: %s", job.Workspace)
		}
		if !strings.Contains(job.Prompt, words) {
			t.Fatalf("%s got the other direction's words:\n%s", job.Workspace, job.Prompt)
		}
		delete(want, job.Workspace)
	}
	// each handed in against itself, with what it proposed
	handed := map[string]string{}
	for _, c := range f.CallsNamed("recordDirectionResearch") {
		proposals, _ := c.Input["proposals"].([]any)
		if len(proposals) != 1 {
			t.Fatalf("one proposal each: %+v", proposals)
		}
		first, _ := proposals[0].(map[string]any)
		handed[fmt.Sprint(c.Input["aggregateId"])] = fmt.Sprint(first["slug"])
	}
	if handed[dirID] != "overnight-idea" || handed[mornings] != "mornings-idea" {
		t.Fatalf("each direction is handed its own session's proposals: %v", handed)
	}
	// and both claims are let go, for the next window
	for _, id := range []string{dirID, mornings} {
		if held, _ := f.Directions[id]["researchNodeId"].(string); held != "" {
			t.Fatalf("%s still held: %q", id, held)
		}
	}
}

// researchWithUsage runs one research session on the seeded direction,
// its credential issued by the platform, the stream fixture's usage and
// text as its final message.
func researchWithUsage(t *testing.T, text string) (*gatewaytest.Fake, error) {
	t.Helper()
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	direction(f, dirID, projID, "overnight", "Go on then.", 3)
	issueCredential(f, dirID)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &streamHarness{file: usageStream(), text: text}
	l := log.New(os.Stderr, "", 0)
	d := &Director{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Credentials: usageCredentials(gw, l), Log: l}
	ctx := context.Background()
	if err := gw.ClaimDirectionResearch(ctx, dirID, cfg.NodeID); err != nil {
		t.Fatal(err)
	}
	dn := gateway.Direction{ID: dirID, ProjectID: projID, OrgID: orgID, Slug: "overnight",
		Direction: "Go on then.", Enabled: true, Status: "active", MaxLiveGoals: 3}
	err := d.Research(ctx, dn, gateway.Project{ID: projID, Slug: "mesh", OrgID: orgID, RepoIDs: []string{repoID}}, scopesOf(cfg)[0])
	if reads := f.CallsNamed("researchCredentials"); len(reads) != 1 {
		t.Fatalf("researchCredentials: %+v", reads)
	}
	return f, err
}

// Research hands its usage report in on recordDirectionResearch — and
// so does the empty hand-in of a session whose proposals cannot be read.
func TestResearchReportsUsage(t *testing.T) {
	f, err := researchWithUsage(t, "```json\n{\"goals\": [{\"slug\": \"health-board\", \"title\": \"The health board\", \"statement\": \"Every service's health on one page.\", \"acceptance\": \"the page lists every service\", \"why\": \"nobody can see what is down\"}]}\n```")
	if err != nil {
		t.Fatal(err)
	}
	checkSessionUsage(t, f, "recordDirectionResearch")

	f, err = researchWithUsage(t, "I had a good think and decided not to say.")
	if err == nil {
		t.Fatal("an unreadable session is an error")
	}
	checkSessionUsage(t, f, "recordDirectionResearch")
}

// Research runs on an issued subscription with headroom — a stale
// `default` reading aside — and waits while every issued one is paced.
func TestResearchClaimsOnAnIssuedSubscriptionWithHeadroom(t *testing.T) {
	for _, c := range []struct {
		name       string
		shhemail   float64
		wantClaims int
	}{{"one fresh", 0.1, 1}, {"both paced", 0.85, 0}} {
		p := newPriorityFake(t)
		direction(p.Fake, dirID, projID, "nightly", "Tidy up.", 5)
		loop, _, _ := p.pacedLoop(2, reading(0.2, 0.97))
		loop.Known = issuedPair(t, "")
		loop.Limits.Observe("max20", reading(0.85, 0.2))
		loop.Limits.Observe("shhemail", reading(c.shhemail, 0.2))
		loop.Tick(context.Background())
		loop.wg.Wait()
		if n := len(p.CallsNamed("claimDirectionResearch")); n != c.wantClaims {
			t.Fatalf("%s: claimed research %d time(s), want %d", c.name, n, c.wantClaims)
		}
	}
}

// twoPitches is a propose-mode session's final message: two pitches —
// an initiative with every field, and a goal with no bet, estimate or
// signal — and one answer to an earlier pitch's open question.
const twoPitches = "I read the scorecard and the pitch feedback.\n\n```json\n" + `{"pitches": [
{"slug": "Fewer Conflicts!", "title": "Fewer conflicts", "problem": "Rebases eat a fifth of the night.",
 "evidence": [{"kind": "metric", "ref": "scorecard:conflictReworksPer100Merges", "note": "22 per 100 merges"},
              {"kind": "research_note", "ref": "notes/rebases.md", "note": "most are lockfiles"}],
 "bet": "the-night-shift",
 "scope": {"kind": "initiative", "goals": [
   {"slug": "Lockfile Merge", "title": "Lockfiles merge themselves", "statement": "s1", "acceptance": "a1", "budget_cents": 40000},
   {"slug": "smaller-items", "title": "Smaller items", "statement": "s2", "acceptance": "a2"}]},
 "estimate": {"cost_cents": 90000, "days": 5.5, "basis": "goal_history: three goals of this size"},
 "risks": ["the planner splits too finely"], "alternatives": ["serialise merges"],
 "signal": {"metric": "conflictReworksPer100Merges", "how_measured": "the scorecard", "direction": "down", "window_days": 30}},
{"slug": "onboarding-doc", "title": "An onboarding doc", "problem": "New operators ask the same things.",
 "evidence": [{"kind": "user_signal", "ref": "https://example.com/issue/7", "note": "asked twice"}],
 "bet": "", "scope": {"kind": "goal", "goals": [{"slug": "onboarding-doc", "title": "Onboarding doc", "statement": "s", "acceptance": "a"}]},
 "risks": [], "alternatives": []}],
"answers": [{"pitch": "Earlier Pitch", "text": "Yes: the metric is on the scorecard already."}]}` + "\n```"

// researchOn is a Director on a fake harness answering text, for a
// direction in mode on the test project, its claim already held; and
// what it logs.
func researchOn(t *testing.T, f *gatewaytest.Fake, mode, text string) (*Director, gateway.Direction, gateway.Project, Scope, *strings.Builder) {
	t.Helper()
	f.Projects[projID]["repoIds"] = []string{repoID}
	direction(f, dirID, projID, "scout", "Go on then.", 3)["mode"] = mode
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	var logged strings.Builder
	d := &Director{Credentials: testCreds(gw), GW: gw, Harness: &fakeHarness{text: text}, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logged, "", 0)}
	dn := gateway.Direction{ID: dirID, ProjectID: projID, OrgID: orgID, Slug: "scout", Mode: mode,
		Direction: "Go on then.", Enabled: true, Status: "active", MaxLiveGoals: 3}
	return d, dn, gateway.Project{ID: projID, Slug: "mesh", OrgID: orgID, RepoIDs: []string{repoID}}, scopesOf(cfg)[0], &logged
}

// claimAndResearch holds the direction's claim and runs one session.
func claimAndResearch(t *testing.T, d *Director, dn gateway.Direction, p gateway.Project, sc Scope) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := d.GW.ClaimDirectionResearch(ctx, dn.ID, sc.NodeID); err != nil {
		t.Fatal(err)
	}
	return d.Research(ctx, dn, p, sc)
}

// A propose-mode direction pitches (runsheet ADR 026): the session's
// pitches land on the direction as `pitches`, every field carried and
// every slug the platform's identity, with its `answers` — and no
// proposals.
func TestResearchProposeDirectionHandsInPitches(t *testing.T) {
	f := seed(t)
	d, dn, p, sc, logged := researchOn(t, f, "propose", twoPitches)
	if err := claimAndResearch(t, d, dn, p, sc); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("recordDirectionResearch")
	if len(calls) != 1 || calls[0].Input["notes"] != "2 pitched, 1 answered" {
		t.Fatalf("hand-in: %+v", calls)
	}
	row := f.Directions[dirID]
	if proposals, _ := row["proposals"].([]any); len(proposals) != 0 {
		t.Fatalf("a propose direction hands in no proposals: %+v", row["proposals"])
	}
	pitches, _ := row["pitches"].([]any)
	if len(pitches) != 2 {
		t.Fatalf("two pitches on the row: %+v", row["pitches"])
	}
	first, _ := pitches[0].(map[string]any)
	for k, want := range map[string]any{
		"slug": "fewer-conflicts", "title": "Fewer conflicts", "problem": "Rebases eat a fifth of the night.",
		"bet": "the-night-shift",
	} {
		if first[k] != want {
			t.Fatalf("pitch 1 %s = %v, want %v: %+v", k, first[k], want, first)
		}
	}
	evidence, _ := first["evidence"].([]any)
	if len(evidence) != 2 {
		t.Fatalf("evidence: %+v", first["evidence"])
	}
	if ev, _ := evidence[0].(map[string]any); ev["kind"] != "metric" || ev["ref"] != "scorecard:conflictReworksPer100Merges" || ev["note"] != "22 per 100 merges" {
		t.Fatalf("evidence 1: %+v", ev)
	}
	scope, _ := first["scope"].(map[string]any)
	goals, _ := scope["goals"].([]any)
	if scope["kind"] != "initiative" || len(goals) != 2 {
		t.Fatalf("scope: %+v", scope)
	}
	if g, _ := goals[0].(map[string]any); g["slug"] != "lockfile-merge" || g["title"] != "Lockfiles merge themselves" ||
		g["statement"] != "s1" || g["acceptance"] != "a1" || g["budgetCents"] != float64(40000) {
		t.Fatalf("scope goal 1, its slug slugified: %+v", g)
	}
	if e, _ := first["estimate"].(map[string]any); e["costCents"] != float64(90000) || e["days"] != float64(6) || e["basis"] != "goal_history: three goals of this size" {
		t.Fatalf("estimate, its days rounded up: %+v", first["estimate"])
	}
	if r, _ := first["risks"].([]any); len(r) != 1 || r[0] != "the planner splits too finely" {
		t.Fatalf("risks: %+v", first["risks"])
	}
	if a, _ := first["alternatives"].([]any); len(a) != 1 || a[0] != "serialise merges" {
		t.Fatalf("alternatives: %+v", first["alternatives"])
	}
	if sig, _ := first["signal"].(map[string]any); sig["metric"] != "conflictReworksPer100Merges" || sig["howMeasured"] != "the scorecard" ||
		sig["direction"] != "down" || sig["windowDays"] != float64(30) {
		t.Fatalf("signal: %+v", first["signal"])
	}
	second, _ := pitches[1].(map[string]any)
	if second["slug"] != "onboarding-doc" || second["bet"] != "" {
		t.Fatalf("pitch 2: %+v", second)
	}
	if _, ok := second["estimate"]; ok {
		t.Fatalf("a pitch with no estimate sends none: %+v", second)
	}
	if _, ok := second["signal"]; ok {
		t.Fatalf("a pitch with no signal sends none: %+v", second)
	}
	if r, _ := second["risks"].([]any); r == nil || len(r) != 0 {
		t.Fatalf("empty risks are sent as []: %+v", second["risks"])
	}
	answers, _ := row["answers"].([]any)
	if len(answers) != 1 {
		t.Fatalf("one answer on the row: %+v", row["answers"])
	}
	if a, _ := answers[0].(map[string]any); a["pitchSlug"] != "earlier-pitch" || a["text"] != "Yes: the metric is on the scorecard already." {
		t.Fatalf("answer: %+v", a)
	}
	if held, _ := row["researchNodeId"].(string); held != "" {
		t.Fatalf("the claim is let go: %q", held)
	}
	if !strings.Contains(logged.String(), "scout: pitched 2, answered 1") {
		t.Fatalf("the log says pitched: %q", logged.String())
	}
}

// A set-mode direction keeps proposing: a goals block lands as
// `proposals`, and no pitches.
func TestResearchSetDirectionHandsInProposals(t *testing.T) {
	f := seed(t)
	text := "```json\n{\"goals\": [{\"slug\": \"One Sign-In\", \"title\": \"One sign-in\", \"statement\": \"s\", \"acceptance\": \"a\", \"why\": \"w\"}]}\n```"
	d, dn, p, sc, logged := researchOn(t, f, "set", text)
	if err := claimAndResearch(t, d, dn, p, sc); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("recordDirectionResearch")
	if len(calls) != 1 || calls[0].Input["notes"] != "1 proposed" {
		t.Fatalf("hand-in: %+v", calls)
	}
	if _, ok := calls[0].Input["pitches"]; ok {
		t.Fatalf("a set direction sends no pitches: %+v", calls[0].Input)
	}
	row := f.Directions[dirID]
	proposals, _ := row["proposals"].([]any)
	if len(proposals) != 1 {
		t.Fatalf("one proposal on the row: %+v", row["proposals"])
	}
	if first, _ := proposals[0].(map[string]any); first["slug"] != "one-sign-in" || first["why"] != "w" {
		t.Fatalf("proposal: %+v", first)
	}
	if row["pitches"] != nil || row["answers"] != nil {
		t.Fatalf("no pitches or answers on the row: %+v %+v", row["pitches"], row["answers"])
	}
	if !strings.Contains(logged.String(), "scout: proposed 1 goal(s)") {
		t.Fatalf("the log says proposed: %q", logged.String())
	}
}

// A platform older than pitches refuses the field: the pitches land as
// proposals — one per goal of each pitch's scope — the claim is let
// go, the research counts as handed in, and the node says so once
// however many sessions hand in.
func TestResearchPitchesFallBackToProposalsSaidOnce(t *testing.T) {
	f := seed(t)
	f.UnknownFields["pitches"] = true
	d, dn, p, sc, logged := researchOn(t, f, "propose", twoPitches)
	for i := range 2 {
		if err := claimAndResearch(t, d, dn, p, sc); err != nil {
			t.Fatalf("session %d: the fallback is no error: %v", i+1, err)
		}
		row := f.Directions[dirID]
		proposals, _ := row["proposals"].([]any)
		if len(proposals) != 3 {
			t.Fatalf("session %d: one proposal per scope goal: %+v", i+1, row["proposals"])
		}
		slugs := []string{}
		for _, pr := range proposals {
			m, _ := pr.(map[string]any)
			slugs = append(slugs, fmt.Sprint(m["slug"]))
		}
		if strings.Join(slugs, ",") != "lockfile-merge,smaller-items,onboarding-doc" {
			t.Fatalf("session %d: proposals: %v", i+1, slugs)
		}
		if row["pitches"] != nil {
			t.Fatalf("session %d: no pitches on the row: %+v", i+1, row["pitches"])
		}
		if held, _ := row["researchNodeId"].(string); held != "" {
			t.Fatalf("session %d: the claim is let go: %q", i+1, held)
		}
	}
	if n := strings.Count(logged.String(), "takes no pitches"); n != 1 {
		t.Fatalf("the fallback is said once, not %d times: %q", n, logged.String())
	}
	if strings.Contains(logged.String(), "hand in the research") {
		t.Fatalf("the fallback is not an error: %q", logged.String())
	}
}

// A propose session with no pitches block hands in an empty research,
// so the claim is let go, and returns the parse error.
func TestResearchUnreadablePitchesHandsInEmpty(t *testing.T) {
	f := seed(t)
	text := "```json\n{\"goals\": [{\"slug\": \"a\", \"title\": \"A\", \"statement\": \"s\", \"acceptance\": \"a\"}]}\n```"
	d, dn, p, sc, _ := researchOn(t, f, "propose", text)
	err := claimAndResearch(t, d, dn, p, sc)
	if err == nil || !strings.Contains(err.Error(), "no pitches") {
		t.Fatalf("the parse error is returned: %v", err)
	}
	calls := f.CallsNamed("recordDirectionResearch")
	if len(calls) != 1 {
		t.Fatalf("the empty research is handed in anyway: %+v", calls)
	}
	if notes, _ := calls[0].Input["notes"].(string); !strings.Contains(notes, "pitches could not be read") {
		t.Fatalf("the notes say why: %q", notes)
	}
	for _, k := range []string{"pitches", "answers"} {
		if _, ok := calls[0].Input[k]; ok {
			t.Fatalf("an empty research carries no %s: %+v", k, calls[0].Input)
		}
	}
	if proposals, _ := calls[0].Input["proposals"].([]any); len(proposals) != 0 {
		t.Fatalf("an empty research carries no proposals: %+v", proposals)
	}
	if held, _ := f.Directions[dirID]["researchNodeId"].(string); held != "" {
		t.Fatalf("the claim is let go: %q", held)
	}
}
