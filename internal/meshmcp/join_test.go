package meshmcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
)

const (
	jOrgID  = "0f3b8a1e-2c4d-4e6f-8a9b-0c1d2e3f4a5b"
	jAcctID = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	jProjID = "bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb"
	jRepoID = "cccccccc-1111-4111-8111-cccccccccccc"
	jGoalID = "dddddddd-1111-4111-8111-dddddddddddd"
	jItemID = "eeeeeeee-1111-4111-8111-eeeeeeeeeeee"
	jNodeID = "22222222-2222-4222-8222-222222222222"
)

// joinWorld is a platform with one ready item on acme/widgets, a GitHub
// serving acme/widgets from a local bare origin — the operator's global
// helper reaching it too — and a temp HOME.
type joinWorld struct {
	fake    *gatewaytest.Fake
	github  string
	home    string
	root    string
	origin  string
	persona *Personal
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// originRepo is a bare origin under srvRoot/<repo>.git holding one
// commit on main (internal/node/runner_test.go's, at GitHub's path).
func originRepo(t *testing.T, srvRoot, repo string, files map[string]string) string {
	t.Helper()
	origin := filepath.Join(srvRoot, repo+".git")
	gitIn(t, filepath.Dir(srvRoot), "init", "--quiet", "--bare", "-b", "main", origin)
	seedDir := filepath.Join(t.TempDir(), "seed")
	gitIn(t, filepath.Dir(seedDir), "clone", "--quiet", origin, seedDir)
	for k, v := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(seedDir, k)), 0o755)
		os.WriteFile(filepath.Join(seedDir, k), []byte(v), 0o644)
	}
	gitIn(t, seedDir, "add", "-A")
	gitIn(t, seedDir, "commit", "--quiet", "-m", "base")
	gitIn(t, seedDir, "push", "--quiet", "origin", "HEAD:main")
	return origin
}

func newJoinWorld(t *testing.T) *joinWorld {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	os.WriteFile(global, []byte("[credential]\n\thelper = \"!f() { echo username=operator; echo password=operator-login; }; f\"\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_TERMINAL_PROMPT=0"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o755)
	t.Setenv("HOME", home)

	srvRoot := filepath.Join(root, "srv")
	origin := originRepo(t, srvRoot, "acme/widgets", map[string]string{"AGENTS.md": "# widgets\n", "api/api.go": "package api\n"})
	srv := gatewaytest.GitHub(t, srvRoot, map[string]string{"ghs_item": "acme/widgets", "operator-login": "*"})

	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	f.Orgs = []map[string]any{{"id": jOrgID, "login": "acme", "accountId": jAcctID}}
	f.Accounts[jAcctID] = map[string]any{"id": jAcctID, "login": "acme"}
	f.Projects[jProjID] = map[string]any{"id": jProjID, "orgId": jOrgID, "slug": "mesh", "name": "Mesh"}
	f.Repos[jRepoID] = map[string]any{"id": jRepoID, "accountId": jAcctID, "name": "widgets", "defaultBranch": "main"}
	f.Goals[jGoalID] = map[string]any{"id": jGoalID, "orgId": jOrgID, "projectId": jProjID, "slug": "ship-it", "title": "Ship it", "status": "running",
		"statement": "Ship the widgets.", "acceptance": "(1) widgets ship."}
	f.WorkItems[jItemID] = map[string]any{"id": jItemID, "goalId": jGoalID, "orgId": jOrgID, "seq": 1, "title": "Write the failing test",
		"brief": "Write the failing test in api/.", "acceptance": "go test ./api/ fails, then passes.",
		"repoId": jRepoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	f.GitHubTokens[jItemID] = map[string]any{"token": "ghs_item", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repository": "acme/widgets"}
	f.Documents["acme/mesh"] = []map[string]any{{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/widgets", "path": "AGENTS.md"}}
	f.Bodies["acme/mesh/agents"] = "# widgets\n"

	gw := gateway.New(f.URL(), "rs_test")
	notRepo := filepath.Join(root, "elsewhere")
	os.MkdirAll(notRepo, 0o755)
	p := &Personal{GW: gw, Join: &Join{
		GW: gw, WS: &node.Git{GitHubURL: srv.URL}, Cwd: notRepo,
		NodeID: func(context.Context) (string, error) { return jNodeID, nil },
		Logf:   t.Logf,
	}}
	return &joinWorld{fake: f, github: srv.URL, home: home, root: root, origin: origin, persona: p}
}

func (w *joinWorld) join(t *testing.T, args map[string]any) string {
	t.Helper()
	for _, tool := range w.persona.Tools() {
		if tool.Name == "mesh_join" {
			out, err := tool.Call(context.Background(), args)
			if err != nil {
				t.Fatalf("mesh_join: %v", err)
			}
			return out
		}
	}
	t.Fatal("no mesh_join tool")
	return ""
}

func decodeJoin(t *testing.T, out string) JoinAnswer {
	t.Helper()
	var a JoinAnswer
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatalf("answer %q: %v", out, err)
	}
	return a
}

// mesh_join claims the goal's first ready item and makes its checkout
// under ~/.mesh/work/<goal>/<seq> on the item's branch, the item token
// on the checkout's own credential helper, the context written, and
// mcp-state.json holding it.
func TestJoinPreparesTheCheckoutOnTheItemToken(t *testing.T) {
	w := newJoinWorld(t)
	raw := w.join(t, map[string]any{"goal": "ship-it"})
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("answer %q: %v", raw, err)
	}
	for _, k := range []string{"path", "branch", "brief", "acceptance", "context", "item"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("answer has no %s: %s", k, raw)
		}
	}
	a := decodeJoin(t, raw)
	want := filepath.Join(w.home, ".mesh", "work", "ship-it", "1")
	if a.Path != want || a.Branch != "mesh/ship-it-1" {
		t.Fatalf("path %s branch %s", a.Path, a.Branch)
	}
	if a.Brief != "Write the failing test in api/." || a.Acceptance != "go test ./api/ fails, then passes." {
		t.Fatalf("brief %q acceptance %q", a.Brief, a.Acceptance)
	}
	if a.Item != (JoinItem{ID: jItemID, Goal: "ship-it", Seq: 1, Title: "Write the failing test"}) {
		t.Fatalf("item %+v", a.Item)
	}
	if got := gitIn(t, a.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "mesh/ship-it-1" {
		t.Fatalf("HEAD is %s", got)
	}
	cfg := exec.Command("git", "config", "--local", "--get-all", "credential."+w.github+".helper")
	cfg.Dir = a.Path
	out, err := cfg.Output()
	if err != nil {
		t.Fatal(err)
	}
	helpers := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(helpers) != 2 || helpers[0] != "" || !strings.Contains(helpers[1], "password=ghs_item") || !strings.Contains(helpers[1], "username=x-access-token") {
		t.Fatalf("helpers: %q", helpers)
	}
	if _, err := os.Stat(filepath.Join(a.Path, ".mesh", "context", "INDEX.md")); err != nil {
		t.Fatal(err)
	}
	instr, _ := filepath.Glob(filepath.Join(a.Path, ".mesh", "context", "instructions-*.md"))
	if len(instr) != 1 {
		t.Fatalf("instructions files: %v", instr)
	}
	if len(a.Context) != 2 {
		t.Fatalf("context: %v", a.Context)
	}
	if n := len(w.fake.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimWorkItem %d times", n)
	}
	if n := len(w.fake.CallsNamed("issueItemRepoToken")); n != 1 {
		t.Fatalf("issueItemRepoToken %d times", n)
	}
	if c := w.fake.CallsNamed("claimWorkItem")[0]; c.Input["nodeId"] != jNodeID || c.Input["harness"] != JoinHarness {
		t.Fatalf("claim: %v", c.Input)
	}

	s, err := ReadState(filepath.Join(w.home, ".mesh", StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Held) != 1 || s.Held[0].ItemID != jItemID || s.Held[0].Goal != "ship-it" || s.Held[0].Seq != 1 ||
		s.Held[0].Path != a.Path || s.Held[0].Branch != a.Branch || s.Held[0].ClaimedAt.IsZero() {
		t.Fatalf("state: %+v", s)
	}
	status, err := w.persona.status(context.Background(), nil)
	if err != nil || !strings.Contains(status, `"branch": "mesh/ship-it-1"`) {
		t.Fatalf("mesh_status = %q, %v", status, err)
	}
}

// Run from a checkout of the item's repo, mesh_join works there: the
// branch checked out and the helper written in the cwd's own checkout.
func TestJoinUsesTheCwdWhenItIsTheRepo(t *testing.T) {
	w := newJoinWorld(t)
	clone := filepath.Join(w.root, "my-widgets")
	gitIn(t, w.root, "clone", "--quiet", w.origin, clone)
	w.persona.Join.Cwd = filepath.Join(clone, "api")
	a := decodeJoin(t, w.join(t, map[string]any{"goal": "ship-it", "item": float64(1)}))
	if a.Path != clone {
		t.Fatalf("path %s, want the cwd's checkout %s", a.Path, clone)
	}
	if got := gitIn(t, clone, "rev-parse", "--abbrev-ref", "HEAD"); got != "mesh/ship-it-1" {
		t.Fatalf("HEAD is %s", got)
	}
	if helpers := gitIn(t, clone, "config", "--local", "--get-all", "credential."+w.github+".helper"); !strings.Contains(helpers, "ghs_item") {
		t.Fatalf("helpers: %q", helpers)
	}
	if got := gitIn(t, clone, "remote", "get-url", "origin"); got != w.origin {
		t.Fatalf("origin rewritten to %s", got)
	}
	if _, err := os.Stat(filepath.Join(clone, ".mesh", "context", "INDEX.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.home, ".mesh", "work", "ship-it", "1")); err == nil {
		t.Fatal("a work-root checkout was made too")
	}
}

// A claim the platform refuses is the tool's answer, not an error.
func TestJoinClaimRefusedIsTheAnswer(t *testing.T) {
	w := newJoinWorld(t)
	w.fake.Refuse["claimWorkItem"] = "the item is held by another node"
	out := w.join(t, map[string]any{"goal": "ship-it", "item": "1"})
	if !strings.HasPrefix(out, "claim refused:") {
		t.Fatalf("answer %q", out)
	}
	if n := len(w.fake.CallsNamed("issueItemRepoToken")); n != 0 {
		t.Fatalf("issueItemRepoToken asked %d times after a refusal", n)
	}
}

func TestJoinOriginNames(t *testing.T) {
	for _, c := range []struct {
		url  string
		want bool
	}{
		{"https://github.com/acme/widgets.git", true},
		{"git@github.com:Acme/Widgets.git", true},
		{"/srv/acme/widgets.git", true},
		{"https://github.com/acme/widgets-two.git", false},
		{"https://github.com/other/widgets", false},
		{"", false},
	} {
		if got := OriginNames(c.url, "acme/widgets"); got != c.want {
			t.Errorf("OriginNames(%q) = %v", c.url, got)
		}
	}
}
