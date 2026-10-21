package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// WorkspaceSpec names one item's checkout: ~/.mesh/work/<goal>/<seq>,
// the repo, and the branch it works on from baseRef.
type WorkspaceSpec struct {
	Root     string
	GoalSlug string
	Seq      int
	Repo     string // owner/name
	Branch   string
	BaseRef  string
	// Path, when set, is the checkout's directory outright: a plan's
	// read-only checkout under PlanDir/repos/<name> (PlanRepoSpec),
	// which no goal and seq name.
	Path string
	// Head, when set, is the head the item recorded for Branch: the last
	// place RebuildWorktree looks when neither the shared clone's
	// refs/heads/<branch> nor origin/<branch> is there.
	Head string
	// Repos, when set, is the repos root of the item's connection
	// (ConnectionDirs.Repos, ADR-039): the shared clone a new checkout is
	// a worktree of is <Repos>/<owner>/<name>, whatever Git.ReposRoot
	// says, so one connection's worktree is never added from another's
	// clone.
	Repos string
	// Job, when set, is the item's job directory (ADR-039): the checkout
	// is <Job>/checkout, and Root is the connection's directory, whose
	// .locks hold the repo's locks.
	Job string
}

// Dir is the workspace directory.
func (s WorkspaceSpec) Dir() string {
	if s.Path != "" {
		return s.Path
	}
	if s.Job != "" {
		return filepath.Join(s.Job, "checkout")
	}
	return filepath.Join(s.Root, s.GoalSlug, fmt.Sprint(s.Seq))
}

// EvalWorkspaceSpec is an evaluation's checkout of an item: its own
// clone at <root>/<goal>/<seq>-eval, beside the build's <root>/<goal>/<seq>,
// so the evaluation's Authorize writes its read token on a credential
// helper the build's push never reads (goal
// a-build-pushes-before-it-finishes). Root stays the work root, so the
// per-repo fetch lock is the build's (ADR-023).
func EvalWorkspaceSpec(root, goalSlug string, seq int, repo, branch, baseRef string) WorkspaceSpec {
	return WorkspaceSpec{Root: root, GoalSlug: goalSlug, Seq: seq, Repo: repo, Branch: branch, BaseRef: baseRef,
		Path: filepath.Join(root, goalSlug, fmt.Sprintf("%d-eval", seq))}
}

// PlanRepoSpec is a plan's read-only checkout of repo at dir, for
// Authorize: the goal's token on that checkout alone (adr-022).
func PlanRepoSpec(repo, dir string) WorkspaceSpec {
	return WorkspaceSpec{Repo: repo, Path: dir}
}

// ResearchRepoSpec is PlanRepoSpec for a research session's read-only
// checkout: the direction's token on that checkout alone (adr-022).
func ResearchRepoSpec(repo, dir string) WorkspaceSpec { return PlanRepoSpec(repo, dir) }

// BranchName is the branch an item works on: mesh/<goal-slug>-<seq>.
func BranchName(goalSlug string, seq int) string {
	return fmt.Sprintf("mesh/%s-%d", goalSlug, seq)
}

// Workspacer prepares checkouts and pushes branches. Git is the real one;
// tests substitute a fake.
type Workspacer interface {
	// Authorize makes spec's checkout reach GitHub with the item's token
	// alone — creating the checkout, empty, when there is none yet — so
	// the Prepare and Push after it use nothing else. Called again with
	// a renewed token, it replaces the old one.
	Authorize(ctx context.Context, spec WorkspaceSpec, tok *gateway.GitHubToken) error
	// Prepare clones (or fetches) the repo into spec.Dir() and checks out
	// spec.Branch from spec.BaseRef, returning the directory. The checkout
	// carries origin/<base> with its history, deepened to FetchDepth
	// commits when shallow: the builder checks main first (adr-003). An
	// empty spec.Branch leaves it on origin/<base>, detached, with the
	// base's full history (an already_done item's evaluation).
	Prepare(ctx context.Context, spec WorkspaceSpec) (string, error)
	// Identify makes the checkout in dir commit as id's agent, author
	// and committer, with each pairer's Co-Authored-By (ADR-024): what
	// a build or rebase session is given after Prepare and before it
	// starts, so none commits as the node's owner.
	Identify(ctx context.Context, dir string, id Identity) error
	// Commit stages paths and commits them with msg when anything
	// changed. The node itself commits nothing today (goal documents
	// were retired 2026-10-12); a build's branch is committed by its
	// session.
	Commit(ctx context.Context, dir string, paths []string, msg string) error
	// Push pushes branch to origin and returns its head sha.
	Push(ctx context.Context, dir, branch string) (string, error)
	// Diff renders what the checked-out branch changed since it left
	// origin/base: the stat, then the patch (the evaluator's evidence).
	Diff(ctx context.Context, dir, base string) (string, error)
	// ChangedPaths are the files the checked-out branch changed since it
	// left origin/base (`git diff --name-only <merge-base> HEAD`): what
	// selects a build's validation steps.
	ChangedPaths(ctx context.Context, dir, base string) ([]string, error)
	// Fetch clones repo (owner/name) into dir, shallow but with the
	// default branch's last FetchDepth commits, or refreshes a clone
	// already there — an empty one Authorize made included, fetched on
	// its own helper with no `gh` — a read-only copy for the planner,
	// whose `git log` is how it sees what main already holds (adr-003
	// decision 2, adr-022).
	Fetch(ctx context.Context, repo, dir string) error
	// PushedHead fetches origin and returns the head of origin/branch —
	// what a session pushed, read back — or an error when origin/base is
	// not in its history (the branch was not rebased onto it).
	PushedHead(ctx context.Context, dir, branch, base string) (string, error)
}

// FetchDepth is how much of the default branch's history a read-only
// checkout carries: enough for a planner's `git log` to reach back past
// the goal's start.
const FetchDepth = 300

// Git is the Workspacer over `gh` and `git`: an Authorized checkout on
// its item's token, any other on the operator's own credentials.
type Git struct {
	Log io.Writer
	// Logf, when set, takes the node's log lines about a fetch's own
	// retries — one line per retried fetch, when it finally succeeds or
	// finally gives up (ADR-023's Retry section). Nil is silent.
	Logf func(string, ...any)
	// GitHubURL is GitHub's base URL (GitHubURL when empty).
	GitHubURL string
	// LockRoot is where the per-repo fetch locks live, under
	// <LockRoot>/.locks/ (ADR-023, fetches of one repo are serialised on
	// the node). Empty, Prepare takes its spec's Root and every other
	// call the checkout's grandparent — the work root for an item's
	// <root>/<goal>/<seq>.
	LockRoot string
	// ReposRoot is where the node's one clone per repo lives, as
	// <ReposRoot>/<owner>/<name> (mesh ADR 017): a new item's checkout
	// is a `git worktree add` on it. Empty, it is "repos" beside the
	// spec's Root — ~/.mesh/repos for ~/.mesh/work (ReposRoot()).
	ReposRoot string
	// exec, when set, replaces the real git/gh invocation: tests wrap it
	// to inject a failure and delegate to realRun for the rest. Nil is
	// the real one.
	exec func(ctx context.Context, dir, name string, args ...string) (string, error)
}

func (g *Git) githubURL() string {
	if g.GitHubURL != "" {
		return strings.TrimRight(g.GitHubURL, "/")
	}
	return GitHubURL
}

// Authorize implements Workspacer: origin becomes the repo's https URL
// (a `gh repo clone` may have left an ssh one, on the operator's key)
// and the checkout's own credential helper answers with tok — a
// worktree's in its own config.worktree, never the shared clone's
// config (writeHelper). A new item's checkout that Prepare would make a
// worktree is made one here, the shared clone cloned or fetched on tok
// by the environment, so the helper has its worktree to go in.
func (g *Git) Authorize(ctx context.Context, spec WorkspaceSpec, tok *gateway.GitHubToken) error {
	if tok == nil || !tokenShape.MatchString(tok.Token) {
		return fmt.Errorf("github token: missing or malformed")
	}
	if err := g.originOnToken(ctx, spec, tok); err != nil {
		return err
	}
	return WriteCredentialHelper(ctx, spec.Dir(), g.githubURL(), tok)
}

// Checkout is the session's checkout step on the node (runsheet
// adr-061 §3, the session's ordered steps): spec's workspace prepared —
// made, fetched and checked out as Prepare does — on tok by each git
// command's environment alone (withGitToken), never a config file.
// Origin is left the repo's plain https URL with no credential helper
// of the checkout's own, one an earlier Authorize wrote removed, so
// nothing under the workspace — its .git/config, a worktree's
// config.worktree, the shared clone's config — holds the token the
// job's container would mount. A push or fetch the node runs later
// takes the token per command, by OnItemToken.
func (g *Git) Checkout(ctx context.Context, spec WorkspaceSpec, tok *gateway.GitHubToken) (string, error) {
	if tok == nil || !tokenShape.MatchString(tok.Token) {
		return "", fmt.Errorf("github token: missing or malformed")
	}
	if err := g.originOnToken(ctx, spec, tok); err != nil {
		return "", err
	}
	dir, err := g.Prepare(withGitToken(ctx, g.githubURL(), tok), spec)
	if err != nil {
		return "", err
	}
	if err := clearHelper(ctx, dir, g.githubURL()); err != nil {
		return "", err
	}
	return dir, nil
}

// originOnToken is Authorize's half that writes no credential: spec's
// checkout made when Prepare would make it a worktree (the shared clone
// cloned or fetched on tok by the environment) or an empty clone, and
// origin set to the repo's https URL.
func (g *Git) originOnToken(ctx context.Context, spec WorkspaceSpec, tok *gateway.GitHubToken) error {
	dir := spec.Dir()
	origin := g.githubURL() + "/" + spec.Repo + ".git"
	// a worktree whose .git/worktrees/<n> is gone fails every git command
	// below: rebuilt first, the set-url then runs as on any worktree
	if err := g.RebuildWorktree(ctx, spec); err != nil {
		return err
	}
	if err := g.removeStale(dir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil && g.sharedClone(spec) != "" {
		tctx := withGitToken(ctx, g.githubURL(), tok)
		if _, err := g.addWorktree(tctx, g.lockRoot(spec.Root, dir), RepoLockKey(spec.Repo), g.sharedClone(spec), spec); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		// no clone yet: an empty checkout Prepare fetches into
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if _, err := g.run(ctx, dir, "git", "init", "--quiet"); err != nil {
			return err
		}
		if _, err := g.withStaleLockRetry(dir, func() (string, error) {
			return g.run(ctx, dir, "git", "remote", "add", "origin", origin)
		}); err != nil {
			return err
		}
	} else if isWorktree(dir) {
		if err := g.lockWorktree(ctx, dir, spec); err != nil {
			return err
		}
		// a worktree's origin is its shared clone's: one .git/config
		// every item of the repo reads and writes, so it is changed only
		// under the repo's lock, and only when it differs (sharedOrigin)
		if err := g.sharedOrigin(ctx, g.lockRoot(spec.Root, dir), RepoLockKey(spec.Repo), dir, origin); err != nil {
			return err
		}
	} else if _, err := g.withStaleLockRetry(dir, func() (string, error) {
		return g.run(ctx, dir, "git", "remote", "set-url", "origin", origin)
	}); err != nil {
		return err
	}
	return nil
}

// sharedOrigin makes the shared clone's origin, seen from the worktree
// dir, the repo's https URL. Two items of one repo authorize at once on
// a busy node, and an unlocked `git remote set-url` from each raced for
// the clone's .git/config.lock — "could not lock config file … File
// exists" failed every item of the loser's goal (latchkey,
// 2026-10-12). The URL is read first, which takes no lock and is all
// the steady state needs; a change is made under the repo's lock and
// read again inside it, so one writer sets it and the rest see it set.
func (g *Git) sharedOrigin(ctx context.Context, root, key, dir, origin string) error {
	if cur, err := g.run(ctx, dir, "git", "remote", "get-url", "origin"); err == nil && strings.TrimSpace(cur) == origin {
		return nil
	}
	unlock, err := lockRepo(ctx, root, key)
	if err != nil {
		return err
	}
	defer unlock()
	if cur, err := g.run(ctx, dir, "git", "remote", "get-url", "origin"); err == nil && strings.TrimSpace(cur) == origin {
		return nil
	}
	_, err = g.withStaleLockRetry(dir, func() (string, error) {
		return g.run(ctx, dir, "git", "remote", "set-url", "origin", origin)
	})
	return err
}

func (g *Git) run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	if g.exec != nil {
		return g.exec(ctx, dir, name, args...)
	}
	return g.realRun(ctx, dir, name, args...)
}

// realRun is the real git/gh invocation: run dispatches to it, or to
// g.exec when a test has set one.
func (g *Git) realRun(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), gitTokenEnv(ctx)...)
	out, err := cmd.CombinedOutput()
	if g.Log != nil {
		fmt.Fprintf(g.Log, "$ %s %s\n%s", name, strings.Join(args, " "), out)
	}
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// IsRefLockFailure reports whether err is git refusing a ref update
// because another process was writing the same ref at the same moment —
// "cannot lock ref" or "unable to update local ref" in its text, the two
// lines the grapevine node's log carried before fetches of one repo were
// serialised on the node (ADR-023).
func IsRefLockFailure(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "cannot lock ref") || strings.Contains(s, "unable to update local ref")
}

// SharedClonePath is repo's one clone on the node under reposRoot:
// <reposRoot>/<owner>/<name> for a GitHub repo however it is spelt; any
// other remote — a path, as tests pass — one directory named by the
// lock file's rule (mesh ADR 017).
func SharedClonePath(reposRoot, repo string) string {
	key := RepoLockKey(repo)
	if strings.Count(key, "/") == 1 && !strings.HasPrefix(key, "/") && !strings.Contains(key, ":") {
		return filepath.Join(reposRoot, key)
	}
	return filepath.Join(reposRoot, repoFileName(key))
}

// sharedClone is the clone spec's new checkout is a worktree of, or ""
// when spec takes a clone of its own: a checkout named outright (a plan,
// research or evaluation checkout, Path set), one with no branch, the
// goal doc's <goal>/0 on the base branch itself (Seq 0, Branch ==
// BaseRef: the shared clone has the default branch checked out, and git
// checks a branch out in one worktree only), or one with no repos root
// to keep the clone under.
//
// Those stay full clones, never worktrees, so none of them asks git to
// check out a branch another worktree holds: an evaluation's <seq>-eval
// checks out the build's branch in a clone of its own (ADR-027), the
// goal doc and a no-branch spec (CheckoutBase, detached) the base, and
// a plan or research checkout stays a clone as ADR 017 §6 decides.
func (g *Git) sharedClone(spec WorkspaceSpec) string {
	if spec.Path != "" || spec.Branch == "" || spec.Repo == "" || spec.Seq == 0 || spec.Branch == spec.BaseRef {
		return ""
	}
	reposRoot := spec.Repos
	if reposRoot == "" {
		// a job's job.json names its connection's repos root (ADR-039)
		if c, ok := itemConnectionOf(spec.Dir()); ok {
			reposRoot = c.Repos
		}
	}
	if reposRoot == "" {
		reposRoot = g.ReposRoot
	}
	if reposRoot == "" {
		if spec.Root == "" {
			return ""
		}
		reposRoot = filepath.Join(filepath.Dir(spec.Root), "repos")
	}
	return SharedClonePath(reposRoot, spec.Repo)
}

// Prepare implements Workspacer. A new item's checkout is a worktree of
// the repo's one clone on the node (addWorktree, mesh ADR 017); a
// checkout already there — a worktree, or a full clone from before the
// shared clone — is fetched and checked out in place.
func (g *Git) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	dir := spec.Dir()
	root, key := g.lockRoot(spec.Root, dir), RepoLockKey(spec.Repo)
	if err := g.RebuildWorktree(ctx, spec); err != nil {
		return "", err
	}
	if err := g.removeStale(dir); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if clone := g.sharedClone(spec); clone != "" {
			return g.addWorktree(ctx, root, key, clone, spec)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if _, err := g.fetchLocked(ctx, root, key, filepath.Dir(dir), "gh", "repo", "clone", spec.Repo, dir); err != nil {
			return "", err
		}
	} else if _, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--prune", "origin"); err != nil {
		return "", err
	} else if err := g.lockWorktree(ctx, dir, spec); err != nil {
		// a worktree reused — a rework, an adopted item — is locked as a
		// new one is: its item is live again
		return "", err
	}
	base := spec.BaseRef
	if base == "" {
		out, err := g.run(ctx, dir, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			return "", fmt.Errorf("no base ref and no origin default branch: %w", err)
		}
		base = strings.TrimPrefix(out, "origin/")
	}
	if spec.Branch == "" {
		return dir, g.checkoutBase(ctx, root, key, dir, base)
	}
	if err := g.baseHistory(ctx, root, key, dir, base); err != nil {
		// a branch already pushed — its evaluation's own checkout, a
		// rework — is continued from origin whatever became of its base:
		// a merged dependency's branch GitHub deleted is no failure
		// (ADR-033); a new branch still needs it
		if !g.hasRef(ctx, dir, "refs/remotes/origin/"+spec.Branch) || !g.baseGone(ctx, dir, base) {
			return "", err
		}
		if g.Logf != nil {
			g.Logf("workspace %s: base %s is gone (merged); continuing origin/%s", dir, base, spec.Branch)
		}
	}
	// a rework of the same checkout — a worktree or a full clone — whose
	// local branch is the pushed one or ahead of it keeps its unpushed
	// commits: the push still fast-forwards (mesh ADR 017 §2)
	if _, err := g.run(ctx, dir, "git", "merge-base", "--is-ancestor", "origin/"+spec.Branch, "refs/heads/"+spec.Branch); err == nil {
		if _, err := g.run(ctx, dir, "git", "checkout", "--quiet", spec.Branch); err == nil {
			return dir, nil
		}
	}
	// a branch pushed by an earlier attempt (or just finished, for its
	// evaluation) is continued, not restarted: the evaluator's notes say
	// what to fix, and the push must fast-forward
	// tracking origin's branch (a pair's `git pull --rebase` needs it),
	// written to .git/config — a worktree's shared one — only when the
	// branch does not track it already
	track := "--track"
	if g.tracksOrigin(ctx, dir, spec.Branch) {
		track = "--no-track"
	}
	if _, err := g.run(ctx, dir, "git", "checkout", "--quiet", track, "-B", spec.Branch, "origin/"+spec.Branch); err == nil {
		return dir, nil
	}
	// a local branch from an attempt that was interrupted before its push
	// (the node restarted): continue it, do not reset it onto the base
	if _, err := g.run(ctx, dir, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+spec.Branch); err == nil {
		if _, err := g.run(ctx, dir, "git", "checkout", "--quiet", spec.Branch); err != nil {
			return "", err
		}
		return dir, nil
	}
	// --no-track from the base, here and in addWorktree: tracking is
	// branch.<b>.* in a worktree's shared .git/config, so it is left to
	// Push, which sets it once (the base is never the branch's upstream)
	if _, err := g.run(ctx, dir, "git", "checkout", "--quiet", "--no-track", "-B", spec.Branch, "origin/"+base); err != nil {
		return "", err
	}
	return dir, nil
}

// syncSharedClone makes clone current under the per-repo lock of key:
// `gh repo clone` when there is none yet, else `git fetch --prune
// origin`, the choice made inside the lock so two first items of a repo
// clone it once. A clone that fails is removed before the lock goes, so
// the next prepare clones again rather than fetching into half a repo.
func (g *Git) syncSharedClone(ctx context.Context, root, key, repo, clone string) error {
	unlock, err := lockRepo(ctx, root, key)
	if err != nil {
		return err
	}
	defer unlock()
	// on an item's token (withGitToken) the clone is git's over https,
	// as an authorized checkout's fetch is; else the operator's gh
	tokened := gitTokenEnv(ctx) != nil
	origin := g.githubURL() + "/" + repo + ".git"
	if _, err := os.Stat(filepath.Join(clone, ".git")); err == nil {
		// read first: the steady state — origin already the https URL —
		// writes nothing to the clone's one .git/config (as sharedOrigin)
		if tokened {
			if cur, err := g.run(ctx, clone, "git", "remote", "get-url", "origin"); err != nil || strings.TrimSpace(cur) != origin {
				if _, err := g.withStaleLockRetry(clone, func() (string, error) {
					return g.run(ctx, clone, "git", "remote", "set-url", "origin", origin)
				}); err != nil {
					return err
				}
			}
		}
		_, err := g.fetchRetrying(ctx, key, clone, "git", "fetch", "--prune", "origin")
		return err
	}
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		return err
	}
	cloneCmd := []string{"gh", "repo", "clone", repo, clone}
	if tokened {
		cloneCmd = []string{"git", "clone", "--quiet", origin, clone}
	}
	if _, err := g.fetchRetrying(ctx, key, filepath.Dir(clone), cloneCmd[0], cloneCmd[1:]...); err != nil {
		os.RemoveAll(clone)
		return err
	}
	return nil
}

// addWorktree makes spec's new checkout a worktree of clone (mesh ADR
// 017): the clone fetched under the lock, then — under it again, so no
// other item's prune lands mid-add — `git worktree add` on the item's
// branch, extensions.worktreeConfig enabled for its credential helper,
// continued as Prepare continues a checkout's: origin/<branch> when it was pushed, else a
// local branch an interrupted attempt left in the clone, else a new one
// from origin/<base>. A registration whose directory is gone (a sweep
// interrupted, a directory removed by hand) that names dir, or holds the
// branch and is neither locked nor live, is forgotten first
// (forgetStaleRegistrations), so git does not refuse the path or the
// branch — never a blanket `git worktree prune`, which drops every
// registration whose directory it cannot see. The worktree added is
// locked (lockWorktree), so no prune, inside a pod or out, takes it
// while its item is live.
func (g *Git) addWorktree(ctx context.Context, root, key, clone string, spec WorkspaceSpec) (string, error) {
	dir := spec.Dir()
	if err := g.syncSharedClone(ctx, root, key, spec.Repo, clone); err != nil {
		return "", err
	}
	base := spec.BaseRef
	if base == "" {
		out, err := g.run(ctx, clone, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			return "", fmt.Errorf("no base ref and no origin default branch: %w", err)
		}
		base = strings.TrimPrefix(out, "origin/")
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	// the prune and the add under the repo's lock: another item's prune
	// while this add is half done removes its .git/worktrees/<name>, and
	// git config writes to the clone's one config file collide
	unlock, err := lockRepo(ctx, root, key)
	if err != nil {
		return "", err
	}
	defer unlock()
	// enabled once per clone: read first, so a clone that has it writes
	// nothing to its .git/config
	if out, err := g.run(ctx, clone, "git", "config", "--local", "--type=bool", "--get", "extensions.worktreeConfig"); err != nil || strings.TrimSpace(out) != "true" {
		if _, err := g.withStaleLockRetry(clone, func() (string, error) {
			return g.run(ctx, clone, "git", "config", "--local", "extensions.worktreeConfig", "true")
		}); err != nil {
			return "", err
		}
	}
	// and gc's own prune of worktrees off for good: a `git gc` in a pod,
	// which sees no other item's worktree, must not drop them
	if out, err := g.run(ctx, clone, "git", "config", "--local", "--get", "gc.worktreePruneExpire"); err != nil || strings.TrimSpace(out) != "never" {
		if _, err := g.withStaleLockRetry(clone, func() (string, error) {
			return g.run(ctx, clone, "git", "config", "--local", "gc.worktreePruneExpire", "never")
		}); err != nil {
			return "", err
		}
	}
	if err := forgetStaleRegistrations(clone, dir, spec.Branch); err != nil {
		return "", err
	}
	args := []string{"worktree", "add", "--quiet", "--no-track", "-B", spec.Branch, dir, "origin/" + base}
	if _, err := g.run(ctx, clone, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+spec.Branch); err == nil {
		args = []string{"worktree", "add", "--quiet", "--track", "-B", spec.Branch, dir, "origin/" + spec.Branch}
	} else if _, err := g.run(ctx, clone, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+spec.Branch); err == nil {
		args = []string{"worktree", "add", "--quiet", dir, spec.Branch}
	}
	if _, err := g.run(ctx, clone, "git", args...); err != nil {
		return "", err
	}
	if err := g.lockWorktree(ctx, dir, spec); err != nil {
		return "", err
	}
	return dir, nil
}

// lockWorktree locks the worktree at dir with `git worktree lock
// --reason 'mesh: item <goal>/<seq> live'`, unless it is locked already:
// a locked worktree's registration is never pruned, by the node, by a
// session's `git worktree prune` in a pod that mounts only its own
// worktree beside the shared clone's git dir, or by `git gc`. Only
// RemoveWorkspace, for a finished item, unlocks it. A full clone has
// nothing to lock.
func (g *Git) lockWorktree(ctx context.Context, dir string, spec WorkspaceSpec) error {
	admin := worktreeAdmin(dir)
	if admin == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(admin, "locked")); err == nil {
		return nil
	}
	reason := fmt.Sprintf("mesh: item %s/%d live", spec.GoalSlug, spec.Seq)
	if _, err := g.run(ctx, dir, "git", "worktree", "lock", "--reason", reason, dir); err != nil {
		// locked between the stat and the lock: what was wanted
		if _, serr := os.Stat(filepath.Join(admin, "locked")); serr == nil {
			return nil
		}
		return fmt.Errorf("lock worktree %s: %w", dir, err)
	}
	return nil
}

// forgetStaleRegistrations removes from clone's .git/worktrees the admin
// dir of each registration whose worktree directory is gone and that
// either names dir — the checkout about to be added there, locked or
// not — or has branch checked out and is neither locked nor live on this
// node: what would make `git worktree add` refuse the path or the branch,
// and nothing else.
func forgetStaleRegistrations(clone, dir, branch string) error {
	worktrees := filepath.Join(clone, ".git", "worktrees")
	regs, err := os.ReadDir(worktrees)
	if err != nil {
		return nil
	}
	want := liveKey(dir)
	for _, r := range regs {
		admin := filepath.Join(worktrees, r.Name())
		wt, ok := registeredDir(admin)
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
			continue
		}
		named := liveKey(wt) == want
		if !named {
			if branch == "" || WorkspaceLive(wt) {
				continue
			}
			if _, err := os.Stat(filepath.Join(admin, "locked")); err == nil {
				continue
			}
			head, err := os.ReadFile(filepath.Join(admin, "HEAD"))
			if err != nil || strings.TrimSpace(string(head)) != "ref: refs/heads/"+branch {
				continue
			}
		}
		if err := os.RemoveAll(admin); err != nil {
			return err
		}
	}
	return nil
}

// registeredDir is the worktree directory admin (a .git/worktrees/<n>)
// registers, from its gitdir file's `<dir>/.git`.
func registeredDir(admin string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		return "", false
	}
	return filepath.Dir(filepath.Clean(strings.TrimSpace(string(b)))), true
}

// reposRootFor is the repos root dir's shared clone lives under: its
// job's connection's (~/.mesh/connections/<id>/repos for a checkout in
// ~/.mesh/jobs/<job>, ADR-039), else ReposRoot when set, else "repos"
// beside dir's work root (its grandparent's parent: <root>/../repos for
// <root>/<goal>/<seq>), as sharedClone derives it from a spec's Root.
func (g *Git) reposRootFor(dir string) string {
	if c, ok := itemConnectionOf(dir); ok {
		return c.Repos
	}
	if g.ReposRoot != "" {
		return g.ReposRoot
	}
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(dir))), "repos")
}

// RemoveWorkspace removes a finished item's checkout at dir, for the
// sweep (mesh ADR 017): a worktree — its `.git` a file — unlocked,
// removed by `git worktree remove --force` and its own admin dir gone
// from its shared clone, under the repo's lock; a full clone from before
// the shared clone by os.RemoveAll. A directory already gone, or left
// half-removed by a sweep interrupted after the rm, is removed and its
// own registration forgotten by whichever shared clone still names it,
// so its branch can be checked out again. No other worktree's
// registration is touched — there is no blanket `git worktree prune` —
// and a dir held live on this node (HoldWorkspace: its item claimed,
// running, evaluating or rebasing here) is refused outright. Nothing
// under the repos root is ever removed but through git's worktree
// metadata: a shared clone itself is refused.
func (g *Git) RemoveWorkspace(ctx context.Context, dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	reposRoot, _ := filepath.Abs(g.reposRootFor(dir))
	if rel, err := filepath.Rel(reposRoot, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("remove workspace %s: under the repos root %s; a shared clone is never removed", dir, reposRoot)
	}
	if WorkspaceLive(dir) {
		return fmt.Errorf("remove workspace %s: its item is live on this node; left alone", dir)
	}
	root := g.lockRoot("", dir)
	fi, err := os.Stat(filepath.Join(dir, ".git"))
	switch {
	case err == nil && fi.IsDir():
		return os.RemoveAll(dir)
	case err == nil:
		if clone := worktreeClone(dir); clone != "" {
			return g.removeWorktree(ctx, root, clone, dir)
		}
		// a worktree whose clone is gone: nothing left to prune
		return os.RemoveAll(dir)
	case !os.IsNotExist(err):
		return err
	}
	// gone, or half-removed: whatever is left goes, then each shared
	// clone still naming dir forgets that one registration
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	for clone, admins := range registrationsNaming(reposRoot, dir) {
		if err := g.forgetRegistrations(ctx, root, clone, admins); err != nil {
			return err
		}
	}
	return nil
}

// removeWorktree unlocks the worktree at dir, runs `git worktree remove
// --force dir` and removes that worktree's own admin dir, if git left
// it, in clone under its repo's lock. Only dir's registration is
// touched: no prune, so a live item's worktree a pod cannot see keeps
// its .git/worktrees/<n>.
func (g *Git) removeWorktree(ctx context.Context, root, clone, dir string) error {
	admin := worktreeAdmin(dir)
	unlock, err := lockRepo(ctx, root, g.repoKeyOf(ctx, clone))
	if err != nil {
		return err
	}
	defer unlock()
	if admin != "" {
		if _, err := os.Stat(filepath.Join(admin, "locked")); err == nil {
			_, _ = g.run(ctx, clone, "git", "worktree", "unlock", dir)
		}
	}
	if _, err := g.run(ctx, clone, "git", "worktree", "remove", "--force", dir); err != nil {
		// git refused (a damaged worktree): the files go, and its own
		// registration below
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return err
		}
	}
	if admin != "" {
		return os.RemoveAll(admin)
	}
	return nil
}

// pruneWorktrees runs `git worktree prune` in clone under its repo lock,
// removing the admin entries of worktrees the quartermaster found gone
// (pruneGoneWorktrees).
func (g *Git) pruneWorktrees(ctx context.Context, root, clone string) error {
	unlock, err := lockRepo(ctx, root, g.repoKeyOf(ctx, clone))
	if err != nil {
		return err
	}
	defer unlock()
	_, err = g.run(ctx, clone, "git", "worktree", "prune")
	return err
}

// forgetRegistrations removes admins — clone's .git/worktrees/<n> of a
// worktree already gone — under clone's repo lock.
func (g *Git) forgetRegistrations(ctx context.Context, root, clone string, admins []string) error {
	unlock, err := lockRepo(ctx, root, g.repoKeyOf(ctx, clone))
	if err != nil {
		return err
	}
	defer unlock()
	for _, a := range admins {
		if err := os.RemoveAll(a); err != nil {
			return err
		}
	}
	return nil
}

// worktreeAdmin is the admin dir — <clone>/.git/worktrees/<n> — the
// worktree at dir's `.git` file names, or "" when dir is no worktree or
// that admin dir is not under a .git/worktrees.
func worktreeAdmin(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	if filepath.Base(filepath.Dir(gitdir)) != "worktrees" {
		return ""
	}
	return gitdir
}

// worktreeClone is the clone the worktree at dir belongs to, from its
// `.git` file's `gitdir: <clone>/.git/worktrees/<name>`, or "" when the
// file says otherwise or that clone is gone.
func worktreeClone(dir string) string {
	admin := worktreeAdmin(dir)
	if admin == "" {
		return ""
	}
	common := filepath.Dir(filepath.Dir(admin))
	if fi, err := os.Stat(common); err != nil || !fi.IsDir() {
		return ""
	}
	return filepath.Dir(common)
}

// hasGitFile reports whether dir's `.git` is a file — a worktree's,
// live or stale — rather than a directory or nothing.
func hasGitFile(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && !fi.IsDir()
}

// gitFileTarget is the gitdir dir's `.git` file names, made absolute, or
// "" when `.git` is no file or names none.
func gitFileTarget(dir string) string {
	if !hasGitFile(dir) {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	return filepath.Clean(gitdir)
}

// staleWorktree reports a checkout at dir whose `.git` is a file naming
// a gitdir that no longer exists, or a .git/worktrees/<n> whose clone is
// gone: no checkout is here, though `.git` exists, and every git command
// in it fails `fatal: not a git repository`. It returns the gitdir the
// file names.
func staleWorktree(dir string) (gitdir string, ok bool) {
	gitdir = gitFileTarget(dir)
	if gitdir == "" {
		return "", false
	}
	if _, err := os.Stat(gitdir); os.IsNotExist(err) {
		return gitdir, true
	}
	if worktreeAdmin(dir) != "" && worktreeClone(dir) == "" {
		return gitdir, true
	}
	return "", false
}

// removeStale removes the checkout at dir when it is a stale worktree
// (staleWorktree), so the caller finds no `.git` and makes it again — a
// worktree by addWorktree, which forgets any registration still naming
// dir, else a clone of its own. A stale worktree whose clone is still
// there and whose admin dir alone is gone is RebuildWorktree's, which
// keeps its files; what is left here has no clone, or no admin dir
// shape, to rebuild from. Logged `workspace <dir>: worktree <gitdir> is
// gone; made again`.
func (g *Git) removeStale(dir string) error {
	gitdir, ok := staleWorktree(dir)
	if !ok {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("workspace %s: worktree %s is gone; remove: %w", dir, gitdir, err)
	}
	if g.Logf != nil {
		g.Logf("workspace %s: worktree %s is gone; made again", dir, gitdir)
	}
	return nil
}

// worktreeAdminGone reports a worktree at dir whose `.git` file names an
// admin dir — <clone>/.git/worktrees/<n> — that no longer exists while
// its shared clone does: every git command there fails `fatal: not a
// git repository: …/.git/worktrees/<n>`. It returns the clone and the
// admin dir the file names; ok is false for a full clone, a healthy
// worktree, or one whose clone is gone too.
func worktreeAdminGone(dir string) (clone, admin string, ok bool) {
	if !hasGitFile(dir) {
		return "", "", false
	}
	admin = worktreeAdmin(dir)
	if admin == "" {
		return "", "", false
	}
	if _, err := os.Stat(admin); err == nil || !os.IsNotExist(err) {
		return "", "", false
	}
	clone = worktreeClone(dir)
	if clone == "" {
		return "", "", false
	}
	return clone, admin, true
}

// adminGoneRE is git's words for a worktree whose admin dir is gone.
var adminGoneRE = regexp.MustCompile(`not a git repository: .*/\.git/worktrees/`)

// IsWorktreeAdminGone reports whether err is git refusing a worktree
// whose .git/worktrees/<n> is gone: what one RebuildWorktree and one
// retry of the step answer (PrepareWithRetry, the validation run).
func IsWorktreeAdminGone(err error) bool {
	return err != nil && adminGoneRE.MatchString(err.Error())
}

// worktreeRebuilder is the Workspacer that can rebuild a worktree whose
// admin dir is gone (*Git); a fake without it gets no retry.
type worktreeRebuilder interface {
	RebuildWorktree(ctx context.Context, spec WorkspaceSpec) error
}

// RebuildWorktree recreates spec's worktree when its `.git` file names a
// .git/worktrees/<n> that is gone (worktreeAdminGone), and does nothing
// otherwise. Under the repo's lock it adds, with --no-checkout, a
// worktree for spec.Branch at its recorded head — the shared clone's
// refs/heads/<branch>, which outlives the admin dir, else
// origin/<branch>, else spec.Head — at a temporary path whose base name
// is the lost admin dir's, so git registers it under the same
// .git/worktrees/<n>; moves its `.git` file over the stale one, runs
// `git worktree repair` and a mixed `git reset`, so the working tree's
// files — committed or not — are left as they were and only the index
// is rebuilt; and locks it as addWorktree does. A checkout that is no
// worktree of the shared clone (an evaluation's <seq>-eval, a plan's) is
// removed whole, for the Prepare after it to make again. Logged
// `workspace <dir>: worktree <path> is gone; made again for <branch> at
// <sha>`.
func (g *Git) RebuildWorktree(ctx context.Context, spec WorkspaceSpec) error {
	dir := spec.Dir()
	clone, admin, ok := worktreeAdminGone(dir)
	if !ok {
		return nil
	}
	if g.sharedClone(spec) == "" {
		// no worktree spec: removed, and cloned again as a new checkout
		return g.removeStale(dir)
	}
	key := RepoLockKey(spec.Repo)
	if spec.Repo == "" {
		key = g.repoKeyOf(ctx, clone)
	}
	unlock, err := lockRepo(ctx, g.lockRoot(spec.Root, dir), key)
	if err != nil {
		return err
	}
	defer unlock()
	if _, _, ok := worktreeAdminGone(dir); !ok {
		// rebuilt by another caller while this one waited on the lock
		return nil
	}
	branch := spec.Branch
	if branch == "" {
		return fmt.Errorf("workspace %s: worktree admin %s gone and no branch to rebuild it for", dir, admin)
	}
	tmpParent, err := os.MkdirTemp(filepath.Dir(dir), ".rebuild-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpParent)
	// the lost admin dir's own name, so the .git file's gitdir is the one
	// it was, and a pod mounting that path finds it again
	tmp := filepath.Join(tmpParent, filepath.Base(admin))
	var sha string
	var args []string
	if out, err := g.run(ctx, clone, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		sha = strings.TrimSpace(out)
		args = []string{"worktree", "add", "--quiet", "--no-checkout", tmp, branch}
	} else if out, err := g.run(ctx, clone, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		sha = strings.TrimSpace(out)
		args = []string{"worktree", "add", "--quiet", "--no-checkout", "--track", "-B", branch, tmp, "origin/" + branch}
	} else if spec.Head != "" {
		sha = spec.Head
		args = []string{"worktree", "add", "--quiet", "--no-checkout", "-B", branch, tmp, sha}
	} else {
		return fmt.Errorf("workspace %s: worktree admin %s gone and branch %s has no head to rebuild at", dir, admin, branch)
	}
	// a registration naming dir, or holding the branch with its directory
	// gone, would refuse the add; dir's own is gone already
	if err := forgetStaleRegistrations(clone, dir, branch); err != nil {
		return err
	}
	if _, err := g.run(ctx, clone, "git", args...); err != nil {
		return fmt.Errorf("workspace %s: rebuild worktree: %w", dir, err)
	}
	if err := os.Rename(filepath.Join(tmp, ".git"), filepath.Join(dir, ".git")); err != nil {
		return fmt.Errorf("workspace %s: rebuild worktree: %w", dir, err)
	}
	if _, err := g.run(ctx, clone, "git", "worktree", "repair", dir); err != nil {
		return fmt.Errorf("workspace %s: rebuild worktree: %w", dir, err)
	}
	if _, err := g.run(ctx, dir, "git", "reset", "--quiet"); err != nil {
		return fmt.Errorf("workspace %s: rebuild worktree: %w", dir, err)
	}
	if err := g.lockWorktree(ctx, dir, spec); err != nil {
		return err
	}
	if g.Logf != nil {
		g.Logf("workspace %s: worktree %s is gone; made again for %s at %s", dir, admin, branch, sha)
	}
	return nil
}

// rebuildAndRetry runs step and, when it fails on a worktree whose admin
// dir is gone and ws can rebuild one, rebuilds spec's worktree once and
// runs step once more, retry true: the rebuilt worktree's own
// config.worktree — its credential helper, its identity — went with the
// admin dir, so a step on the item's token re-authorizes first.
func rebuildAndRetry[T any](ctx context.Context, ws Workspacer, spec WorkspaceSpec, step func(retry bool) (T, error)) (T, error) {
	v, err := step(false)
	if !IsWorktreeAdminGone(err) || ctx.Err() != nil {
		return v, err
	}
	rb, ok := ws.(worktreeRebuilder)
	if !ok || (spec.Path == "" && spec.GoalSlug == "") {
		return v, err
	}
	if rerr := rb.RebuildWorktree(ctx, spec); rerr != nil {
		return v, fmt.Errorf("%w; rebuild: %v", err, rerr)
	}
	return step(true)
}

// registrationsNaming is, per shared clone under reposRoot —
// <owner>/<name>, or one directory for a non-GitHub remote — the admin
// dirs of its worktrees registered at dir.
func registrationsNaming(reposRoot, dir string) map[string][]string {
	var candidates []string
	tops, _ := os.ReadDir(reposRoot)
	for _, top := range tops {
		if !top.IsDir() {
			continue
		}
		p := filepath.Join(reposRoot, top.Name())
		candidates = append(candidates, p)
		subs, _ := os.ReadDir(p)
		for _, s := range subs {
			if s.IsDir() && s.Name() != ".git" {
				candidates = append(candidates, filepath.Join(p, s.Name()))
			}
		}
	}
	names := map[string]bool{dir: true}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		names[filepath.Join(parent, filepath.Base(dir))] = true
	}
	regsOf := map[string][]string{}
	for _, c := range candidates {
		regs, err := os.ReadDir(filepath.Join(c, ".git", "worktrees"))
		if err != nil {
			continue
		}
		for _, r := range regs {
			admin := filepath.Join(c, ".git", "worktrees", r.Name())
			if wt, ok := registeredDir(admin); ok && names[wt] {
				regsOf[c] = append(regsOf[c], admin)
			}
		}
	}
	return regsOf
}

// CheckoutBase leaves dir on origin/<base>, detached, with the base's
// full history: what an already_done item's evaluator checks its
// evidence on (mesh ADR 003 decision 1). A single-branch clone is
// fetched the base explicitly and a shallow one unshallowed, so every
// cited SHA can be looked up.
func (g *Git) CheckoutBase(ctx context.Context, dir, base string) error {
	return g.checkoutBase(ctx, g.lockRoot("", dir), g.repoKeyOf(ctx, dir), dir, base)
}

func (g *Git) checkoutBase(ctx context.Context, root, key, dir, base string) error {
	refspec := "+refs/heads/" + base + ":refs/remotes/origin/" + base
	args := []string{"fetch", "--quiet", "origin", refspec}
	if out, err := g.run(ctx, dir, "git", "rev-parse", "--is-shallow-repository"); err == nil && out == "true" {
		args = []string{"fetch", "--quiet", "--unshallow", "origin", refspec}
	}
	if _, err := g.fetchLocked(ctx, root, key, dir, "git", args...); err != nil {
		return err
	}
	_, err := g.run(ctx, dir, "git", "checkout", "--quiet", "--detach", "origin/"+base)
	return err
}

// baseHistory gives the checkout origin/<base> with its history, the
// builder's evidence of what main already holds (adr-003 decision 2): the
// base branch fetched by name (a single-branch clone lacks it) and, in a
// shallow checkout, its last FetchDepth commits when it has fewer (never
// shortened: a deeper history an evaluation fetched stays).
func (g *Git) baseHistory(ctx context.Context, root, key, dir, base string) error {
	refspec := "+refs/heads/" + base + ":refs/remotes/origin/" + base
	if _, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--quiet", "origin", refspec); err != nil {
		return err
	}
	if out, err := g.run(ctx, dir, "git", "rev-parse", "--is-shallow-repository"); err != nil || out != "true" {
		return nil
	}
	if out, err := g.run(ctx, dir, "git", "rev-list", "--count", "origin/"+base); err == nil {
		if n, err := strconv.Atoi(out); err == nil && n >= FetchDepth {
			return nil
		}
	}
	_, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--quiet", "--depth="+strconv.Itoa(FetchDepth), "origin", refspec)
	return err
}

// Diff implements Workspacer: the three-dot diff from where the branch
// forked origin/base, stat first, capped so a prompt stays a prompt.
func (g *Git) Diff(ctx context.Context, dir, base string) (string, error) {
	if err := g.EnsureMergeBase(ctx, dir, base); err != nil {
		return "", err
	}
	ref := "origin/" + base + "...HEAD"
	stat, err := g.run(ctx, dir, "git", "diff", "--stat", ref)
	if err != nil {
		return "", err
	}
	patch, err := g.run(ctx, dir, "git", "diff", ref)
	if err != nil {
		return "", err
	}
	if len(patch) > MaxDiffBytes {
		patch = patch[:MaxDiffBytes] + "\n… (diff truncated; read the branch for the rest)"
	}
	return stat + "\n\n" + patch, nil
}

// ChangedPaths implements Workspacer.
func (g *Git) ChangedPaths(ctx context.Context, dir, base string) ([]string, error) {
	if err := g.EnsureMergeBase(ctx, dir, base); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, dir, "git", "diff", "--name-only", "origin/"+base+"...HEAD")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}

// NoMergeBaseError says the checked-out branch shares no history with
// origin/<Base>: it is the item's problem (rebuilt from scratch or from an
// unrelated root), not the node's.
type NoMergeBaseError struct {
	Base, Branch string
}

func (e *NoMergeBaseError) Error() string {
	return fmt.Sprintf("branch %s shares no history with origin/%s: it was rebuilt from scratch or from an unrelated root, so there is nothing to diff it against", e.Branch, e.Base)
}

// BaseGoneError says origin no longer has the base branch: a dependency's
// branch GitHub deleted when it merged. Only `git ls-remote --exit-code`
// answering 2 (asked, and no such ref) makes one, so a fetch refused for
// its credential or the network stays that error (ADR-030).
type BaseGoneError struct {
	Base string
	Err  error // the fetch's own error
}

func (e *BaseGoneError) Error() string {
	return fmt.Sprintf("base %s is gone from origin: %v", e.Base, e.Err)
}

func (e *BaseGoneError) Unwrap() error { return e.Err }

// baseGone asks origin whether it still has refs/heads/<base>: true only
// when it answered and has none (ls-remote's exit 2).
func (g *Git) baseGone(ctx context.Context, dir, base string) bool {
	_, err := g.run(ctx, dir, "git", "ls-remote", "--exit-code", "origin", "refs/heads/"+base)
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 2
}

// hasRef says dir's repository has ref.
func (g *Git) hasRef(ctx context.Context, dir, ref string) bool {
	_, err := g.run(ctx, dir, "git", "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// deepenRounds bounds EnsureMergeBase: this many `--deepen` fetches (the
// step doubling from FetchDepth), then one `--unshallow`.
const deepenRounds = 3

// EnsureMergeBase makes origin/<base> and HEAD share a merge base in dir,
// fetching the base branch explicitly (a single-branch clone lacks it) and
// deepening a shallow checkout in a fixed number of steps. A branch that
// still shares none yields a *NoMergeBaseError; a base origin no longer
// has, a *BaseGoneError.
func (g *Git) EnsureMergeBase(ctx context.Context, dir, base string) error {
	root, key := g.lockRoot("", dir), g.repoKeyOf(ctx, dir)
	remote := "refs/remotes/origin/" + base
	if _, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--quiet", "origin", "+refs/heads/"+base+":"+remote); err != nil {
		if g.baseGone(ctx, dir, base) {
			return &BaseGoneError{Base: base, Err: err}
		}
		return err
	}
	has := func() bool {
		_, err := g.run(ctx, dir, "git", "merge-base", remote, "HEAD")
		return err == nil
	}
	if has() {
		return nil
	}
	shallow := func() bool {
		out, err := g.run(ctx, dir, "git", "rev-parse", "--is-shallow-repository")
		return err == nil && out == "true"
	}
	step := FetchDepth
	for i := 0; i < deepenRounds && shallow(); i++ {
		if _, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--quiet", "--deepen="+strconv.Itoa(step), "origin"); err != nil {
			return err
		}
		if has() {
			return nil
		}
		step *= 2
	}
	// a repository the deepening completed is no longer shallow: nothing to unshallow
	if shallow() {
		if _, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--quiet", "--unshallow", "origin"); err != nil {
			return err
		}
		if has() {
			return nil
		}
	}
	branch, _ := g.run(ctx, dir, "git", "rev-parse", "--abbrev-ref", "HEAD")
	return &NoMergeBaseError{Base: base, Branch: branch}
}

// Fetch implements Workspacer.
func (g *Git) Fetch(ctx context.Context, repo, dir string) error {
	root, key := g.lockRoot("", dir), RepoLockKey(repo)
	if err := g.removeStale(dir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		_, err := g.fetchLocked(ctx, root, key, filepath.Dir(dir), "gh", "repo", "clone", repo, dir, "--", "--depth", strconv.Itoa(FetchDepth))
		return err
	}
	// origin's HEAD by name: a checkout Authorize made empty has no
	// branch tracking the default one, so a bare `fetch origin` would
	// leave FETCH_HEAD on whichever branch sorts first
	if _, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--depth", strconv.Itoa(FetchDepth), "origin", "HEAD"); err != nil {
		return err
	}
	_, err := g.run(ctx, dir, "git", "reset", "--hard", "FETCH_HEAD")
	return err
}

// MaxDiffBytes caps the patch an evaluator is handed inline.
const MaxDiffBytes = 200 * 1024

// Commit implements Workspacer.
func (g *Git) Commit(ctx context.Context, dir string, paths []string, msg string) error {
	if _, err := g.run(ctx, dir, "git", append([]string{"add", "--"}, paths...)...); err != nil {
		return err
	}
	if _, err := g.run(ctx, dir, "git", "diff", "--cached", "--quiet"); err == nil {
		return nil // nothing staged
	}
	_, err := g.run(ctx, dir, "git", "commit", "--quiet", "-m", msg)
	return err
}

// HasCommitSince is a build's hand-back test (ADR 012): whether dir's
// branch has a commit past origin/base, by `git rev-list --count
// origin/<base>..HEAD`. A git that cannot answer counts as a commit, so
// a session is never interrupted on a guess.
func HasCommitSince(dir, base string) func() bool {
	return func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-list", "--count", "origin/"+base+"..HEAD").Output()
		if err != nil {
			return true
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		return err != nil || n > 0
	}
}

// Push implements Workspacer.
func (g *Git) Push(ctx context.Context, dir, branch string) (string, error) {
	// --force-with-lease: a reworked branch was rebased; a fresh one is
	// unaffected, and a remote that moved under us still refuses
	// -u only until the branch tracks origin's: `push -u` rewrites
	// branch.<b>.* in .git/config — a worktree's shared clone's — on
	// every push, so a push of a branch already tracking writes none
	args := []string{"push", "--force-with-lease", "origin", branch}
	if !g.tracksOrigin(ctx, dir, branch) {
		args = []string{"push", "-u", "--force-with-lease", "origin", branch}
	}
	if out, err := g.run(ctx, dir, "git", args...); err != nil {
		if PushRefusal(out) {
			return "", &PushRefusedError{Branch: branch, Output: strings.TrimSpace(out), Err: err}
		}
		return "", err
	}
	return g.run(ctx, dir, "git", "rev-parse", "HEAD")
}

// tracksOrigin reports whether branch's upstream in dir is already
// origin's branch of the same name.
func (g *Git) tracksOrigin(ctx context.Context, dir, branch string) bool {
	remote, err := g.run(ctx, dir, "git", "config", "--get", "branch."+branch+".remote")
	if err != nil || strings.TrimSpace(remote) != "origin" {
		return false
	}
	merge, err := g.run(ctx, dir, "git", "config", "--get", "branch."+branch+".merge")
	return err == nil && strings.TrimSpace(merge) == "refs/heads/"+branch
}

// PushRefusedError is git refusing a push because the branch moved on
// origin — the lease is stale or the update was rejected — as against a
// transport failure: the remote answered, and the answer was no. A pair
// working the holder's branch (ADR-023, Pairing) meets it when the
// holder pushed first; it is fixed by a rebase, never by a retry.
type PushRefusedError struct {
	Branch string
	// Output is git's own words.
	Output string
	Err    error
}

func (e *PushRefusedError) Error() string {
	return fmt.Sprintf("git push --force-with-lease origin %s refused: the branch moved on origin: %s", e.Branch, e.Output)
}

func (e *PushRefusedError) Unwrap() error { return e.Err }

// PushRefusal reports whether git's push output is a refusal by the
// remote — `stale info` (the lease) or `rejected` — rather than a
// failure to reach it.
func PushRefusal(out string) bool {
	return strings.Contains(out, "stale info") || strings.Contains(out, "rejected")
}

// PushedHead implements Workspacer.
func (g *Git) PushedHead(ctx context.Context, dir, branch, base string) (string, error) {
	if _, err := g.fetchLocked(ctx, g.lockRoot("", dir), g.repoKeyOf(ctx, dir), dir, "git", "fetch", "--prune", "origin"); err != nil {
		return "", err
	}
	head, err := g.run(ctx, dir, "git", "rev-parse", "refs/remotes/origin/"+branch)
	if err != nil {
		return "", fmt.Errorf("origin has no %s: %w", branch, err)
	}
	if _, err := g.run(ctx, dir, "git", "merge-base", "--is-ancestor", "refs/remotes/origin/"+base, head); err != nil {
		return "", fmt.Errorf("origin/%s at %s does not contain origin/%s", branch, short(head), base)
	}
	return head, nil
}

// FetchHead fetches origin's branch into dir under the per-repo fetch
// lock (ADR-023) and makes sure head is there: the head a refusal named
// as newer than the one pushed (goal the-node-attests-the-head-it-pushed).
// A head the branch no longer leads to is asked for by its sha; one origin
// does not have is an error.
func (g *Git) FetchHead(ctx context.Context, dir, branch, head string) error {
	root, key := g.lockRoot("", dir), g.repoKeyOf(ctx, dir)
	refspec := "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
	if _, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--quiet", "origin", refspec); err != nil {
		return err
	}
	if _, err := g.run(ctx, dir, "git", "cat-file", "-e", head+"^{commit}"); err == nil {
		return nil
	}
	if _, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--quiet", "origin", head); err != nil {
		return fmt.Errorf("origin's %s has no %s: %w", branch, short(head), err)
	}
	if _, err := g.run(ctx, dir, "git", "cat-file", "-e", head+"^{commit}"); err != nil {
		return fmt.Errorf("origin's %s has no %s", branch, short(head))
	}
	return nil
}

// ContextDir is where the goal's context documents land in a workspace.
const ContextDir = ".mesh/context"

// Context is a session's context (ADR-015): the instructions of its own
// repos on disk by file name, the project's whole listing kept in
// memory for INDEX.md's count line (never rendered as rows), the
// project (org/slug), its PRD slug when it lists one, and the session's
// own goal.
type Context struct {
	Docs  map[string]string
	Index []gateway.Document
	// OnDisk is file name → the document it holds, for the index. It is
	// keyed by file name and not by slug: several repos' AGENTS.md share
	// the slug `agents`, and each is its own repo's file.
	OnDisk  map[string]gateway.Document
	Project string
	PRDSlug string
	// Dropped is the own repos' instructions fetched but left off disk
	// by the cap: named in INDEX.md as binding, to fetch first.
	Dropped []gateway.Document
	// IndexBytes is INDEX.md's length as FetchContext counted it against
	// the cap, the goal section included.
	IndexBytes int
	// BytesMax is the cap the context holds to, INDEX.md included; zero
	// is DefaultContextBytesMax.
	BytesMax int
	// Live is the session's own goal with its items as the platform
	// holds them (adr-003 decision 3), rendered into INDEX.md; nil for a
	// session with no goal (research, channel mode).
	Live *LiveGoal
}

// LiveGoal is the session's own goal with its items and, by repo id,
// the owner/name each item works in.
type LiveGoal struct {
	Goal  gateway.Goal
	Items []gateway.WorkItem
	Repos map[string]string
	// Unread is why the items could not be read; the goal's statement
	// and acceptance are still rendered.
	Unread string
}

// fetchOwnGoal reads g's items and the repos they work in. A failed
// items read keeps the goal with Unread set; a failed repos read leaves
// the repo cells empty.
func fetchOwnGoal(ctx context.Context, gw Gateway, g gateway.Goal) *LiveGoal {
	lg := &LiveGoal{Goal: g, Repos: map[string]string{}}
	items, err := gw.GoalWorkItems(ctx, g.ID)
	if err != nil {
		lg.Unread = err.Error()
		return lg
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Seq < items[j].Seq })
	lg.Items = items
	var ids []string
	seen := map[string]bool{}
	for _, it := range items {
		if it.RepoID != "" && !seen[it.RepoID] {
			seen[it.RepoID] = true
			ids = append(ids, it.RepoID)
		}
	}
	if repos, err := gw.ProjectRepos(ctx, ids); err == nil {
		for _, r := range repos {
			lg.Repos[r.ID] = r.FullName()
		}
	}
	return lg
}

// ContextOptions is how FetchContext fetches: Budget is the size budget
// (zero is the defaults; bytes_max holds everything written,
// doc_bytes_max each instructions document, ADR-015), Repos
// (owner/name) the repos of the session's own work — whose instructions
// alone go on disk — Checkouts each of those repos' checkout on this
// machine (owner/name → dir), read for an instructions document whose
// slug another repo's shares, and Goal the session's own goal, nil for
// none. Logf, when set, takes the warnings: the goal's items unread,
// and INDEX.md alone past the cap.
type ContextOptions struct {
	Budget    ContextConfig
	Repos     []string
	Checkouts map[string]string
	Goal      *gateway.Goal
	Logf      func(string, ...any)
}

// FetchContext lists the project's documents and puts on disk only what
// a session cannot do without (ADR-015): the instructions documents
// whose repo is one of opts.Repos (case-insensitive) — no ADR, PRD or
// TRD body is fetched — and, in INDEX.md, the session's own goal with
// its items. Everything else is fetched on demand through the MCP
// tools. INDEX.md is inside bytes_max: its length is counted first, and
// an instructions document past doc_bytes_max, or that would take the
// total past the cap, is left off disk whole, on cx.Dropped, and named
// in INDEX.md as binding, to fetch first. An INDEX.md alone past the
// cap places nothing, with one warning.
//
// Instructions are read by repo and path, not by slug alone: every
// repo's AGENTS.md is slug `agents`, and the gateway's by-slug read
// (/documents/<slug>) answers one of them, whichever repo it picks. A
// slug only one instructions document carries is read from the gateway;
// a shared one is read from the session's own checkout of its repo
// (opts.Checkouts, the file at the document's path — the checkout is
// the repo's default branch, or a branch from it), and with no checkout
// it is left to the index, named by repo and path.
func FetchContext(ctx context.Context, gw Gateway, orgLogin, projectSlug string, opts ContextOptions) (*Context, error) {
	budget := opts.Budget.Effective()
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	docs, err := gw.ContextDocuments(ctx, orgLogin, projectSlug)
	if err != nil {
		return nil, err
	}
	out := &Context{Docs: map[string]string{}, Index: docs, OnDisk: map[string]gateway.Document{}, Project: orgLogin + "/" + projectSlug, BytesMax: budget.BytesMax}
	if opts.Goal != nil {
		out.Live = fetchOwnGoal(ctx, gw, *opts.Goal)
		if out.Live.Unread != "" {
			logf("warning: goal %s: items unavailable, INDEX.md carries its statement and acceptance only: %s", opts.Goal.Slug, out.Live.Unread)
		}
	}
	ownRepo := map[string]bool{}
	for _, r := range opts.Repos {
		ownRepo[repoKey(r)] = true
	}
	checkouts := map[string]string{}
	for r, dir := range opts.Checkouts {
		checkouts[repoKey(r)] = dir
	}
	var own []gateway.Document
	for _, d := range docs {
		if d.Kind == "prd" && out.PRDSlug == "" {
			out.PRDSlug = d.Slug
		}
		if d.Kind == "instructions" && ownRepo[repoKey(d.Repo)] {
			own = append(own, d)
		}
	}
	if least := len(renderIndex(out, indexGoalTitle, 0)); least > budget.BytesMax {
		logf("warning: INDEX.md alone is %d bytes at its shortest, past bytes_max %d: it is cut to fit, no document on disk, every one to fetch", least, budget.BytesMax)
		out.Dropped = own
		out.IndexBytes = len(RenderIndex(out))
		return out, nil
	}
	shared := sharedSlugs(docs)
	total := 0
	for _, d := range own {
		var body string
		if !shared[d.Slug] {
			if body, err = gw.ContextDocument(ctx, orgLogin, projectSlug, d.Slug); err != nil {
				return nil, fmt.Errorf("fetch %s: %w", d.Slug, err)
			}
		} else if b, ok := readCheckout(checkouts[repoKey(d.Repo)], d.Path); ok {
			body = b
		} else {
			// by slug the gateway may answer another repo's body
			logf("warning: instructions %s of %s: slug %q is shared and no checkout holds %s: left to INDEX.md", d.Path, d.Repo, d.Slug, d.Path)
			out.Dropped = append(out.Dropped, d)
			continue
		}
		name := contextFileName(d, out.OnDisk)
		out.Docs[name] = body
		out.OnDisk[name] = d
		// the index names the file: count it as it will be written
		if index := len(RenderIndex(out)); len(body) <= budget.DocBytesMax && total+len(body)+index <= budget.BytesMax {
			total += len(body)
			continue
		}
		delete(out.Docs, name)
		delete(out.OnDisk, name)
		out.Dropped = append(out.Dropped, d)
	}
	// a dropped document's line grows the index: fit again
	out.fit()
	out.IndexBytes = len(RenderIndex(out))
	return out, nil
}

// contextFileName is an instructions document's file on disk, named by
// its repo and path: instructions-<owner>-<repo>-<path>.md, the path
// without its .md (AGENTS.md is `agents`, docs/AGENTS.md `docs-agents`),
// the slug when the listing gives no path. One repo may hold two
// instructions documents of one slug at different paths, so the slug
// alone would put both under one name; a name another path of the same
// repo already took (docs/agents.md and docs-agents.md) gets a short
// hash of its path.
func contextFileName(d gateway.Document, taken map[string]gateway.Document) string {
	stem := Slugify(strings.TrimSuffix(strings.TrimSuffix(d.Path, ".md"), ".MD"))
	if stem == "" {
		stem = Slugify(d.Slug)
	}
	name := "instructions-" + Slugify(d.Repo) + "-" + stem + ".md"
	if t, ok := taken[name]; ok && (repoKey(t.Repo) != repoKey(d.Repo) || t.Path != d.Path || t.Slug != d.Slug) {
		sum := sha256.Sum256([]byte(d.Repo + "\x00" + d.Path + "\x00" + d.Slug))
		name = "instructions-" + Slugify(d.Repo) + "-" + stem + "-" + hex.EncodeToString(sum[:4]) + ".md"
	}
	return name
}

// checkoutsUnder is each repo's checkout, its sub-directory of dir.
func checkoutsUnder(dir string, subs map[string]string) map[string]string {
	out := make(map[string]string, len(subs))
	for repo, sub := range subs {
		out[repo] = filepath.Join(dir, sub)
	}
	return out
}

func repoKey(r string) string { return strings.ToLower(strings.TrimSpace(r)) }

// sharedSlugs is the instructions slugs more than one document of the
// listing carries — every repo's AGENTS.md is `agents`.
func sharedSlugs(docs []gateway.Document) map[string]bool {
	n := map[string]int{}
	for _, d := range docs {
		if d.Kind == "instructions" {
			n[d.Slug]++
		}
	}
	out := map[string]bool{}
	for slug, c := range n {
		if c > 1 {
			out[slug] = true
		}
	}
	return out
}

// readCheckout reads path inside the checkout dir; false when either is
// empty, the path leaves the checkout, or the file cannot be read.
func readCheckout(dir, path string) (string, bool) {
	if dir == "" || path == "" || !filepath.IsLocal(path) {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (cx *Context) bytesMax() int {
	if cx.BytesMax > 0 {
		return cx.BytesMax
	}
	return DefaultContextBytesMax
}

// fit holds the context to its cap, INDEX.md included: while the
// documents and the index as rendered pass it, the last document by
// file name leaves the disk for Dropped — named in the index, binding,
// to fetch first. RenderIndex never passes the cap itself, so it ends.
func (cx *Context) fit() {
	for len(cx.Docs) > 0 {
		total := len(RenderIndex(cx))
		names := make([]string, 0, len(cx.Docs))
		for n, body := range cx.Docs {
			names = append(names, n)
			total += len(body)
		}
		if total <= cx.bytesMax() {
			return
		}
		sort.Strings(names)
		last := names[len(names)-1]
		delete(cx.Docs, last)
		if d, ok := cx.OnDisk[last]; ok {
			delete(cx.OnDisk, last)
			cx.Dropped = append(cx.Dropped, d)
		}
	}
}

// IndexFile is the context directory's index: what is on disk and how
// to find the rest.
const IndexFile = "INDEX.md"

// RenderIndex is INDEX.md, a screen and not a table (ADR-015): the
// project; what is on disk — the instruction files by name and the repo
// each came from, and the goal section; the own repos' instructions the
// cap left off disk, binding, to fetch first; how to find the rest with
// the MCP tools; the ADRs and the TRD named as binding, searched before
// any design decision; one count line. No document is listed as a row
// and no other goal is named. The session's own goal follows: its
// statement, acceptance and items.
//
// It never passes the room the instruction files leave under the cap —
// they are binding, the goal's items can be asked for: past it the
// goal's item table is cut at a row boundary — as many rows as fit,
// then a line sending the rest to mesh_items — then its statement and
// acceptance go too. With files on disk an index still past the room is
// returned as it is, for fit to take a file off; with none it is cut at
// the cap with a closing line.
func RenderIndex(cx *Context) string {
	max := cx.bytesMax()
	for _, body := range cx.Docs {
		max -= len(body)
	}
	if s := renderIndex(cx, indexFull, -1); len(s) <= max {
		return s
	}
	if cx.Live != nil && len(cx.Live.Items) > 0 && cx.Live.Unread == "" {
		// the most rows that fit: the length grows with the rows kept
		lo, hi := 0, len(cx.Live.Items)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if len(renderIndex(cx, indexFull, mid)) <= max {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if s := renderIndex(cx, indexFull, lo); len(s) <= max {
			return s
		}
	}
	s := renderIndex(cx, indexGoalTitle, 0)
	if len(s) <= max || len(cx.Docs) > 0 {
		return s
	}
	tail := fmt.Sprintf("\n… INDEX.md cut at the context cap (%d bytes): ask `mesh_items` and `mesh_search` for the rest.\n", max)
	keep := max - len(tail)
	if keep < 0 {
		return strings.ToValidUTF8(tail[:max], "")
	}
	return strings.ToValidUTF8(s[:keep], "") + tail
}

// How much of the goal section renderIndex renders.
const (
	indexFull      = iota // statement, acceptance, items (rows of them)
	indexGoalTitle        // the goal's title, the rest to ask mesh_items for
)

// renderIndex renders INDEX.md at level with the goal's first rows
// items as table rows, every one for rows < 0.
func renderIndex(cx *Context, level, rows int) string {
	var b strings.Builder
	project := fmt.Sprintf("%q", cx.Project)
	cell := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", "\\|") }
	fmt.Fprintf(&b, "# Project %s\n\n", cx.Project)
	b.WriteString("## On disk here\n\n")
	files := make([]string, 0, len(cx.OnDisk))
	for name := range cx.OnDisk {
		files = append(files, name)
	}
	sort.Strings(files)
	for _, name := range files {
		fmt.Fprintf(&b, "- `%s` — the instructions of %s, binding\n", name, cx.OnDisk[name].Repo)
	}
	if cx.Live != nil {
		fmt.Fprintf(&b, "- this session's goal, `%s`, below: its statement, acceptance and items\n", cx.Live.Goal.Slug)
	}
	if len(cx.OnDisk) == 0 && cx.Live == nil {
		b.WriteString("- nothing but this index\n")
	}
	if len(cx.Dropped) > 0 {
		b.WriteString("\n## Binding, not on disk — fetch first\n\n")
		shared := sharedSlugs(cx.Index)
		for _, d := range cx.Dropped {
			if shared[d.Slug] {
				// mesh_document by a shared slug may answer another repo's
				fmt.Fprintf(&b, "- `%s` of %s — its instructions, binding, not on disk: read `%s` in %s's checkout before you start (slug `%s` is shared by several repos' instructions)\n", d.Path, d.Repo, d.Path, d.Repo, d.Slug)
				continue
			}
			fmt.Fprintf(&b, "- `%s` — the instructions of %s, binding, past the context cap: fetch it with `mesh_document` before you start\n", d.Slug, d.Repo)
		}
	}
	b.WriteString("\n## Everything else: fetch it\n\n")
	b.WriteString("Search before you read, and read a document only when the work touches it.\n\n")
	fmt.Fprintf(&b, "- `mesh_search` (project %s, a query) → slug, title, kind, summary\n", project)
	fmt.Fprintf(&b, "- `mesh_document` (project %s, a slug) → the body\n", project)
	b.WriteString("- `mesh_documents` and `mesh_projects` → another project's documents\n")
	b.WriteString("- `mesh_items` → live state: goals and items as the platform holds them now\n")
	b.WriteString("\nThe ADRs and the TRD are binding: search them with `mesh_search` before any design decision.\n")
	fmt.Fprintf(&b, "\n%d documents indexed, none listed here.\n", len(cx.Index))
	if cx.Live != nil {
		renderOwnGoal(&b, cx.Live, cell, level, rows)
	}
	return b.String()
}

// renderOwnGoal is INDEX.md's goal section: the session's own goal,
// its statement, acceptance and a row per item — the first rows, then a
// line for the rest, when rows >= 0.
func renderOwnGoal(b *strings.Builder, lg *LiveGoal, cell func(string) string, level, rows int) {
	g := lg.Goal
	or := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "—"
		}
		return s
	}
	fmt.Fprintf(b, "\n## Goal `%s` — %s (%s)\n\n", g.Slug, cell(g.Title), g.Status)
	b.WriteString("Read from the platform when this session started; not committed.\n")
	if level >= indexGoalTitle {
		b.WriteString("\nIts statement, acceptance and items are past the context cap: ask `mesh_items`.\n")
		return
	}
	if s := strings.TrimSpace(g.Statement); s != "" {
		fmt.Fprintf(b, "\n### Statement\n\n%s\n", s)
	}
	if s := strings.TrimSpace(g.Acceptance); s != "" {
		fmt.Fprintf(b, "\n### Acceptance\n\n%s\n", s)
	}
	b.WriteString("\n### Items\n\n")
	switch {
	case lg.Unread != "":
		b.WriteString("Unread here: ask `mesh_items`.\n")
		return
	case len(lg.Items) == 0:
		b.WriteString("No items yet.\n")
		return
	case rows == 0:
		fmt.Fprintf(b, "%d items, past the context cap: ask `mesh_items`.\n", len(lg.Items))
		return
	}
	items := lg.Items
	if rows > 0 && rows < len(items) {
		items = items[:rows]
	}
	b.WriteString("| seq | title | status | repo | branch |\n| --- | --- | --- | --- | --- |\n")
	defer func() {
		if rest := len(lg.Items) - len(items); rest > 0 {
			fmt.Fprintf(b, "\n… %d more items past the context cap (seq %d on): ask `mesh_items` for the rest.\n", rest, lg.Items[len(items)].Seq)
		}
	}()
	for _, it := range items {
		branch := or(it.Branch)
		if branch != "—" {
			branch = "`" + branch + "`"
		}
		fmt.Fprintf(b, "| %d | %s | %s | %s | %s |\n", it.Seq, cell(it.Title), it.Status, or(lg.Repos[it.RepoID]), branch)
	}
}

// MeshMCPURL is the platform's /mcp beside the gateway at gatewayURL —
// what runsheet and `mesh mcp install --remote` point at.
func MeshMCPURL(gatewayURL string) string {
	return strings.TrimRight(gatewayURL, "/") + "/mcp"
}

// MeshContextMCP is the MCP server a session gets for runsheet:
// bin's `mesh mcp-relay` to the platform's /mcp on a token file
// (harness.MeshContextRelay), so the session reads what the node's
// operator may on the node's bearer of the moment, never one that
// rotated under it (mesh-cli ADR-029). The entry names no token file
// and carries no bearer: the harness puts the session's own file in
// its directory — `<workdir>.session/`, a Codex session's CODEX_HOME;
// for a pod, a dir of the node's, the pod's relay (harness.RelayScript)
// reaching it only through the node's endpoint socket in its HOME — and
// the node's TokenFiles keeps it fresh until the session's end is
// handled.
func MeshContextMCP(bin, gatewayURL string) map[string]any {
	if gatewayURL == "" {
		return nil
	}
	if bin == "" {
		bin = "mesh"
	}
	return map[string]any{harness.BuiltinMCP: harness.MeshContextRelay(bin, MeshMCPURL(gatewayURL), "")}
}

// MeshContextHTTP is runsheet as the platform's /mcp with token as
// its header: a person's own CLI client (meshmcp.Hosted), which takes a
// fresh bearer per request, and a test's harness. No session the node
// starts gets it: a header outlives no rotation (ADR-029).
func MeshContextHTTP(gatewayURL, token string) map[string]any {
	if gatewayURL == "" || token == "" {
		return nil
	}
	return map[string]any{harness.BuiltinMCP: map[string]any{
		"type": "http", "url": MeshMCPURL(gatewayURL),
		"headers": map[string]string{"Authorization": "Bearer " + token},
	}}
}

// ContextWritten is what WriteContext put on disk (adr-016): Bytes is
// every document file written plus INDEX.md, Files the documents
// (INDEX.md not among them), and ToFetch the indexed documents left off
// disk, fetched on demand with mesh_document. PastTarget is the file
// that took the running total past ContextBytesTarget, empty while it
// held (ADR-015).
type ContextWritten struct {
	Bytes      int
	Files      int
	ToFetch    int
	PastTarget string
}

// Line is the session's one context log line after its label, e.g.
// "context 187392 bytes in 21 files, 43 documents to fetch".
func (w ContextWritten) Line() string {
	return fmt.Sprintf("context %d bytes in %d files, %d documents to fetch", w.Bytes, w.Files, w.ToFetch)
}

// TargetWarning is the session's warning line when its context passed
// ContextBytesTarget, naming the file that took it past; empty when it
// held.
func (w ContextWritten) TargetWarning() string {
	if w.PastTarget == "" {
		return ""
	}
	return fmt.Sprintf("warning: context %d bytes, past the %d-byte target: %s took it past", w.Bytes, ContextBytesTarget, w.PastTarget)
}

// WriteContext writes the context's documents under <dir>/.mesh/context
// — and INDEX.md, the screen that says what is here and how to find the
// rest — and excludes .mesh from git via .git/info/exclude. It returns
// the document file names written, in name order; the index is beside
// them, not among them. The ContextWritten is what that cost. Whatever
// an earlier session left under .mesh/context goes first, an empty
// context included.
func WriteContext(dir string, cx *Context) ([]string, ContextWritten, error) {
	var w ContextWritten
	// a reused checkout keeps the last session's context: clear it
	// first, so an empty context leaves nothing behind it
	cdir := filepath.Join(dir, ContextDir)
	if err := os.RemoveAll(cdir); err != nil {
		return nil, w, err
	}
	if cx == nil || (len(cx.Docs) == 0 && len(cx.Index) == 0 && cx.Live == nil) {
		return nil, w, nil
	}
	// the cap holds on what is written, whoever built the context
	cx.fit()
	w.ToFetch = max(len(cx.Index)-len(cx.OnDisk), 0)
	docs := cx.Docs
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		return nil, w, err
	}
	names := make([]string, 0, len(docs))
	for n := range docs {
		names = append(names, n)
	}
	sort.Strings(names)
	count := func(name string, n int) {
		w.Bytes += n
		if w.PastTarget == "" && w.Bytes > ContextBytesTarget {
			w.PastTarget = name
		}
	}
	// measured as rendered, before a byte is written: nothing past the
	// cap is ever written
	index := RenderIndex(cx)
	total := len(index)
	for _, n := range names {
		total += len(docs[n])
	}
	if total > cx.bytesMax() {
		return nil, w, fmt.Errorf("context: %d bytes rendered, past the %d-byte cap", total, cx.bytesMax())
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(cdir, n), []byte(docs[n]), 0o644); err != nil {
			return nil, w, err
		}
		count(n, len(docs[n]))
	}
	w.Files = len(names)
	if err := os.WriteFile(filepath.Join(cdir, IndexFile), []byte(index), 0o644); err != nil {
		return nil, w, err
	}
	count(IndexFile, len(index))
	if err := excludeFromGit(dir, ".mesh/"); err != nil {
		return nil, w, err
	}
	return names, w, nil
}

func excludeFromGit(dir, pattern string) error {
	common := commonGitDir(dir)
	if common == "" {
		return nil // not a git checkout (tests)
	}
	info := filepath.Join(common, "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return err
	}
	path := filepath.Join(info, "exclude")
	existing, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(l) == pattern {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		_, _ = f.WriteString("\n")
	}
	_, err = f.WriteString(pattern + "\n")
	return err
}

// commonGitDir is the git dir whose info/exclude the checkout in dir
// reads: its own .git, or for a worktree the shared clone's (git reads
// info/exclude from the common dir), "" when dir is no checkout.
func commonGitDir(dir string) string {
	dotgit := filepath.Join(dir, ".git")
	st, err := os.Stat(dotgit)
	if err != nil {
		return ""
	}
	if st.IsDir() {
		return dotgit
	}
	b, err := os.ReadFile(dotgit)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	c, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return gitdir
	}
	common := strings.TrimSpace(string(c))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	return filepath.Clean(common)
}

// Gateway is what the node needs of the platform; *gateway.Client is one.
type Gateway interface {
	MyOrgs(ctx context.Context) ([]gateway.Org, error)
	HeartbeatNode(ctx context.Context, nodeID, version string, running int, extras ...gateway.HeartbeatExtras) (*gateway.NodeAnswer, error)
	GetDesiredVersion(ctx context.Context, nodeID string) (string, error)
	// Credentials is an org's credentialIndexs rows (and its
	// enterprise's): the loop reconciles the accounts it knows against
	// their standing each tick (reconcileCredentials).
	Credentials(ctx context.Context, orgID string) ([]gateway.CredentialRow, error)
	ReadyWorkItems(ctx context.Context, orgID string) ([]gateway.WorkItem, error)
	GoalWorkItems(ctx context.Context, goalID string) ([]gateway.WorkItem, error)
	OrgWorkItems(ctx context.Context, orgID, status string) ([]gateway.WorkItem, error)
	WorkItem(ctx context.Context, id string) (*gateway.WorkItem, error)
	AgentModel(ctx context.Context, agentID string) (string, error)
	ClaimWorkItem(ctx context.Context, itemID, nodeID, harness string) error
	ReportWorkItemProgress(ctx context.Context, itemID, note string, costCents int, sessionID string) error
	// description, when given and non-empty, is the PR body
	// FinishDescription rendered: sent only when the platform's
	// FinishWorkItemInput takes it (every-actor-has-a-name item 3).
	FinishWorkItem(ctx context.Context, itemID, branch, headSha string, costCents int, usage *gateway.UsageReport, description ...string) error
	FinishWorkItemAlreadyDone(ctx context.Context, itemID, evidence string, costCents int, usage *gateway.UsageReport, description ...string) error
	FailWorkItem(ctx context.Context, itemID, errMsg string) error
	ReleaseWorkItem(ctx context.Context, itemID, nodeID, reason string) error
	// ReleasePlan hands back a goal's plan claim with why, the goal's
	// planError on the platform (adr-022, A fetch that fails is reported).
	ReleasePlan(ctx context.Context, goalID, nodeID, reason string) error
	// ReleaseGoalPlan hands back a plan claim the check refused twice
	// with the same text, the text its reason (ADR-023, a refused plan is
	// released, not re-planned); IsUnsupported on an old platform.
	ReleaseGoalPlan(ctx context.Context, goalID, nodeID, reason string) error
	IssueItemRepoToken(ctx context.Context, itemID, nodeID string) (*gateway.GitHubToken, error)
	// PlanGitHubTokens / ResearchGitHubTokens are IssueItemRepoToken for
	// a plan's or a direction's own fetch (adr-022): one token per repo
	// of the goal's or the direction's project, given only to the node
	// holding the plan's or the research's claim.
	PlanGitHubTokens(ctx context.Context, goalID, nodeID string) ([]gateway.GitHubToken, error)
	ResearchGitHubTokens(ctx context.Context, directionID, nodeID string) ([]gateway.GitHubToken, error)
	EvaluateWorkItem(ctx context.Context, itemID string, passed bool, notes string, clauses []gateway.FailedClause, costCents int, usage *gateway.UsageReport, choice gateway.EvaluateChoice) error
	// WorkItemPriorAttempts reads the evaluations already recorded
	// against an item (none, and no error, on a platform that does not
	// serve them).
	WorkItemPriorAttempts(ctx context.Context, itemID string) ([]gateway.PriorAttempt, error)
	// EvaluateWorkItemNoVerdict records an evaluation that reached no
	// verdict (gateway.ErrNoVerdictOutcome on a platform without the
	// outcome, nothing sent).
	EvaluateWorkItemNoVerdict(ctx context.Context, itemID, reason string, costCents int, usage *gateway.UsageReport, choice gateway.EvaluateChoice) error
	Goal(ctx context.Context, id string) (*gateway.Goal, error)
	Goals(ctx context.Context, orgID string) ([]gateway.Goal, error)
	ClaimPlan(ctx context.Context, goalID, nodeID string) error
	// RequestPlanReplacing asks for a narrow re-plan replacing one item
	// (gateway.ErrNoReplaceItem on a platform without replaceItem,
	// nothing sent; ADR-021's amendment 2026-10-02).
	RequestPlanReplacing(ctx context.Context, goalID, by, itemID string) error
	PlanGoal(ctx context.Context, goalID, by string, items []gateway.PlannedItem, costCents int, usage *gateway.UsageReport, questions ...gateway.PlanQuestion) error
	// TakesPlanQuestions is whether PlanGoal can carry the planner's
	// questions for the person (ADR-020).
	TakesPlanQuestions(ctx context.Context) bool
	// SetGoalCheckpoints sets the goal's checkpoints: a plan's `pr`
	// checkpoint is set before PlanGoal (ADR-023's amendment, the cut-over
	// rule).
	SetGoalCheckpoints(ctx context.Context, goalID, by string, checkpoints []string) error
	// ProposeGoalSplit hands in the planner's `split` answer — goal
	// drafts in place of a graph over the project's item cap — and
	// TakesGoalSplit is whether the platform has it (ADR-023, the size
	// rule).
	ProposeGoalSplit(ctx context.Context, goalID, by string, drafts []gateway.GoalDraft, costCents int, usage *gateway.UsageReport) error
	TakesGoalSplit(ctx context.Context) bool
	Project(ctx context.Context, id string) (*gateway.Project, error)
	Projects(ctx context.Context, orgID string) ([]gateway.Project, error)
	Directions(ctx context.Context, orgID, projectID string) ([]gateway.Direction, error)
	ClaimDirectionResearch(ctx context.Context, directionID, nodeID string) error
	RequestedRebases(ctx context.Context, orgID string) ([]gateway.WorkItem, error)
	ClaimWorkItemRebase(ctx context.Context, itemID, nodeID string) error
	// ClaimWorkItemEvaluation is the claim a node that did not build an
	// item makes to evaluate it (goal a-node-says-which-sessions-it-takes).
	ClaimWorkItemEvaluation(ctx context.Context, itemID, nodeID string) error
	// TakesEvaluationClaim is whether the platform has it: without it a
	// node evaluates only what it holds (ADR-025).
	TakesEvaluationClaim(ctx context.Context) bool
	FinishWorkItemRebase(ctx context.Context, itemID, headSha string, resolvedFiles []string, usage *gateway.UsageReport) error
	FailWorkItemRebase(ctx context.Context, itemID, reason string) error
	RecordDirectionResearch(ctx context.Context, directionID, nodeID string, research gateway.Research, costCents int, notes string, usage *gateway.UsageReport) error
	Repo(ctx context.Context, id string) (*gateway.Repo, error)
	ProjectRepos(ctx context.Context, repoIDs []string) ([]gateway.Repo, error)
	ContextDocuments(ctx context.Context, org, project string) ([]gateway.Document, error)
	ContextDocument(ctx context.Context, org, project, doc string) (string, error)
	Scorecard(ctx context.Context, org, project string) (map[string]any, error)
}
