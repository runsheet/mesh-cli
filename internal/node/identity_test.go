package node

import (
	"context"
	"os"
	"os/exec"

	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

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

// commitBody is rev's whole message.
func commitBody(t *testing.T, dir, rev string) string {
	t.Helper()
	cmd := exec.Command("git", "log", "-1", "--format=%B", rev)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// assertNoClaudeTrailer fails when rev's message carries a Claude
// Co-Authored-By in any case.
func assertNoClaudeTrailer(t *testing.T, dir, rev string) {
	t.Helper()
	if body := commitBody(t, dir, rev); strings.Contains(strings.ToLower(body), "co-authored-by: claude") {
		t.Fatalf("%s carries a Claude trailer:\n%s", rev, body)
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
