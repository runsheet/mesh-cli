package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// identityWS is gitWS whose Identify is Git's own: the checkout's
// config and hooks are what the session commits through.
type identityWS struct{ *gitWS }

func (w identityWS) Identify(ctx context.Context, dir string, id Identity) error {
	return (&Git{}).Identify(ctx, dir, id)
}

// sessionCommitHarness is a build session that makes real commits as a
// session would: git on the inherited environment plus the job's, no
// identity of its own. msgs are its commits' messages; bare, when set,
// makes the last commit with the job's GIT_* variables left out — a
// session that bypasses them still has the checkout's config.
type sessionCommitHarness struct {
	t    *testing.T
	mu   sync.Mutex
	jobs []harness.Job
	msgs []string
	bare bool
}

func (h *sessionCommitHarness) Name() string { return "claude-code" }

func (h *sessionCommitHarness) Run(_ context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.jobs = append(h.jobs, job)
	h.mu.Unlock()
	for i, msg := range h.msgs {
		writeFiles(h.t, job.Workspace, map[string]string{"api/turn.go": "package api // " + msg + "\n"})
		env := append(os.Environ(), job.Env...)
		if h.bare && i == len(h.msgs)-1 {
			env = withoutGitIdentity(env)
		}
		for _, args := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", msg}} {
			cmd := exec.Command("git", args...)
			cmd.Dir, cmd.Env = job.Workspace, env
			if out, err := cmd.CombinedOutput(); err != nil {
				h.t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
	}
	on(harness.Progress{SessionID: "s-1"})
	return harness.Result{Success: true, Text: "Committed.", SessionID: "s-1", CostUSD: 0.5}, nil
}

func withoutGitIdentity(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_AUTHOR_") && !strings.HasPrefix(kv, "GIT_COMMITTER_") {
			out = append(out, kv)
		}
	}
	return out
}

// identityRunner is a runner on node slug mini with pair as node.yaml's
// pair:, whose session commits msgs on a real checkout.
func identityRunner(t *testing.T, pair []Pairer, bare bool, msgs ...string) (*Runner, *sessionCommitHarness, gateway.WorkItem) {
	t.Helper()
	f := seed(t)
	h := &sessionCommitHarness{t: t, msgs: msgs, bare: bare}
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	r.Cfg.Slug, r.Cfg.Pair = "mini", pair
	r.WS = identityWS{&gitWS{fakeWS: ws, t: t, origin: originRepo(t, nil)}}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d", n)
	}
	return r, h, item
}

// commitLine is `%an|%ae|%cn|%ce|<trailers>` of rev, the trailers each
// on its own line.
func commitLine(t *testing.T, dir, rev string) string {
	t.Helper()
	cmd := exec.Command("git", "log", "-1", "--format=%an|%ae|%cn|%ce|%(trailers:key=Co-Authored-By)", rev)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

const agentMini = "Mesh builder (mini)|mesh-builder+mini@runsheet.dev|Mesh builder (mini)|mesh-builder+mini@runsheet.dev|"

// A build session commits as its agent, author and committer, with the
// paired person's one Co-Authored-By; a message that already carries it
// gains no second (ADR-024).
func TestBuildCommitsAsTheAgentWithThePairer(t *testing.T) {
	chris := Pairer{Name: "Chris Kolenko", Email: "chris@example.com"}
	_, h, _ := identityRunner(t, []Pairer{chris}, false,
		"turn 1",
		"turn 2\n\nCo-Authored-By: Chris Kolenko <chris@example.com>")
	dir := h.jobs[0].Workspace
	want := agentMini + "Co-Authored-By: Chris Kolenko <chris@example.com>"
	for _, rev := range []string{"HEAD~1", "HEAD"} {
		if got := commitLine(t, dir, rev); got != want {
			t.Fatalf("%s: %q, want %q", rev, got, want)
		}
	}
	if v, _ := envOf(h.jobs[0].Env, "GIT_AUTHOR_EMAIL"); v != "mesh-builder+mini@runsheet.dev" {
		t.Fatalf("job GIT_AUTHOR_EMAIL = %q", v)
	}
	if v, _ := envOf(h.jobs[0].Env, "GIT_COMMITTER_NAME"); v != "Mesh builder (mini)" {
		t.Fatalf("job GIT_COMMITTER_NAME = %q", v)
	}
}

// With no pairer the hook adds no trailer.
func TestBuildCommitsWithNoTrailerWithoutAPairer(t *testing.T) {
	_, h, _ := identityRunner(t, nil, false, "turn 1")
	if got := commitLine(t, h.jobs[0].Workspace, "HEAD"); got != agentMini {
		t.Fatalf("HEAD: %q, want %q", got, agentMini)
	}
}

// Two pairers are two trailers, each once.
func TestBuildCommitsWithEveryPairer(t *testing.T) {
	_, h, _ := identityRunner(t, []Pairer{{Name: "Ann", Email: "ann@example.com"}, {Name: "Bo", Email: "bo@example.com"}}, false,
		"turn 1\n\nCo-Authored-By: Bo <bo@example.com>")
	want := agentMini + "Co-Authored-By: Bo <bo@example.com>\nCo-Authored-By: Ann <ann@example.com>"
	if got := commitLine(t, h.jobs[0].Workspace, "HEAD"); got != want {
		t.Fatalf("HEAD: %q, want %q", got, want)
	}
}

// The node's owner has a global git identity; no session commit is
// theirs — neither one on the job's env nor one that drops it and falls
// back to the checkout's config.
func TestNoSessionCommitsAsTheOwner(t *testing.T) {
	home := t.TempDir()
	global := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\tname = Owner Person\n\temail = owner@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	_, h, _ := identityRunner(t, nil, true, "on the env", "past the env")
	dir := h.jobs[0].Workspace
	for _, rev := range []string{"HEAD~1", "HEAD"} {
		got := commitLine(t, dir, rev)
		if strings.Contains(got, "Owner Person") || strings.Contains(got, "owner@example.com") {
			t.Fatalf("%s commits as the owner: %q", rev, got)
		}
		if got != agentMini {
			t.Fatalf("%s: %q, want %q", rev, got, agentMini)
		}
	}
}

func TestAgentIdentity(t *testing.T) {
	name, email := AgentIdentity("rebase", "mini")
	if name != "Mesh rebase (mini)" || email != "mesh-rebase+mini@runsheet.dev" {
		t.Fatalf("got %q <%q>", name, email)
	}
}

func TestPairersDropsTheMalformed(t *testing.T) {
	got := Pairers(gateway.WorkItem{}, nil, []Pairer{
		{Name: "Ann", Email: "ann@example.com"},
		{Name: "Ann again", Email: "ANN@example.com"},
		{Name: "", Email: "x@example.com"},
		{Name: "Evil\nCo-Authored-By: X", Email: "e@example.com"},
	})
	if len(got) != 1 || got[0].Trailer() != "Co-Authored-By: Ann <ann@example.com>" {
		t.Fatalf("got %+v", got)
	}
}
