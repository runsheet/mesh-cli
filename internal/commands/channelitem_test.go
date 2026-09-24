package commands

import (
	"context"
	"fmt"
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
	chOrgID  = "0f3b8a1e-2c4d-4e6f-8a9b-0c1d2e3f4a5b"
	chAcctID = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	chProjID = "bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb"
	chRepoID = "cccccccc-1111-4111-8111-cccccccccccc"
	chGoalID = "dddddddd-1111-4111-8111-dddddddddddd"
	chItemID = "eeeeeeee-1111-4111-8111-eeeeeeeeeeee"
	chNodeID = "22222222-2222-4222-8222-222222222222"
)

// channelWorld is a platform with one claimed item on acme/widgets, a
// GitHub holding acme/widgets and other/gadgets — the operator's own
// global git helper reaching both — and the node's workspace on it.
type channelWorld struct {
	fake    *gatewaytest.Fake
	gw      *gateway.Client
	ws      *node.Git
	scopes  node.Scopes
	item    gateway.WorkItem
	root    string
	srvRoot string
	git     func(dir string, args ...string) (string, error)
}

func newChannelWorld(t *testing.T, tok map[string]any) *channelWorld {
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
	w := &channelWorld{root: root, srvRoot: filepath.Join(root, "srv")}
	w.git = func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(dir string, args ...string) {
		t.Helper()
		if out, err := w.git(dir, args...); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	for _, repo := range []string{"acme/widgets", "other/gadgets"} {
		bare := filepath.Join(w.srvRoot, repo+".git")
		must(root, "init", "--quiet", "--bare", "-b", "main", bare)
		seed := filepath.Join(root, "seed-"+strings.ReplaceAll(repo, "/", "-"))
		must(root, "init", "--quiet", "-b", "main", seed)
		os.WriteFile(filepath.Join(seed, "a"), []byte(repo), 0o644)
		must(seed, "add", "a")
		must(seed, "commit", "--quiet", "-m", "a")
		must(seed, "push", "--quiet", bare, "HEAD:main")
	}
	srv := gatewaytest.GitHub(t, w.srvRoot, map[string]string{"ghs_item": "acme/widgets", "ghs_renewed": "acme/widgets", "operator-login": "*"})
	w.ws = &node.Git{GitHubURL: srv.URL}

	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	f.Orgs = []map[string]any{{"id": chOrgID, "login": "acme", "accountId": chAcctID}}
	f.Accounts[chAcctID] = map[string]any{"id": chAcctID, "login": "acme"}
	f.Projects[chProjID] = map[string]any{"id": chProjID, "orgId": chOrgID, "slug": "mesh", "name": "Mesh"}
	f.Repos[chRepoID] = map[string]any{"id": chRepoID, "accountId": chAcctID, "name": "widgets", "defaultBranch": "main"}
	f.Goals[chGoalID] = map[string]any{"id": chGoalID, "orgId": chOrgID, "projectId": chProjID, "slug": "ship-it", "title": "Ship it", "status": "running"}
	f.WorkItems[chItemID] = map[string]any{"id": chItemID, "goalId": chGoalID, "orgId": chOrgID, "seq": 1, "title": "Write the failing test",
		"repoId": chRepoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	if tok != nil {
		f.GitHubTokens[chItemID] = tok
	} else {
		f.Unsupported["issueItemRepoToken"] = true
	}
	w.fake = f
	w.gw = gateway.New(f.URL(), "rs_test")
	if err := w.gw.ClaimWorkItem(context.Background(), chItemID, chNodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	w.scopes = node.Scopes{{Login: "acme", ID: chOrgID, AccountID: chAcctID, NodeID: chNodeID}}
	w.item = gateway.WorkItem{ID: chItemID, OrgID: chOrgID, GoalID: chGoalID, RepoID: chRepoID, Seq: 1, Title: "Write the failing test"}
	return w
}

func (w *channelWorld) ready(t *testing.T, logf func(string, ...any)) *channelItem {
	t.Helper()
	if logf == nil {
		logf = t.Logf
	}
	ready, err := readyChannelItem(context.Background(), w.gw, w.ws, w.scopes, w.item, filepath.Join(w.root, "work"), logf)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

// The event channel mode pushes names a workspace already cloned on the
// item's own token: its branch pushes to its repo, and a push from
// inside it to a second repo fails, though the operator's global helper
// would reach that one.
func TestChannelDispatchCredentialsTheWorkspaceBeforeTheEvent(t *testing.T) {
	w := newChannelWorld(t, map[string]any{"token": "ghs_item", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repository": "acme/widgets"})
	ready := w.ready(t, nil)
	if ready.Meta["kind"] != "work_item.ready" || ready.Cred == nil || ready.Cred.Token().Token != "ghs_item" {
		t.Fatalf("ready: %v cred %+v", ready.Meta, ready.Cred)
	}
	dir := ready.Meta["workspace"]
	if branch, _ := w.git(dir, "branch", "--show-current"); branch != "mesh/ship-it-1" {
		t.Fatalf("the workspace is on %q", branch)
	}
	var issued int
	for _, c := range w.fake.Calls {
		if c.Name == "issueItemRepoToken" {
			issued++
		}
	}
	if issued != 1 {
		t.Fatalf("issueItemRepoToken asked %d times", issued)
	}
	// what the session does with it: commit, push its branch
	os.WriteFile(filepath.Join(dir, "b"), []byte("b"), 0o644)
	if out, err := w.git(dir, "add", "b"); err != nil {
		t.Fatal(out)
	}
	if out, err := w.git(dir, "commit", "--quiet", "-m", "b"); err != nil {
		t.Fatal(out)
	}
	if out, err := w.git(dir, "push", "--quiet", "-u", "origin", "mesh/ship-it-1"); err != nil {
		t.Fatalf("push to the item's own repo: %v: %s", err, out)
	}
	if out, err := w.git(dir, "push", strings.TrimSuffix(w.ws.GitHubURL, "/")+"/other/gadgets.git", "HEAD:refs/heads/stolen"); err == nil {
		t.Fatalf("a push to a second repo must fail: %s", out)
	}
	if _, err := w.git(w.root, "--git-dir", filepath.Join(w.srvRoot, "other/gadgets.git"), "rev-parse", "--verify", "stolen"); err == nil {
		t.Fatal("the second repo took the push")
	}
}

// A held item's token near its expiry is renewed by the channel's
// bookkeeping and its workspace's helper rewritten.
func TestChannelRefreshesAHeldItemsToken(t *testing.T) {
	w := newChannelWorld(t, map[string]any{"token": "ghs_item", "expiresAt": time.Now().Add(time.Minute).UTC().Format(time.RFC3339), "repository": "acme/widgets"})
	ready := w.ready(t, nil)
	w.fake.GitHubTokens[chItemID] = map[string]any{"token": "ghs_renewed", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repository": "acme/widgets"}
	refreshItemCreds(context.Background(), map[string]*node.ItemCred{chItemID: ready.Cred}, func(f string, a ...any) { t.Errorf(f, a...) })
	if got := ready.Cred.Token().Token; got != "ghs_renewed" {
		t.Fatalf("token after refresh = %s", got)
	}
	helpers, _ := w.git(ready.Meta["workspace"], "config", "--local", "--get-all", "credential."+w.ws.GitHubURL+".helper")
	if strings.Contains(helpers, "ghs_item") || !strings.Contains(helpers, "ghs_renewed") {
		t.Fatalf("helpers after refresh:\n%s", helpers)
	}
	// a token with time to spare is left alone
	w.fake.GitHubTokens[chItemID] = map[string]any{"token": "ghs_third", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repository": "acme/widgets"}
	refreshItemCreds(context.Background(), map[string]*node.ItemCred{chItemID: ready.Cred}, t.Errorf)
	if got := ready.Cred.Token().Token; got != "ghs_renewed" {
		t.Fatalf("a fresh token was renewed: %s", got)
	}
}

// On a platform with no item tokens the item still runs, on the
// operator's login, which its checkout asks the operator's gh for at
// the operator's own config dir — the session's GH_CONFIG_DIR is empty.
func TestChannelDispatchFallsBackOnAnOldPlatform(t *testing.T) {
	w := newChannelWorld(t, nil)
	operatorGH := filepath.Join(w.root, "operator-gh")
	t.Setenv("GH_CONFIG_DIR", operatorGH)
	// no gh here: stand a clone in place, as the operator's gh would have made it
	spec := node.WorkspaceSpec{Root: filepath.Join(w.root, "work"), GoalSlug: "ship-it", Seq: 1}
	if out, err := w.git(w.root, "clone", "--quiet", filepath.Join(w.srvRoot, "acme/widgets.git"), spec.Dir()); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	var logs []string
	ready := w.ready(t, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	if ready.Cred != nil {
		t.Fatalf("an old platform issues no token: %+v", ready.Cred)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "operator's own gh login") {
		t.Fatalf("the fallback must be said once: %q", logs)
	}
	helpers, _ := w.git(ready.Meta["workspace"], "config", "--local", "--get-all", "credential."+w.ws.GitHubURL+".helper")
	// the empty helper first (trimmed away here) drops the operator's global one
	if want := "!GH_CONFIG_DIR='" + operatorGH + "' gh auth git-credential"; helpers != want {
		t.Fatalf("fallback helpers = %q, want %q", helpers, want)
	}
}
