//go:build e2e

package node

import (
	"context"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// baseGoneE2EYML has one suite per directory the history touches: the
// dependency's web/, the item's api/.
const baseGoneE2EYML = `suites:
  - name: web
    kind: unit
    command: "true"
    paths: ["web/**"]
  - name: api
    kind: unit
    command: "true"
    paths: ["api/**"]
`

// mergeMidBuild is a build session that commits its turn and then, before
// the node validates, has the dependency merge: after(), once.
type mergeMidBuild struct {
	*commitHarness
	after func()
}

func (h *mergeMidBuild) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	res, err := h.commitHarness.Run(ctx, job, on)
	if h.after != nil {
		h.after()
		h.after = nil
	}
	return res, err
}

// An item whose dependency merges mid-build validates on main and is
// evaluated (ADR-033, goal
// a-merged-dependencys-gone-branch-fails-no-validation): against a local
// bare origin served as GitHub, the node's own Git prepares item 2 of a
// goal from item 1's branch, mesh/ship-it-1; the session commits, and
// before validation item 1 squash-merges into main and its branch is
// deleted on origin, as GitHub does. The node still validates, uploads an
// attestation for item 2's head and finishes it; the evaluator then diffs
// it against origin/main and it passes. node.log carries the fallback
// lines of validation, of the evaluation's checkout and of its diff, and
// no `changed paths:` failure.
func TestBaseGoneMergedDependencyValidatesOnMainE2E(t *testing.T) {
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

	// GitHub: acme/widgets with main and the dependency's branch
	base := filepath.Join(root, "gh")
	origin := filepath.Join(base, "acme", "widgets.git")
	seedDir := filepath.Join(root, "seed")
	gitRun(t, root, "init", "--quiet", "--bare", "-b", "main", origin)
	gitRun(t, root, "init", "--quiet", "-b", "main", seedDir)
	writeFiles(t, seedDir, map[string]string{".runsheet/validate.yml": baseGoneE2EYML, "api/api.go": "package api\n", "web/web.go": "package web\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "main")
	gitRun(t, seedDir, "push", "--quiet", origin, "HEAD:main")
	dep := BranchName("ship-it", 1)
	gitRun(t, seedDir, "checkout", "--quiet", "-b", dep)
	writeFiles(t, seedDir, map[string]string{"web/dep.go": "package web\n\nconst Dep = 1\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "the dependency")
	gitRun(t, seedDir, "push", "--quiet", origin, "HEAD:"+dep)

	// the platform: item 1 in review on its branch, item 2 ready on it
	f := seed(t)
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	f.SetItem(item1, map[string]any{"status": "review", "branch": dep})
	f.SetItem(item2, map[string]any{"status": "ready", "baseRef": dep})
	issueClaude(f, item2)
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.ClaimWorkItem(context.Background(), item2, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuf{}
	lg := log.New(logs, "", 0)
	g := &Git{GitHubURL: "file://" + base, ReposRoot: filepath.Join(root, "repos"), Logf: lg.Printf, Log: io.Discard}
	work := filepath.Join(root, "work")

	// mid-build: item 1 squash-merges into main and its branch goes
	merged := false
	h := &mergeMidBuild{commitHarness: &commitHarness{t: t, turns: []map[string]string{{"api/item.go": "package api\n\nconst Item = 2\n"}}}}
	h.after = func() {
		gitRun(t, seedDir, "checkout", "--quiet", "main")
		gitRun(t, seedDir, "merge", "--quiet", "--squash", dep)
		gitRun(t, seedDir, "commit", "--quiet", "-m", "the dependency (#1)")
		gitRun(t, seedDir, "push", "--quiet", origin, "HEAD:main")
		gitRun(t, seedDir, "push", "--quiet", origin, "--delete", dep)
		f.SetItem(item1, map[string]any{"status": "merged"})
		merged = true
	}
	r := &Runner{GW: gw, Harness: h, WS: g, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: work, ProgressEvery: time.Hour, Log: lg,
		Credentials: &Credentials{GW: gw, Log: lg}}

	item, err := gw.WorkItem(context.Background(), item2)
	if err != nil {
		t.Fatal(err)
	}
	if item.BaseRef != dep {
		t.Fatalf("item 2's baseRef = %q, want %s", item.BaseRef, dep)
	}
	if err := r.RunItem(context.Background(), *item); err != nil {
		t.Fatalf("RunItem: %v\n%s", err, logs.String())
	}
	if !merged {
		t.Fatal("the dependency never merged mid-build")
	}
	if out, err := exec.Command("git", "ls-remote", "--heads", origin, dep).Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("origin still has %s: %q %v", dep, out, err)
	}

	// the head pushed is the one attested and finished
	branch := BranchName("ship-it", 2)
	head := strings.Fields(gitRun(t, root, "ls-remote", "--heads", origin, branch)+" ")[0]
	if head == "" {
		t.Fatalf("origin has no %s\n%s", branch, logs.String())
	}
	a := uploadedAttestation(t, f)
	if a["commit"] != head {
		t.Fatalf("attested %v, want item 2's head %s\n%s", a["commit"], head, logs.String())
	}
	if got := strings.Join(attestationSteps(t, f.CallsNamed("recordValidationAttestation")[0]), ","); !strings.Contains(got, "api") {
		t.Fatalf("attested suites %q lack the item's api\n%s", got, logs.String())
	}
	finish := f.CallsNamed("finishWorkItem")
	if len(finish) != 1 || finish[0].Input["headSha"] != head {
		t.Fatalf("finish = %v, want head %s\n%s", finish, head, logs.String())
	}
	if len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatalf("item 2 was failed:\n%s", logs.String())
	}

	// the evaluation, on its own checkout of the item's branch
	evh := &fakeHarness{text: "The item's change holds.\nVERDICT: PASS"}
	ev := &Evaluator{GW: gw, Harness: evh, WS: g, Scopes: scopesOf(cfg), WorkRoot: work, Model: "sonnet",
		Thresholds: func() LimitsConfig { return LimitsConfig{} }, Credentials: &Credentials{GW: gw, Log: lg}, Log: lg}
	item, err = gw.WorkItem(context.Background(), item2)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "evaluating" {
		t.Fatalf("item 2 is %s after its finish, want evaluating", item.Status)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatalf("Evaluate: %v\n%s", err, logs.String())
	}
	evals := f.CallsNamed("evaluateWorkItem")
	if len(evals) != 1 || evals[0].Input["passed"] != true {
		t.Fatalf("evaluations = %+v, want one passing\n%s", evals, logs.String())
	}
	if len(evh.jobs) != 1 {
		t.Fatalf("evaluator sessions = %d, want one", len(evh.jobs))
	}
	prompt := evh.jobs[0].Prompt
	if !strings.Contains(prompt, "origin/main") || strings.Contains(prompt, "origin/"+dep) || !strings.Contains(prompt, "api/item.go") {
		t.Fatalf("the evaluator was not given origin/main's diff:\n%s", prompt)
	}

	out := logs.String()
	for _, line := range []string{
		"item ship-it/2: validation: base " + dep + " is gone (merged); changed paths from origin/main",
		"item ship-it/2: evaluate: base " + dep + " is gone (merged); diff from origin/main",
		filepath.Join(work, "ship-it", "2-eval") + ": base " + dep + " is gone (merged); continuing origin/" + branch,
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("node.log lacks %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "changed paths: ") || strings.Contains(out, "could not diff") {
		t.Fatalf("node.log carries a changed paths or diff failure:\n%s", out)
	}
	t.Logf("node.log:\n%s", out)
}
