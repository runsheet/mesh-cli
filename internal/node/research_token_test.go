package node

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// researchTokenSeed is seed with a second repo, acme/gadgets, enrolled
// in the project beside acme/widgets, the direction claimed by the node,
// and the direction's research tokens served for both (adr-022).
func researchTokenSeed(t *testing.T) (*gatewaytest.Fake, *Director, *fakeWS, *fakeHarness, *Config, *bytes.Buffer) {
	t.Helper()
	f := seed(t)
	delete(f.Unsupported, "researchGitHubToken")
	f.Repos[repo2ID] = map[string]any{"id": repo2ID, "accountId": acctID, "name": "gadgets", "defaultBranch": "main"}
	f.Projects[projID]["repoIds"] = []string{repoID, repo2ID}
	direction(f, dirID, projID, "overnight", "Go on then.", 3)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	f.ResearchGitHubTokens[dirID] = []map[string]any{
		{"token": "ghs_research_widgets", "expiresAt": exp, "repository": "acme/widgets"},
		{"token": "ghs_research_gadgets", "expiresAt": exp, "repository": "acme/gadgets"},
	}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"goals\": []}\n```"}
	var logs bytes.Buffer
	d := &Director{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	if err := gw.ClaimDirectionResearch(context.Background(), dirID, cfg.NodeID); err != nil {
		t.Fatal(err)
	}
	return f, d, ws, h, cfg, &logs
}

func overnight(id, slug string) gateway.Direction {
	return gateway.Direction{ID: id, ProjectID: projID, OrgID: orgID, Slug: slug,
		Direction: "Go on then.", Enabled: true, Status: "active", MaxLiveGoals: 3}
}

func researchProject() gateway.Project {
	return gateway.Project{ID: projID, Slug: "mesh", OrgID: orgID, RepoIDs: []string{repoID, repo2ID}}
}

// A node with no gh login researches: each repo's checkout is
// authorized on its own row of the direction's research tokens before
// it is fetched, and the session runs with GH_TOKEN the first repo's
// (by sorted name) and a GH_CONFIG_DIR of its own, never the
// operator's (adr-022).
func TestDirectorFetchesOnTheDirectionsTokens(t *testing.T) {
	// the operator never ran gh auth login: an empty gh config, no token
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "")
	os.Unsetenv("GH_TOKEN")
	f, d, ws, h, cfg, logs := researchTokenSeed(t)
	if err := d.Research(context.Background(), overnight(dirID, "overnight"), researchProject(), scopesOf(cfg)[0]); err != nil {
		t.Fatalf("research: %v\n%s", err, logs.String())
	}

	dir := ResearchDir(ws.root, "mesh", "overnight")
	wantOps := []string{"authorize acme/gadgets ghs_research_gadgets", "fetch acme/gadgets", "authorize acme/widgets ghs_research_widgets", "fetch acme/widgets"}
	if !slices.Equal(ws.repoOps, wantOps) {
		t.Fatalf("workspace calls = %q, want %q", ws.repoOps, wantOps)
	}
	wantAuth := []string{
		"acme/gadgets ghs_research_gadgets " + filepath.Join(dir, "repos", "gadgets"),
		"acme/widgets ghs_research_widgets " + filepath.Join(dir, "repos", "widgets"),
	}
	if !slices.Equal(ws.authorized, wantAuth) {
		t.Fatalf("authorized = %q, want %q", ws.authorized, wantAuth)
	}
	if calls := f.CallsNamed("researchGitHubToken"); len(calls) != 1 {
		t.Fatalf("researchGitHubToken calls: %+v", calls)
	}
	if calls := f.CallsNamed("recordDirectionResearch"); len(calls) != 1 {
		t.Fatalf("recordDirectionResearch calls: %+v", calls)
	}

	if len(h.jobs) != 1 {
		t.Fatalf("research sessions = %d, want 1", len(h.jobs))
	}
	operator, err := OperatorGHConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	var token, ghDir string
	for _, kv := range h.jobs[0].Env {
		if v, ok := strings.CutPrefix(kv, "GH_TOKEN="); ok {
			token = v
		}
		if v, ok := strings.CutPrefix(kv, "GH_CONFIG_DIR="); ok {
			ghDir = v
		}
	}
	if token != "ghs_research_gadgets" {
		t.Fatalf("session GH_TOKEN = %q, want the first sorted repo's, ghs_research_gadgets (env %q)", token, h.jobs[0].Env)
	}
	if ghDir == "" || ghDir == operator {
		t.Fatalf("session GH_CONFIG_DIR = %q, operator's %q", ghDir, operator)
	}
	if strings.Contains(logs.String(), "operator's gh") {
		t.Fatalf("no fallback on a platform that issues research tokens:\n%s", logs.String())
	}
}

// A repo the platform issues no token for, or a refused read, is the
// research's error naming the repo, before any fetch and any session.
func TestDirectorRefusedTokenNamesTheRepo(t *testing.T) {
	t.Run("no row", func(t *testing.T) {
		f, d, ws, h, cfg, _ := researchTokenSeed(t)
		f.ResearchGitHubTokens[dirID] = f.ResearchGitHubTokens[dirID][:1] // widgets alone
		err := d.Research(context.Background(), overnight(dirID, "overnight"), researchProject(), scopesOf(cfg)[0])
		if err == nil || !strings.Contains(err.Error(), "acme/gadgets") {
			t.Fatalf("research error = %v, want one naming acme/gadgets", err)
		}
		if len(h.jobs) != 0 || len(ws.repoOps) != 0 {
			t.Fatalf("sessions %d, workspace calls %q: want none", len(h.jobs), ws.repoOps)
		}
	})
	t.Run("refused", func(t *testing.T) {
		f, d, ws, h, cfg, _ := researchTokenSeed(t)
		f.Refuse["researchGitHubToken"] = "no GitHub installation reaches acme/gadgets"
		err := d.Research(context.Background(), overnight(dirID, "overnight"), researchProject(), scopesOf(cfg)[0])
		if err == nil || !strings.Contains(err.Error(), "research github token") || !strings.Contains(err.Error(), "acme/gadgets") {
			t.Fatalf("research error = %v, want one naming acme/gadgets", err)
		}
		if len(h.jobs) != 0 || len(ws.repoOps) != 0 {
			t.Fatalf("sessions %d, workspace calls %q: want none", len(h.jobs), ws.repoOps)
		}
	})
}

// An old platform researches as before, on the operator's gh: Fetch
// without Authorize, no GH_TOKEN on the session, the fallback said once
// for the Director however many directions it researches.
func TestDirectorOldPlatformFallsBackOnce(t *testing.T) {
	f, d, ws, h, cfg, logs := researchTokenSeed(t)
	f.Unsupported["researchGitHubToken"] = true
	const dir2ID = "eeeeeeee-5555-4555-8555-eeeeeeeeeeee"
	direction(f, dir2ID, projID, "daytime", "And by day.", 3)
	if err := d.GW.ClaimDirectionResearch(context.Background(), dir2ID, cfg.NodeID); err != nil {
		t.Fatal(err)
	}
	for _, dn := range []gateway.Direction{overnight(dirID, "overnight"), overnight(dir2ID, "daytime")} {
		if err := d.Research(context.Background(), dn, researchProject(), scopesOf(cfg)[0]); err != nil {
			t.Fatalf("research %s: %v\n%s", dn.Slug, err, logs.String())
		}
	}
	for _, op := range ws.repoOps {
		if !strings.HasPrefix(op, "fetch ") {
			t.Fatalf("workspace calls %q: want fetches alone", ws.repoOps)
		}
	}
	if len(ws.repoOps) != 4 {
		t.Fatalf("workspace calls %q: want 4 fetches", ws.repoOps)
	}
	if n := strings.Count(logs.String(), "platform issues no research token; fetching on the operator's gh"); n != 1 {
		t.Fatalf("fallback said %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "direction mesh/overnight: platform issues no research token; fetching on the operator's gh") {
		t.Fatalf("fallback line:\n%s", logs.String())
	}
	if len(h.jobs) != 2 {
		t.Fatalf("research sessions = %d, want 2", len(h.jobs))
	}
	for _, j := range h.jobs {
		for _, kv := range j.Env {
			if strings.HasPrefix(kv, "GH_TOKEN=") {
				t.Fatalf("an old platform's research session got %q", kv)
			}
		}
	}
	if calls := f.CallsNamed("recordDirectionResearch"); len(calls) != 2 {
		t.Fatalf("recordDirectionResearch calls: %d, want 2", len(calls))
	}
}
