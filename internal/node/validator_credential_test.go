package node

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// Goal a-node-validates-every-enrolled-repo-with-the-tools-it-needs,
// item 2: the validator's git fetch and the builder's git push go out on
// the item's repo token as the platform (the broker) issues it, by the
// environment, whatever the checkout's own helper holds by then — and
// the shared clone's config never holds a token (mesh ADR 017).
func TestValidatorFetchAndBuilderPushRunOnTheBrokersItemToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_TERMINAL_PROMPT=0"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	t.Setenv("PATH", filepath.Dir(mustLookPath(t, "git")))
	srvRoot := filepath.Join(root, "srv")
	bare := filepath.Join(srvRoot, "acme/widgets.git")
	seedDir := filepath.Join(root, "seed")
	for _, c := range [][]string{{"init", "--quiet", "--bare", "-b", "main", bare}, {"init", "--quiet", "-b", "main", seedDir}, {"-C", seedDir, "commit", "--quiet", "--allow-empty", "-m", "a"}, {"-C", seedDir, "push", "--quiet", bare, "HEAD:main"}} {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	// GitHub honours the tokens the broker issues; ghs_stale, what the
	// checkout's helper holds once the claim's token is gone, it refuses
	srv := gatewaytest.GitHub(t, srvRoot, map[string]string{"ghs_claim": "acme/widgets", "ghs_fetch": "acme/widgets", "ghs_push": "acme/widgets"})

	f := seed(t)
	r, _, item, _ := runnerOnFake(t, f, nil, nil)
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	// within the renewal skew: every use asks the broker again
	soon := func(tok string) {
		f.GitHubTokens[item1] = map[string]any{"token": tok, "expiresAt": time.Now().Add(time.Minute).UTC().Format(time.RFC3339), "repo": "acme/widgets"}
	}
	soon("ghs_claim")

	g := &Git{GitHubURL: srv.URL, ReposRoot: filepath.Join(root, "repos")}
	var mu sync.Mutex
	ran := map[string][]string{} // fetch|push → the token each ran with ("" none)
	g.exec = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 && (args[0] == "fetch" || args[0] == "push") {
			tok := ""
			for _, kv := range gitTokenEnv(ctx) {
				if _, after, ok := strings.Cut(kv, "password="); ok {
					tok, _, _ = strings.Cut(after, ";")
				}
			}
			mu.Lock()
			ran[args[0]] = append(ran[args[0]], tok)
			mu.Unlock()
		}
		return g.realRun(ctx, dir, name, args...)
	}
	r.WS = g
	ctx := context.Background()
	spec := WorkspaceSpec{Root: filepath.Join(root, "work"), GoalSlug: "g", Seq: 1, Repo: "acme/widgets", Branch: BranchName("g", 1), BaseRef: "main"}
	cred, err := NewItemCred(ctx, r.GW, g, r.Scopes, item, spec, nil, r.logf)
	if err != nil {
		t.Fatalf("item cred: %v", err)
	}
	dir, err := g.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !isWorktree(dir) {
		t.Fatalf("%s is not a worktree", dir)
	}
	// the checkout's helper holds a token GitHub no longer honours, as
	// after the claim's token expired or another session rewrote it
	if err := writeHelper(ctx, dir, srv.URL, credentialHelper("ghs_stale")); err != nil {
		t.Fatal(err)
	}
	// another push lands on the branch: the newer head the validator fetches
	gitOut(t, seedDir, "commit", "--quiet", "--allow-empty", "-m", "elsewhere")
	gitOut(t, seedDir, "push", "--quiet", bare, "HEAD:"+spec.Branch)
	newer := gitOut(t, seedDir, "rev-parse", "HEAD")

	vr := r.validator()
	target := validationTarget{item: item, dir: dir, base: "main", prefix: "item 1", label: "1"}
	if err := vr.fetchHead(ctx, target, spec.Branch, newer); err == nil || !IsCredentialRefusal(err) {
		t.Fatalf("with no item token the stale helper's fetch = %v, want GitHub's refusal", err)
	}
	ran = map[string][]string{}

	issued := func() int { return len(f.CallsNamed("issueItemRepoToken")) }
	target.cred = cred
	soon("ghs_fetch")
	before := issued()
	if err := vr.fetchHead(ctx, target, spec.Branch, newer); err != nil {
		t.Fatalf("validator fetch on the item token: %v\n%s", err, logs.String())
	}
	if _, _, err := vr.changedPaths(ctx, target); err != nil {
		t.Fatalf("validator diff base fetch on the item token: %v\n%s", err, logs.String())
	}
	if issued() <= before {
		t.Fatal("the validator's fetch never asked the broker for the item's token")
	}
	if got := ran["fetch"]; len(got) == 0 || !allEqual(got, "ghs_fetch") {
		t.Fatalf("validator fetches ran with tokens %q, want every one ghs_fetch", got)
	}

	// the build's commit, pushed on the broker's token
	gitOut(t, dir, "commit", "--quiet", "--allow-empty", "-m", "the build")
	soon("ghs_push")
	before = issued()
	b := &buildRun{item: item, spec: spec, cred: cred, dir: dir, label: "1"}
	sha, err := r.pushBuild(ctx, b)
	if err != nil {
		t.Fatalf("push on the item token: %v\n%s", err, logs.String())
	}
	if issued() <= before {
		t.Fatal("the builder's push never asked the broker for the item's token")
	}
	if got := ran["push"]; len(got) != 1 || got[0] != "ghs_push" {
		t.Fatalf("pushes ran with tokens %q, want one on ghs_push", got)
	}
	if at := gitOut(t, bare, "rev-parse", "refs/heads/"+spec.Branch); at != sha {
		t.Fatalf("origin's %s is %s, the push answered %s", spec.Branch, at, sha)
	}

	// and the shared clone's config holds none of them
	clone := SharedClonePath(g.ReposRoot, "acme/widgets")
	cfg, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "ghs_") {
		t.Fatalf("the shared clone's config holds a token:\n%s", cfg)
	}
}

func allEqual(xs []string, want string) bool {
	for _, x := range xs {
		if x != want {
			return false
		}
	}
	return true
}
