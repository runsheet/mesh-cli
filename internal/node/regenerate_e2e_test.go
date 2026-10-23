//go:build e2e

package node

import (
	"context"
	"fmt"
	"io"
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
)

// regenE2EGenerate is the fixture repo's generator: gen/out.txt is one
// line made of schema.txt's fields and extra.txt, so a change to either
// on each side conflicts in it. `--check` regenerates in memory and
// exits 1 when gen/out.txt is stale.
const regenE2EGenerate = `#!/bin/sh
set -e
out="fields: $(tr '\n' ' ' < schema.txt)| extra: $(cat extra.txt)"
if [ "$1" = "--check" ]; then
  [ "$(cat gen/out.txt)" = "$out" ] || { echo "gen/out.txt is stale" >&2; exit 1; }
  exit 0
fi
mkdir -p gen
printf '%s\n' "$out" > gen/out.txt
`

// regenE2EBuild is the fixture's build: field b and extra y together are
// the silent duplicate an auto-merge leaves, caught only by the check.
const regenE2EBuild = `#!/bin/sh
if grep -qx b schema.txt && grep -qx y extra.txt; then echo "field b and extra y clash" >&2; exit 1; fi
`

// regenE2EValidate is the fixture's validate.yml: one always suite, and a
// generated block whose check is check (none: no generated block).
func regenE2EValidate(check ...string) string {
	y := "suites:\n  - name: generated\n    kind: lint\n    command: ./generate --check\n    always: true\n"
	if len(check) == 0 {
		return y
	}
	y += "generated:\n  paths: [\"gen/**\"]\n  generate: [\"./generate\"]\n  check:\n"
	for _, c := range check {
		y += fmt.Sprintf("    - %q\n", c)
	}
	return y
}

// regenFixture is GitHub with acme/widgets: main at base, the item's
// branch adding field b to the schema with gen/out.txt regenerated,
// then main moving on — extra.txt to y, regenerated — so the branch
// conflicts with main in gen/out.txt and, when mainSchema is set, in
// schema.txt too.
type regenFixture struct {
	root, origin, seedDir string
	git                   *Git
	work                  string
	branch                string
	built, mainHead       string
	logs                  *syncBuf
}

func newRegenFixture(t *testing.T, validateYML string, mainSchema bool) *regenFixture {
	t.Helper()
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
	fx := &regenFixture{root: root, branch: BranchName("ship-it", 1), logs: &syncBuf{}}
	base := filepath.Join(root, "gh")
	fx.origin = filepath.Join(base, "acme", "widgets.git")
	fx.seedDir = filepath.Join(root, "seed")
	gitRun(t, root, "init", "--quiet", "--bare", "-b", "main", fx.origin)
	gitRun(t, root, "init", "--quiet", "-b", "main", fx.seedDir)
	commit := func(msg string, files map[string]string) {
		writeFiles(t, fx.seedDir, files)
		for _, s := range []string{"generate", "build"} {
			if err := os.Chmod(filepath.Join(fx.seedDir, s), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		fx.sh(t, fx.seedDir, "./generate")
		gitRun(t, fx.seedDir, "add", "-A")
		gitRun(t, fx.seedDir, "commit", "--quiet", "-m", msg)
	}
	commit("main", map[string]string{"generate": regenE2EGenerate, "build": regenE2EBuild, "schema.txt": "a\n", "extra.txt": "x\n",
		".runsheet/validate.yml": validateYML})
	gitRun(t, fx.seedDir, "push", "--quiet", fx.origin, "HEAD:main")

	// the item: field b, gen/out.txt regenerated, pushed to its branch
	gitRun(t, fx.seedDir, "checkout", "--quiet", "-b", fx.branch)
	commit("the item adds field b", map[string]string{"schema.txt": "a\nb\n"})
	gitRun(t, fx.seedDir, "push", "--quiet", fx.origin, "HEAD:refs/heads/"+fx.branch)
	fx.built = gitRun(t, fx.seedDir, "rev-parse", "HEAD")

	// main moves on: extra y (and, for a non-generated conflict, field c
	// where the branch put b), regenerated
	gitRun(t, fx.seedDir, "checkout", "--quiet", "main")
	files := map[string]string{"extra.txt": "y\n"}
	if mainSchema {
		files["schema.txt"] = "a\nc\n"
	}
	commit("main moves on", files)
	gitRun(t, fx.seedDir, "push", "--quiet", fx.origin, "HEAD:main")
	fx.mainHead = gitRun(t, fx.seedDir, "rev-parse", "HEAD")

	fx.git = &Git{GitHubURL: "file://" + base, ReposRoot: filepath.Join(root, "repos"), Log: fx.logs}
	fx.work = filepath.Join(root, "work")
	return fx
}

// sh runs a command in dir, failing the test on a non-zero exit.
func (fx *regenFixture) sh(t *testing.T, dir, command string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s in %s: %v: %s", command, dir, err, out)
	}
	return string(out)
}

// originHead is what origin has for the item's branch.
func (fx *regenFixture) originHead(t *testing.T) string {
	t.Helper()
	return strings.Fields(gitRun(t, fx.root, "ls-remote", "--heads", fx.origin, fx.branch) + " ")[0]
}

// rebase claims and runs the item's requested rebase on a node over
// fx's GitHub with h as its harness and creds' Session read in front of
// the platform's.
func (fx *regenFixture) rebase(t *testing.T, h harness.Harness, wrap func(CredentialGateway) CredentialGateway) *gatewaytest.Fake {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := seed(t)
	requestRebase(f, item1, fx.branch)
	f.RebaseTokens = true
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	if _, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "test-node", Slug: "test-node"}); err != nil {
		t.Fatal(err)
	}
	w := log.New(io.Writer(fx.logs), "", 0)
	creds := testCreds(gw)
	creds.GW = wrap(creds.GW)
	rb := &Rebaser{Credentials: creds, GW: gw, Harness: h, WS: fx.git, Scopes: scopesOf(cfg), WorkRoot: fx.work, MCP: MeshContextHTTP(f.URL(), "rs_test"), MaxBudgetUSD: 4, ProgressEvery: time.Hour, Log: w}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: w,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Rebase:   func(ctx context.Context, item gateway.WorkItem) { _ = rb.Rebase(ctx, item) }}
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimWorkItemRebase")); n != 1 {
		t.Fatalf("rebase claims = %d, want 1\n%s", n, fx.logs.String())
	}
	return f
}

// countingCreds is the platform's credential read with every Session
// open counted; fail fails the test on one.
type countingCreds struct {
	CredentialGateway
	t     *testing.T
	fail  bool
	mu    sync.Mutex
	opens int
}

func (c *countingCreds) Session(ctx context.Context, ref gateway.SessionRef, nodeID string) (*gateway.Session, error) {
	c.mu.Lock()
	c.opens++
	c.mu.Unlock()
	if c.fail {
		c.t.Errorf("a credential was opened for %s %s: the regenerate path opens none", ref.Kind, ref.ItemID)
		return nil, fmt.Errorf("no credential for the regenerate path")
	}
	return c.CredentialGateway.Session(ctx, ref, nodeID)
}

// noSessionHarness fails the test if any session is run.
type noSessionHarness struct{ t *testing.T }

func (h noSessionHarness) Name() string { return "claude-code" }

func (h noSessionHarness) Run(_ context.Context, job harness.Job, _ func(harness.Progress)) (harness.Result, error) {
	h.t.Errorf("a %s session was run in %s: a generated-only conflict needs none", job.Kind, job.Workspace)
	return harness.Result{}, fmt.Errorf("no session expected")
}

// TestGeneratedConflictIsRegenerated: a branch conflicting with main
// only in its declared generated paths is rebased by regenerating them
// (docs/adr/adr-038-a-generated-only-conflict-is-regenerated.md): no
// harness run, no credential opened; origin's head is on main and
// carries gen/out.txt made from main's extra and the branch's schema,
// the declared check exits 0 in a checkout of it, and the rebase is
// finished with that head and the paths regenerated.
func TestGeneratedConflictIsRegenerated(t *testing.T) {
	fx := newRegenFixture(t, regenE2EValidate("./generate --check"), false)
	creds := &countingCreds{t: t, fail: true}
	f := fx.rebase(t, noSessionHarness{t}, func(g CredentialGateway) CredentialGateway { creds.CredentialGateway = g; return creds })

	if creds.opens != 0 {
		t.Fatalf("credentials opened %d time(s)", creds.opens)
	}
	head := fx.originHead(t)
	if head == "" || head == fx.built {
		t.Fatalf("origin's %s is %q, the build's %s: nothing pushed\n%s", fx.branch, head, short(fx.built), fx.logs.String())
	}
	if out, err := exec.Command("git", "--git-dir", fx.origin, "merge-base", "--is-ancestor", fx.mainHead, head).CombinedOutput(); err != nil {
		t.Fatalf("origin's %s (%s) is not on main %s: %s", fx.branch, short(head), short(fx.mainHead), out)
	}
	if got, want := gitRun(t, fx.root, "--git-dir", fx.origin, "show", head+":gen/out.txt"), "fields: a b | extra: y"; got != want {
		t.Fatalf("gen/out.txt at %s = %q, want %q (main's extra, the branch's schema)", short(head), got, want)
	}
	co := filepath.Join(fx.root, "check")
	gitRun(t, fx.root, "clone", "--quiet", fx.origin, co)
	gitRun(t, co, "checkout", "--quiet", "--detach", head)
	fx.sh(t, co, "./generate --check")

	finishes := f.CallsNamed("finishWorkItemRebase")
	if len(finishes) != 1 || finishes[0].Input["headSha"] != head {
		t.Fatalf("finishes: %+v, want one at %s\n%s", finishes, head, fx.logs.String())
	}
	if files := fmt.Sprint(finishes[0].Input["files"]); files != "[gen/out.txt]" {
		t.Fatalf("resolved files %s, want [gen/out.txt]", files)
	}
	if n := len(f.CallsNamed("failWorkItemRebase")); n != 0 {
		t.Fatalf("failWorkItemRebase called %d time(s)\n%s", n, fx.logs.String())
	}
	if it := f.Item(item1); it["rebaseState"] != "rebased" {
		t.Fatalf("item: %v", it)
	}
	if notes := fmt.Sprint(f.CallsNamed("reportWorkItemProgress")); !strings.Contains(notes, "onto main at "+short(head)+" by regenerating gen/out.txt (no session)") {
		t.Fatalf("no regenerated report among the progress notes: %s", notes)
	}
	if _, err := os.Stat(RebaseWorkspaceSpec(WorkspaceSpec{Root: fx.work, GoalSlug: "ship-it", Seq: 1}).Dir()); !os.IsNotExist(err) {
		t.Fatalf("the rebase checkout is still there: %v", err)
	}
}

// promptHarness is a rebase session that records what it was given and
// what origin and its checkout held when it started, and gives up.
type promptHarness struct {
	fx               *regenFixture
	t                *testing.T
	mu               sync.Mutex
	prompts          []string
	originAt, headAt []string
}

func (h *promptHarness) Name() string { return "claude-code" }

func (h *promptHarness) Run(_ context.Context, job harness.Job, _ func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prompts = append(h.prompts, job.Prompt)
	h.originAt = append(h.originAt, h.fx.originHead(h.t))
	h.headAt = append(h.headAt, gitRun(h.t, job.Workspace, "rev-parse", "HEAD"))
	const text = "REBASE FAILED: the test session resolves nothing"
	handInScript(job, nil, text, nil)
	return harness.Result{Success: true, SessionID: "rb-1", Text: text}, nil
}

// TestNonGeneratedConflictFallsThrough: a conflict outside the
// generated paths, or a check that fails after regeneration, starts the
// rebase session exactly once, on the same RebasePrompt as a repository
// with no generated block gets, with origin's branch untouched and the
// checkout at origin/<branch> when it starts.
func TestNonGeneratedConflictFallsThrough(t *testing.T) {
	for _, c := range []struct {
		name       string
		check      []string
		mainSchema bool
		said       string
	}{
		{"conflict in a non-generated path", []string{"./generate --check"}, true, "conflict outside the generated paths: schema.txt"},
		{"check fails after regeneration", []string{"./generate --check", "./build"}, false, "./build failed: exit 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := func(validateYML string) (*promptHarness, *regenFixture, *countingCreds) {
				fx := newRegenFixture(t, validateYML, c.mainSchema)
				h := &promptHarness{fx: fx, t: t}
				creds := &countingCreds{t: t}
				fx.rebase(t, h, func(g CredentialGateway) CredentialGateway { creds.CredentialGateway = g; return creds })
				return h, fx, creds
			}
			// today: a repository that declares no generated block
			today, _, _ := run(regenE2EValidate())
			h, fx, creds := run(regenE2EValidate(c.check...))

			if len(h.prompts) != 1 {
				t.Fatalf("sessions = %d, want 1\n%s", len(h.prompts), fx.logs.String())
			}
			if creds.opens != 1 {
				t.Fatalf("credentials opened %d time(s), want 1", creds.opens)
			}
			if len(today.prompts) != 1 || h.prompts[0] != today.prompts[0] {
				t.Fatalf("the session's prompt differs from today's:\n--- today\n%s\n--- now\n%s", today.prompts, h.prompts[0])
			}
			if h.originAt[0] != fx.built {
				t.Fatalf("origin's %s was %s when the session started, want the build's %s untouched", fx.branch, h.originAt[0], fx.built)
			}
			if h.headAt[0] != fx.built {
				t.Fatalf("the session's checkout was at %s, want origin/%s %s", h.headAt[0], fx.branch, fx.built)
			}
			if !strings.Contains(fx.logs.String(), c.said) || !strings.Contains(fx.logs.String(), "a rebase session instead") {
				t.Fatalf("the fall-through's reason %q is not in the log:\n%s", c.said, fx.logs.String())
			}
		})
	}
}
