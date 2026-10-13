//go:build e2e

package node

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// concurrentRounds is how many pairs of sessions each variant starts.
const concurrentRounds = 20

// Two sessions of one repo started together both reach their first
// command (ADR-031, a session never writes the shared clone's config):
// each round two goroutines, released by one barrier, run the whole
// pre-session path for an item of the same repo — Authorize on an item
// token, Prepare, Identify — then `git status` and a commit in their
// worktree. Twenty rounds on a shared clone that already exists, as on a
// busy node, and twenty where neither has cloned yet. Every session
// succeeds, no git output or node log line says `could not lock config
// file`, and on the warm clone .git/config is byte for byte what it was
// after round one.
func TestConcurrentSessionsOnOneRepoE2E(t *testing.T) {
	root, origin := lockTestOrigin(t)
	// the item's repo as GitHub would name it, served over file:// so the
	// origin URL Authorize writes is the one the clone already has
	base := filepath.Join(root, "gh")
	if out, err := exec.Command("git", "clone", "-q", "--bare", origin, filepath.Join(base, "acme", "widgets.git")).CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v: %s", err, out)
	}
	const repo = "acme/widgets"

	var logMu sync.Mutex
	var nodeLog bytes.Buffer
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(&nodeLog, format+"\n", args...)
	}
	SetRepoLockLogf(logf)
	t.Cleanup(func() { SetRepoLockLogf(nil) })

	t.Run("clone exists", func(t *testing.T) {
		repos := filepath.Join(root, "warm", "repos")
		work := filepath.Join(root, "warm", "work")
		g, gitOut := concurrentGit(base, repos, logf)
		clone := SharedClonePath(repos, repo)
		// the busy node: an earlier item made the clone
		session(t, g, WorkspaceSpec{Root: work, GoalSlug: "g", Seq: 1000, Repo: repo, Branch: BranchName("g", 1000), BaseRef: "main"})

		var after1 string
		for r := 1; r <= concurrentRounds; r++ {
			pair(t, g, r, func(seq int) WorkspaceSpec {
				return WorkspaceSpec{Root: work, GoalSlug: "g", Seq: seq, Repo: repo, Branch: BranchName("g", seq), BaseRef: "main"}
			})
			cfg := readConfig(t, clone)
			if r == 1 {
				after1 = cfg
				t.Logf("shared .git/config after round 1:\n%s", cfg)
			} else if cfg != after1 {
				t.Fatalf("round %d changed the shared .git/config:\n--- after round 1\n%s--- after round %d\n%s", r, after1, r, cfg)
			}
		}
		noLockError(t, gitOut(), &logMu, &nodeLog)
	})

	t.Run("neither has cloned", func(t *testing.T) {
		var first string
		for r := 1; r <= concurrentRounds; r++ {
			dir := filepath.Join(root, "cold", fmt.Sprint(r))
			repos, work := filepath.Join(dir, "repos"), filepath.Join(dir, "work")
			g, gitOut := concurrentGit(base, repos, logf)
			pair(t, g, r, func(seq int) WorkspaceSpec {
				return WorkspaceSpec{Root: work, GoalSlug: "g", Seq: seq, Repo: repo, Branch: BranchName("g", seq), BaseRef: "main"}
			})
			clones := 0
			for _, line := range strings.Split(gitOut(), "\n") {
				if strings.HasPrefix(line, "$ git clone ") {
					clones++
				}
			}
			if clones != 1 {
				t.Fatalf("round %d: %d clones of the repo, want 1:\n%s", r, clones, gitOut())
			}
			// every fresh clone's config is the same file: nothing a
			// session wrote, only what the clone and its one set-up hold
			cfg := readConfig(t, SharedClonePath(repos, repo))
			if r == 1 {
				first = cfg
			} else if cfg != first {
				t.Fatalf("round %d's fresh clone has another .git/config:\n--- round 1\n%s--- round %d\n%s", r, first, r, cfg)
			}
			noLockError(t, gitOut(), &logMu, &nodeLog)
		}
	})
}

// concurrentGit is the node's Git on base (as GitHub), its git output
// kept, its log lines going to logf.
func concurrentGit(base, repos string, logf func(string, ...any)) (*Git, func() string) {
	var mu sync.Mutex
	var out bytes.Buffer
	g := &Git{GitHubURL: "file://" + base, ReposRoot: repos, Logf: logf, Log: lockedWriter{&mu, &out}}
	return g, func() string {
		mu.Lock()
		defer mu.Unlock()
		return out.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// pair releases two sessions, items 2r-1 and 2r, by one barrier and fails
// the test unless both start within a second and both succeed.
func pair(t *testing.T, g *Git, r int, spec func(seq int) WorkspaceSpec) {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	began := make([]time.Time, 2)
	outs := make([]string, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			began[i] = time.Now()
			outs[i], errs[i] = runSession(g, spec(2*r-1+i))
		}(i)
	}
	close(start)
	wg.Wait()
	if d := began[0].Sub(began[1]).Abs(); d > time.Second {
		t.Fatalf("round %d: the sessions started %s apart, want within a second", r, d)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("round %d, session %d (seq %d): %v\n%s", r, i+1, 2*r-1+i, err, outs[i])
		}
	}
}

// session runs one session's path and fails the test on an error.
func session(t *testing.T, g *Git, spec WorkspaceSpec) {
	t.Helper()
	if out, err := runSession(g, spec); err != nil {
		t.Fatalf("seq %d: %v\n%s", spec.Seq, err, out)
	}
}

// runSession is the node's pre-session path for one item — Authorize on
// its token, Prepare, Identify — and then the session's first commands
// in its worktree: `git status`, and a commit as the agent Identify
// named. It returns those commands' output.
func runSession(g *Git, spec WorkspaceSpec) (string, error) {
	ctx := WithLockHolder(context.Background(), fmt.Sprintf("g/%d", spec.Seq))
	tok := &gateway.GitHubToken{Token: fmt.Sprintf("ghs_item_%d", spec.Seq), ExpiresAt: time.Now().Add(time.Hour), Repository: spec.Repo}
	if err := g.Authorize(ctx, spec, tok); err != nil {
		return "", fmt.Errorf("authorize: %w", err)
	}
	dir, err := g.Prepare(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("prepare: %w", err)
	}
	if !isWorktree(dir) {
		return "", fmt.Errorf("%s is not a worktree", dir)
	}
	name, email := AgentIdentity("builder", "e2e")
	if err := g.Identify(ctx, dir, Identity{Name: name, Email: email}); err != nil {
		return "", fmt.Errorf("identify: %w", err)
	}
	var all strings.Builder
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// the session's own environment names no author: the worktree's
		// config does
		cmd.Env = append(withoutGitIdentity(os.Environ()), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		fmt.Fprintf(&all, "$ git %s\n%s", strings.Join(args, " "), out)
		if err != nil {
			return string(out), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("status", "--short"); err != nil {
		return all.String(), err
	}
	file := fmt.Sprintf("item-%d", spec.Seq)
	if err := os.WriteFile(filepath.Join(dir, file), []byte(spec.Branch+"\n"), 0o644); err != nil {
		return all.String(), err
	}
	if _, err := git("add", file); err != nil {
		return all.String(), err
	}
	if _, err := git("commit", "-q", "-m", "build "+spec.Branch); err != nil {
		return all.String(), err
	}
	got, err := git("log", "-1", "--format=%ae")
	if err != nil {
		return all.String(), err
	}
	if got != email {
		return all.String(), fmt.Errorf("committed as %q, want the agent %q", got, email)
	}
	if strings.Contains(all.String(), "could not lock config file") {
		return all.String(), fmt.Errorf("a config lock error in the session's output")
	}
	return all.String(), nil
}

func readConfig(t *testing.T, clone string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func noLockError(t *testing.T, gitOut string, mu *sync.Mutex, nodeLog *bytes.Buffer) {
	t.Helper()
	mu.Lock()
	log := nodeLog.String()
	mu.Unlock()
	for what, text := range map[string]string{"git output": gitOut, "node log": log} {
		if strings.Contains(text, "could not lock config file") {
			t.Fatalf("the %s says `could not lock config file`:\n%s", what, text)
		}
	}
}
