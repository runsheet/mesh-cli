package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// Goal one-node-serves-several-orgs-each-sandboxed-from-the-rest, item
// 6: one sandboxed node of an enterprise owner serves acme and beta. A
// build of each runs at once, then a plan and a research of each. Every
// session runs in a pod confined to its own org's tree (ADR-034): its
// mounts are its org's checkout, clone, stores and HOME alone, its
// kept-sessions root and token file are its org's, a build's token
// file holds its own mesh session's bearer (ADR-035), and an attempt
// from inside it to mount the other org's checkout, clone, kept
// sessions, package store or MCP data is refused before any pod is made.

const (
	twoOrgsEnterprise = "e1e1e1e1-0000-4000-8000-e1e1e1e1e1e1"
	betaAcct          = "aaaaaaaa-3333-4333-8333-aaaaaaaaaaaa"
	betaProj          = "bbbbbbbb-3333-4333-8333-bbbbbbbbbbbb"
	betaRepo          = "cccccccc-3333-4333-8333-cccccccccccc"
	betaGoal          = "dddddddd-3333-4333-8333-dddddddddddd"
	acmePlanGoal      = "dddddddd-4444-4444-8444-dddddddddddd"
	betaPlanGoal      = "dddddddd-5555-4555-8555-dddddddddddd"
	acmeDir           = "ffffffff-4444-4444-8444-ffffffffffff"
	betaDir           = "ffffffff-5555-4555-8555-ffffffffffff"
)

// twoOrgsOrg is one served org of the e2e: its ids, its build item and
// what its sessions are given.
type twoOrgsOrg struct {
	login, orgID, nodeID, acct, proj, repo, goal, planGoal, dir string
	item                                                        gateway.WorkItem
	roots                                                       OrgRoots
}

// orgCloneWS is fakeWS whose builds are real worktrees of one shared
// clone per repo under the spec's own repos root — the org's, as
// orgSpec sets it (ADR-034) — so a pod's git common dir is that clone.
type orgCloneWS struct {
	*fakeWS
	t      *testing.T
	origin string
}

func (w *orgCloneWS) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	w.mu.Lock()
	w.prepared = append(w.prepared, spec)
	w.mu.Unlock()
	if spec.Repos == "" {
		return "", fmt.Errorf("spec of %s has no org repos root", spec.Dir())
	}
	clone := SharedClonePath(spec.Repos, spec.Repo)
	dir := spec.Dir()
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	gitRun(w.t, filepath.Dir(dir), "-C", clone, "worktree", "add", "--quiet", "-b", spec.Branch, dir, "main")
	return dir, nil
}

// Identify makes the checkout's hooks dir beside it, as Git.Identify
// does, which the pod mounts read-only.
func (w *orgCloneWS) Identify(ctx context.Context, dir string, id Identity) error {
	if err := os.MkdirAll(HooksDir(dir), 0o755); err != nil {
		return err
	}
	return w.fakeWS.Identify(ctx, dir, id)
}

// orgSession is what one session was seen with as it ran.
type orgSession struct {
	org      string
	job      harness.Job
	mounts   []sandbox.Mount
	mountErr error
	token    string
	tokPath  string
	// refused are the other org's paths the session tried to mount,
	// each with what Session.Run answered.
	refused map[string]error
	podsBy  int
}

// twoOrgsHarness runs each session as the Claude Code harness places
// it in a pod — its token file kept in the pod's HOME by the node's
// TokenFiles (KeepSession) — and, from inside it, tries the other
// org's paths. A run with no pod fails (podGuardHarness). Builds wait
// for each other, so both orgs' checkouts exist while either runs.
type twoOrgsHarness struct {
	podGuardHarness
	tokens  *TokenFiles
	orgOf   func(job harness.Job) string
	forbid  func(org string) map[string]string
	rt      *sandbox.Fake
	builds  sync.WaitGroup
	mu      sync.Mutex
	seen    []orgSession
	release map[string]func(bool)
}

func (h *twoOrgsHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	if job.Session == nil {
		return h.podGuardHarness.Run(ctx, job, on)
	}
	org := h.orgOf(job)
	s := orgSession{org: org, job: job, refused: map[string]error{}}
	s.tokPath = filepath.Join(job.Session.Home, harness.TokenFileName)
	release, err := h.tokens.KeepSession(s.tokPath, job.Kind, job.Meta)
	if err != nil {
		h.t.Errorf("%s: token file: %v", job.Label, err)
	} else {
		defer release(true)
		b, _ := os.ReadFile(s.tokPath)
		s.token = strings.TrimSpace(string(b))
	}
	if job.Kind == harness.KindBuild {
		h.builds.Done()
		waited := make(chan struct{})
		go func() { h.builds.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-time.After(20 * time.Second):
			h.t.Errorf("%s: the other org's build never started", job.Label)
		}
	}
	s.mounts, s.mountErr = job.Session.CheckedMounts(job.Workspace)
	before := len(h.rt.Pods)
	for what, p := range h.forbid(org) {
		s.refused[what+" "+p] = job.Session.Run(ctx, sandbox.RunSpec{Workspace: p})
	}
	s.podsBy = len(h.rt.Pods) - before
	h.mu.Lock()
	h.seen = append(h.seen, s)
	h.mu.Unlock()
	return h.podGuardHarness.Run(ctx, job, on)
}

func TestTwoOrgsSandboxedNodeNeitherOrgReadsTheOthersFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	f := seed(t)
	delete(f.Unsupported, "planGitHubToken")
	delete(f.Unsupported, "researchGitHubToken")
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	// an enterprise owner serving acme and beta
	f.Enterprises[twoOrgsEnterprise] = gatewaytest.Enterprise{Slug: "anythink", DisplayName: "Anythink", Admins: []string{f.UserID}}
	f.Orgs = []map[string]any{
		{"id": orgID, "login": "acme", "accountId": acctID, "enterpriseId": twoOrgsEnterprise},
		{"id": orgB, "login": "beta", "accountId": betaAcct, "enterpriseId": twoOrgsEnterprise},
	}
	f.Accounts[betaAcct] = map[string]any{"id": betaAcct, "login": "beta"}
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Projects[betaProj] = map[string]any{"id": betaProj, "orgId": orgB, "slug": "mesh", "name": "Mesh", "repoIds": []string{betaRepo}}
	f.Repos[betaRepo] = map[string]any{"id": betaRepo, "accountId": betaAcct, "name": "widgets", "defaultBranch": "main"}
	f.Goals[betaGoal] = map[string]any{"id": betaGoal, "orgId": orgB, "projectId": betaProj, "slug": "launch", "title": "Launch",
		"statement": "Launch it.", "acceptance": "go test ./... passes", "budgetCents": 1000, "spentCents": 0, "status": "running"}
	f.WorkItems[itemB] = map[string]any{"id": itemB, "goalId": betaGoal, "orgId": orgB, "seq": 1, "title": "Launch the thing",
		"brief": "Launch.", "acceptance": "it launches", "repoId": betaRepo, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	f.GitHubTokens[itemB] = map[string]any{"token": "ghs_itemB", "expiresAt": exp, "repo": "beta/widgets"}
	for _, g := range []struct{ id, org, proj, slug, repo string }{
		{acmePlanGoal, orgID, projID, "plan-acme", "acme/widgets"},
		{betaPlanGoal, orgB, betaProj, "plan-beta", "beta/widgets"},
	} {
		f.Goals[g.id] = map[string]any{"id": g.id, "orgId": g.org, "projectId": g.proj, "slug": g.slug, "title": g.slug,
			"statement": "Plan it.", "acceptance": "go test ./... passes", "budgetCents": 1000, "spentCents": 0, "status": "planning"}
		f.PlanGitHubTokens[g.id] = []map[string]any{{"token": "ghs_plan_" + g.slug, "expiresAt": exp, "repository": g.repo}}
	}
	direction(f, acmeDir, projID, "overnight", "Go on then.", 3)
	direction(f, betaDir, betaProj, "overnight", "Go on then.", 3)["orgId"] = orgB
	f.ResearchGitHubTokens[acmeDir] = []map[string]any{{"token": "ghs_research_acme", "expiresAt": exp, "repository": "acme/widgets"}}
	f.ResearchGitHubTokens[betaDir] = []map[string]any{{"token": "ghs_research_beta", "expiresAt": exp, "repository": "beta/widgets"}}

	gw := gateway.New(f.URL(), machine)
	scopes := Scopes{{Login: "acme", ID: orgID, AccountID: acctID, NodeID: nodeA}, {Login: "beta", ID: orgB, AccountID: betaAcct, NodeID: nodeB}}
	cfg := &Config{Owner: "enterprise", OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: nodeA, Capacity: Fixed(2), OwnerID: f.UserID}

	// the node's sandbox on the fake runtime, ~/.mesh a temp dir, each
	// org's roots under ~/.mesh/orgs/<org>
	sb := nodeStoresSandbox(t)
	rt := sb.Runtime.(*sandbox.Fake)
	// the refused attempts' "mounting the workspace alone" lines are noise
	sb.Logf = func(string, ...any) {}
	home, _ := os.UserHomeDir()
	mesh := filepath.Join(home, ".mesh")
	roots, err := NewOrgRootSet(mesh, scopes.Names())
	if err != nil {
		t.Fatal(err)
	}
	orgs := map[string]*twoOrgsOrg{
		"acme": {login: "acme", orgID: orgID, nodeID: nodeA, acct: acctID, proj: projID, repo: repoID, goal: goalID, planGoal: acmePlanGoal, dir: acmeDir},
		"beta": {login: "beta", orgID: orgB, nodeID: nodeB, acct: betaAcct, proj: betaProj, repo: betaRepo, goal: betaGoal, planGoal: betaPlanGoal, dir: betaDir},
	}
	origin := originRepo(t, nil)
	for name, o := range orgs {
		o.roots, _ = roots.For(name)
		// one clone of the repo per org (ADR-034), each org's own,
		// and something in each org's kept sessions, store and MCP
		// data for the other to try
		gitRun(t, home, "clone", "--quiet", origin, SharedClonePath(o.roots.Repos, name+"/widgets"))
		for _, d := range []string{filepath.Join(o.roots.Kept, "old-session"), filepath.Join(o.roots.Store, "pnpm"), filepath.Join(o.roots.Store, "npm"), filepath.Join(o.roots.MCP, "sessions", "old")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(o.roots.MCP, "sessions", "old", harness.TokenFileName), []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := gw.ClaimWorkItem(context.Background(), map[string]string{"acme": item1, "beta": itemB}[name], o.nodeID, "claude-code"); err != nil {
			t.Fatal(err)
		}
		if err := gw.ClaimDirectionResearch(context.Background(), o.dir, o.nodeID); err != nil {
			t.Fatal(err)
		}
	}
	issueClaude(f, itemB)
	orgs["acme"].item = gateway.WorkItem{ID: item1, OrgID: orgID, GoalID: goalID, RepoID: repoID, Seq: 1, Title: "Write the failing test"}
	orgs["beta"].item = gateway.WorkItem{ID: itemB, OrgID: orgB, GoalID: betaGoal, RepoID: betaRepo, Seq: 1, Title: "Launch the thing"}
	for _, o := range orgs {
		row := f.Item(o.item.ID)
		if n, ok := row["attempts"].(int); ok {
			o.item.Attempts = n
		}
	}

	// each session's other org: everything of it a session must not reach
	other := map[string]string{"acme": "beta", "beta": "acme"}
	forbid := func(org string) map[string]string {
		o := orgs[other[org]]
		m := map[string]string{
			"work":             o.roots.Work,
			"clone":            SharedClonePath(o.roots.Repos, o.login+"/widgets"),
			"clone git dir":    filepath.Join(SharedClonePath(o.roots.Repos, o.login+"/widgets"), ".git"),
			"kept sessions":    o.roots.Kept,
			"pnpm store":       filepath.Join(o.roots.Store, "pnpm"),
			"npm cache":        filepath.Join(o.roots.Store, "npm"),
			"mcp data":         o.roots.MCP,
			"mcp token":        filepath.Join(o.roots.MCP, "sessions", "old", harness.TokenFileName),
			"homes":            filepath.Join(o.roots.Base, "homes"),
			"the node's store": filepath.Join(mesh, "store"),
		}
		if dirs, _ := filepath.Glob(filepath.Join(o.roots.Work, "*", "1")); len(dirs) > 0 {
			m["checkout"] = dirs[0]
		}
		return m
	}
	orgOf := func(job harness.Job) string {
		for name, o := range orgs {
			if underDir(job.Workspace, o.roots.Base) {
				return name
			}
		}
		return ""
	}

	tf := &TokenFiles{Token: gw.Bearer, MultiOrg: func() bool { return len(scopes) > 1 }}
	h := &twoOrgsHarness{podGuardHarness: podGuardHarness{t: t}, tokens: tf, orgOf: orgOf, forbid: forbid, rt: rt}
	h.builds.Add(2)
	var logs syncBuf
	logger := log.New(&logs, "", 0)
	ws := &orgCloneWS{fakeWS: &fakeWS{root: filepath.Join(mesh, "work")}, t: t, origin: origin}
	declines := &Declines{}
	runner := &Runner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopes, WorkRoot: filepath.Join(mesh, "work"),
		Roots: roots, Sandbox: sb, ProgressEvery: time.Hour, Declines: declines,
		// the stand-in agent commits nothing; its build is finished as one
		// that did (a done session with nothing committed is failed)
		HasCommit: func(string, string) bool { return true },
		Bearers: &SessionBearers{GW: gw, Tokens: tf, Audience: MeshMCPURL(f.URL()), Orgs: func() int { return len(scopes) }, Logf: logger.Printf},
		Inspect: func(context.Context, string, ...string) ([]byte, error) { return []byte("sha256:abc\n"), nil }, Log: logger}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// the two builds at once
	var wg sync.WaitGroup
	for _, name := range []string{"acme", "beta"} {
		wg.Add(1)
		go func(o *twoOrgsOrg) {
			defer wg.Done()
			if err := runner.RunItem(ctx, o.item); err != nil {
				t.Errorf("%s build: %v", o.login, err)
			}
		}(orgs[name])
	}
	wg.Wait()

	// then a plan and a research of each
	planner := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: filepath.Join(mesh, "work"), Roots: roots, Sandbox: sb, Declines: declines, Log: logger}
	for _, name := range []string{"acme", "beta"} {
		o := orgs[name]
		sc, _ := scopes.ByOrg(o.orgID)
		h.text = strings.ReplaceAll(planTokenJSON, "acme/widgets", name+"/widgets")
		g := gateway.Goal{ID: o.planGoal, Slug: "plan-" + name, Title: "plan-" + name, ProjectID: o.proj, Status: "planning"}
		if err := planner.Plan(ctx, g, sc); err != nil {
			t.Fatalf("%s plan: %v\n%s", name, err, logs.String())
		}
	}
	h.text = "```json\n{\"goals\": []}\n```"
	director := &Director{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: filepath.Join(mesh, "work"), Roots: roots, Sandbox: sb, Declines: declines, Log: logger}
	for _, name := range []string{"acme", "beta"} {
		o := orgs[name]
		sc, _ := scopes.ByOrg(o.orgID)
		d := gateway.Direction{ID: o.dir, ProjectID: o.proj, OrgID: o.orgID, Slug: "overnight", Direction: "Go on then.", Enabled: true, Status: "active", MaxLiveGoals: 3}
		p := gateway.Project{ID: o.proj, Slug: "mesh", OrgID: o.orgID, RepoIDs: []string{o.repo}}
		if err := director.Research(ctx, d, p, sc); err != nil {
			t.Fatalf("%s research: %v\n%s", name, err, logs.String())
		}
	}

	if h.unsandboxed != 0 {
		t.Fatalf("%d sessions ran with no pod", h.unsandboxed)
	}
	kinds := map[string]map[harness.Kind]int{"acme": {}, "beta": {}}
	for _, s := range h.seen {
		kinds[s.org][s.job.Kind]++
	}
	for name, k := range kinds {
		for _, kind := range []harness.Kind{harness.KindBuild, harness.KindPlan, harness.KindResearch} {
			if k[kind] != 1 {
				t.Fatalf("%s: %d %s sessions in a pod, want 1 (%v)\n%s", name, k[kind], kind, kinds, logs.String())
			}
		}
	}

	buildTokens := map[string]string{}
	for _, s := range h.seen {
		mine, theirs := orgs[s.org], orgs[other[s.org]]
		label := s.org + " " + string(s.job.Kind)

		// the session carried a pod (Job.Session) — plan and research too
		if s.job.Session == nil {
			t.Fatalf("%s: no Sandbox session", label)
		}
		// its mounts: its org's alone
		if s.mountErr != nil {
			t.Fatalf("%s: its own mounts were refused: %v", label, s.mountErr)
		}
		var sources []string
		for _, m := range s.mounts {
			sources = append(sources, m.Source)
			if !underDir(m.Source, mine.roots.Base) {
				t.Errorf("%s mounts %s, outside its org's root %s", label, m.Source, mine.roots.Base)
			}
			if underDir(m.Source, theirs.roots.Base) {
				t.Errorf("%s mounts %s, under %s's root", label, m.Source, theirs.login)
			}
		}
		want := []string{s.job.Workspace, s.job.Session.Home, filepath.Join(mine.roots.Store, "pnpm"), filepath.Join(mine.roots.Store, "npm")}
		if s.job.Kind == harness.KindBuild {
			// a build's worktree mounts its org's clone's git dir
			want = append(want, filepath.Join(SharedClonePath(mine.roots.Repos, mine.login+"/widgets"), ".git"))
		}
		for _, w := range want {
			if !containsPath(sources, w) {
				t.Errorf("%s: mounts %q lack %s", label, sources, w)
			}
		}
		if !underDir(s.job.Workspace, mine.roots.Work) {
			t.Errorf("%s: checkout %s not under %s", label, s.job.Workspace, mine.roots.Work)
		}
		if s.job.KeptRoot != mine.roots.Kept {
			t.Errorf("%s: kept sessions under %q, want %s", label, s.job.KeptRoot, mine.roots.Kept)
		}
		if !underDir(s.tokPath, mine.roots.Base) || underDir(s.tokPath, theirs.roots.Base) {
			t.Errorf("%s: token file %s not under its own org's root", label, s.tokPath)
		}
		// every attempt at the other org's tree refused, no pod made
		for what, err := range s.refused {
			if !errors.Is(err, sandbox.ErrMountOutsideOrg) {
				t.Errorf("%s: mounting %s: %v, want ErrMountOutsideOrg", label, what, err)
			} else if !strings.Contains(err.Error(), mine.login) {
				t.Errorf("%s: refusal %q names no org", label, err)
			}
		}
		if len(s.refused) < 10 {
			t.Errorf("%s: tried %d paths of the other org", label, len(s.refused))
		}
		if s.job.Kind == harness.KindBuild {
			if !anyKeyHasPrefix(s.refused, "checkout ") {
				t.Errorf("%s: the other org's checkout was not tried", label)
			}
		}
		if s.podsBy != 0 {
			t.Errorf("%s: %d pods made for refused mounts", label, s.podsBy)
		}

		// the token file: a build's is its own mesh session's id token,
		// never the machine's; a plan's and a research's are still the
		// node's, as ADR-035 leaves them
		if s.token == "" {
			t.Errorf("%s: empty token file", label)
		}
		if s.job.Kind == harness.KindBuild {
			ms := f.MeshSessionFor(mine.item.ID)
			if ms == nil || len(ms.Minted) == 0 || s.token != ms.Minted[len(ms.Minted)-1] {
				t.Errorf("%s: token file holds %q, want its own session's id token (%+v)", label, s.token, ms)
			}
			if s.token == machine {
				t.Errorf("%s: token file holds the machine bearer", label)
			}
			buildTokens[s.org] = s.token
		} else if s.token != machine {
			t.Errorf("%s: token file holds %q, want the node's bearer (ADR-035 leaves plan and research on it)", label, s.token)
		}
	}
	if buildTokens["acme"] == "" || buildTokens["acme"] == buildTokens["beta"] {
		t.Fatalf("the two orgs' builds hold one bearer: %q", buildTokens)
	}
	for _, o := range orgs {
		if o.roots.Base == "" || !strings.HasPrefix(o.roots.Base, filepath.Join(mesh, OrgsDirName)) {
			t.Fatalf("%s's roots %s are not under ~/.mesh/orgs", o.login, o.roots.Base)
		}
	}
	// and no session's files landed in the node-wide work root
	if entries, _ := os.ReadDir(filepath.Join(mesh, "work")); len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the node-wide work root holds %q", names)
	}
	if strings.Contains(logs.String(), "panic") {
		t.Fatalf("log:\n%s", logs.String())
	}
}

func containsPath(paths []string, p string) bool {
	for _, q := range paths {
		if filepath.Clean(q) == filepath.Clean(p) {
			return true
		}
	}
	return false
}

func anyKeyHasPrefix(m map[string]error, prefix string) bool {
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}
