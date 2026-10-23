package node

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/jobresult"
	"projectmesh.io/mesh-cli/internal/mcptool"
)

// A research session runs on the brief's guidance when the catalogue
// carries one (ADR-041): the prompt opens with it, none of the node's
// role text or rules, and its goals arrive through the node's
// mesh_result, served beside the runsheet server.
func TestResearchRunsOnTheBriefsGuidance(t *testing.T) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	direction(f, dirID, projID, "overnight", "Features while everyone sleeps.", 3)
	f.SetKindGuidance(ResearchKind, "You are the director of one project.\n\n1. Read first.\n", "")
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{result: map[string]any{"research": map[string]any{"goals": []any{
		map[string]any{"slug": "one-sign-in", "title": "One sign-in", "statement": "Everyone signs in once.", "acceptance": "a person signs in once", "why": "the front door"},
	}}}}
	var logged strings.Builder
	dir := &Director{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(&logged, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: dir.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(ctx context.Context, dn gateway.Direction, p gateway.Project, sc Scope) {
			if err := dir.Research(ctx, dn, p, sc); err != nil {
				t.Errorf("research: %v", err)
			}
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	calls := f.CallsNamed("recordDirectionResearch")
	if len(calls) != 1 {
		t.Fatalf("hand-in: %+v\n%s", calls, logged.String())
	}
	if proposals, _ := calls[0].Input["proposals"].([]any); len(proposals) != 1 {
		t.Fatalf("proposals: %+v", calls[0].Input)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("jobs: %d", len(h.jobs))
	}
	prompt := h.jobs[0].Prompt
	if !strings.HasPrefix(prompt, "You are the director of one project.\n\n1. Read first.\n\n# The direction\n\nFeatures while everyone sleeps.") {
		t.Fatalf("the prompt does not open with the guidance:\n%.400s", prompt)
	}
	for _, gone := range []string{"You are the director of acme", "# Rules\n", "1. Research first", "```json"} {
		if strings.Contains(prompt, gone) {
			t.Fatalf("the prompt carries the node's role text %q", gone)
		}
	}
	for _, want := range []string{"# The room", "# How this job finishes", "{research}", "`research.goals` is your proposals"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	if _, ok := h.jobs[0].MCPServers[harness.NodeWorkMCP]; !ok {
		t.Fatalf("no %s server: %v", harness.NodeWorkMCP, h.jobs[0].MCPServers)
	}
	if strings.Contains(logged.String(), noGuidanceLine) {
		t.Fatalf("a brief with guidance logged %q", noGuidanceLine)
	}
}

// A research session that gives the research up (mesh_fail) hands in an
// empty research whose note carries its reason; one that hands nothing
// in says so, and the direction's claim is let go either way.
func TestResearchGivenUpOrSilentHandsInEmpty(t *testing.T) {
	for name, h := range map[string]*fakeHarness{
		"gave up": {onRun: func(job harness.Job) {
			tool, _ := mcptool.Find(job.NodeTools, jobresult.FailTool)
			_, _ = tool.Call(context.Background(), map[string]any{"error": "the repos name no direction I can read"})
		}},
		"silent": {text: "I thought about it."},
	} {
		t.Run(name, func(t *testing.T) {
			f := seed(t)
			f.Projects[projID]["repoIds"] = []string{repoID}
			direction(f, dirID, projID, "overnight", "Features while everyone sleeps.", 3)
			gw := gateway.New(f.URL(), "rs_test")
			cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
			ws := &fakeWS{root: t.TempDir()}
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
			calls := f.CallsNamed("recordDirectionResearch")
			if len(calls) != 1 {
				t.Fatalf("hand-in: %+v", calls)
			}
			notes, _ := calls[0].Input["notes"].(string)
			want := "no result: the session made no mesh_result call"
			if name == "gave up" {
				want = "the session gave the research up: the repos name no direction I can read"
			}
			if proposals, _ := calls[0].Input["proposals"].([]any); len(proposals) != 0 || !strings.Contains(notes, "proposals could not be read") || !strings.Contains(notes, want) {
				t.Fatalf("recorded %+v", calls[0].Input)
			}
		})
	}
}

// A rebase session runs on the brief's guidance (ADR-041): the prompt
// opens with it, none of the node's role text or rules, and the head it
// pushed arrives through the node's mesh_result on a proxy of the node
// tools' own.
func TestRebaseRunsOnTheBriefsGuidance(t *testing.T) {
	f := seed(t)
	requestRebase(f, item1, "mesh/ship-it-1")
	f.SetKindGuidance(RebaseKind, "You rebase one branch onto its base.\n", "")
	ws := &fakeWS{root: t.TempDir(), pushedHead: "abc123def456"}
	h := &fakeHarness{result: map[string]any{"rebasedHead": map[string]any{"headSha": "abc123def456", "files": []any{"main.go"}}}}
	var logs strings.Builder
	loop, cfg := rebaseNode(t, f, "test-node", h, ws, &logs)
	cfg.Capacity = Fixed(0)
	loop.Tick(context.Background())
	loop.wg.Wait()
	finishes := f.CallsNamed("finishWorkItemRebase")
	if len(finishes) != 1 || finishes[0].Input["headSha"] != "abc123def456" {
		t.Fatalf("finishes: %+v\n%s", finishes, logs.String())
	}
	if len(h.jobs) != 1 {
		t.Fatalf("jobs: %d", len(h.jobs))
	}
	prompt := h.jobs[0].Prompt
	if !strings.HasPrefix(prompt, "You rebase one branch onto its base.\n\n# Item 1:") {
		t.Fatalf("the prompt does not open with the guidance:\n%.300s", prompt)
	}
	for _, gone := range []string{"You are a Mesh node session of kind rebase", "# Rules\n", "REBASED:"} {
		if strings.Contains(prompt, gone) {
			t.Fatalf("the prompt carries the node's role text %q", gone)
		}
	}
	if !strings.Contains(prompt, "# The runbook") || !strings.Contains(prompt, "{rebasedHead}") {
		t.Fatalf("the prompt lacks the runbook or the finish:\n%s", prompt)
	}
	if _, ok := h.jobs[0].MCPServers[harness.NodeWorkMCP]; !ok {
		t.Fatalf("no %s server: %v", harness.NodeWorkMCP, h.jobs[0].MCPServers)
	}
	if strings.Contains(logs.String(), noGuidanceLine) {
		t.Fatalf("a brief with guidance logged %q", noGuidanceLine)
	}
}

// A build session runs on the brief's guidance (ADR-041): the prompt
// opens with it, the node's role line and rules gone, and still carries
// the node's own sections — check main first, the workspace, the finish
// by the node's mesh_finish — since the build's finish stays the node's.
func TestBuildRunsOnTheBriefsGuidance(t *testing.T) {
	f := seed(t)
	f.SetKindGuidance(BuildKind, "You build one item of a goal.\n", "")
	h := &fakeHarness{text: "Built it."}
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	_ = r.RunItem(context.Background(), item)
	if len(h.jobs) != 1 {
		t.Fatalf("jobs: %d", len(h.jobs))
	}
	prompt := h.jobs[0].Prompt
	if !strings.HasPrefix(prompt, "You build one item of a goal.\n\n# Item 1: Write the failing test\n") {
		t.Fatalf("the prompt does not open with the guidance:\n%.300s", prompt)
	}
	for _, gone := range []string{"You are a Mesh node session working on item", "# Rules\n", "3. Commit as you go"} {
		if strings.Contains(prompt, gone) {
			t.Fatalf("the prompt carries the node's role text %q", gone)
		}
	}
	for _, want := range []string{"# Check main first", "# Test what you touched", "# The workspace\n\nYou are in acme/widgets on branch `mesh/ship-it-1` (from `main`)", "# Finish by tool call", "`mesh_finish` tool"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
}
