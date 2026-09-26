package node

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
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
}

// Dir is the workspace directory.
func (s WorkspaceSpec) Dir() string {
	return filepath.Join(s.Root, s.GoalSlug, fmt.Sprint(s.Seq))
}

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
	// spec.Branch from spec.BaseRef, returning the directory.
	Prepare(ctx context.Context, spec WorkspaceSpec) (string, error)
	// Commit stages paths and commits them with msg when anything
	// changed. Only the goal's document at the goal's end uses it
	// (RecordEndedGoal, on the default branch); a build's branch is
	// committed by its session.
	Commit(ctx context.Context, dir string, paths []string, msg string) error
	// Push pushes branch to origin and returns its head sha.
	Push(ctx context.Context, dir, branch string) (string, error)
	// Diff renders what the checked-out branch changed since it left
	// origin/base: the stat, then the patch (the evaluator's evidence).
	Diff(ctx context.Context, dir, base string) (string, error)
	// Fetch clones repo (owner/name) into dir, shallow but with the
	// default branch's last FetchDepth commits, or refreshes a clone
	// already there: a read-only copy for the planner, whose `git log`
	// is how it sees what main already holds (adr-003 decision 2).
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
	// GitHubURL is GitHub's base URL (GitHubURL when empty).
	GitHubURL string
}

func (g *Git) githubURL() string {
	if g.GitHubURL != "" {
		return strings.TrimRight(g.GitHubURL, "/")
	}
	return GitHubURL
}

// Authorize implements Workspacer: origin becomes the repo's https URL
// (a `gh repo clone` may have left an ssh one, on the operator's key)
// and the checkout's own credential helper answers with tok.
func (g *Git) Authorize(ctx context.Context, spec WorkspaceSpec, tok *gateway.GitHubToken) error {
	dir := spec.Dir()
	origin := g.githubURL() + "/" + spec.Repo + ".git"
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		// no clone yet: an empty checkout Prepare fetches into
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if _, err := g.run(ctx, dir, "git", "init", "--quiet"); err != nil {
			return err
		}
		if _, err := g.run(ctx, dir, "git", "remote", "add", "origin", origin); err != nil {
			return err
		}
	} else if _, err := g.run(ctx, dir, "git", "remote", "set-url", "origin", origin); err != nil {
		return err
	}
	return WriteCredentialHelper(ctx, dir, g.githubURL(), tok)
}

func (g *Git) run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if g.Log != nil {
		fmt.Fprintf(g.Log, "$ %s %s\n%s", name, strings.Join(args, " "), out)
	}
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Prepare implements Workspacer.
func (g *Git) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	dir := spec.Dir()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if _, err := g.run(ctx, filepath.Dir(dir), "gh", "repo", "clone", spec.Repo, dir); err != nil {
			return "", err
		}
	} else if _, err := g.run(ctx, dir, "git", "fetch", "--prune", "origin"); err != nil {
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
	// a branch pushed by an earlier attempt (or just finished, for its
	// evaluation) is continued, not restarted: the evaluator's notes say
	// what to fix, and the push must fast-forward
	if _, err := g.run(ctx, dir, "git", "checkout", "--quiet", "-B", spec.Branch, "origin/"+spec.Branch); err == nil {
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
	if _, err := g.run(ctx, dir, "git", "checkout", "--quiet", "-B", spec.Branch, "origin/"+base); err != nil {
		return "", err
	}
	return dir, nil
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

// NoMergeBaseError says the checked-out branch shares no history with
// origin/<Base>: it is the item's problem (rebuilt from scratch or from an
// unrelated root), not the node's.
type NoMergeBaseError struct {
	Base, Branch string
}

func (e *NoMergeBaseError) Error() string {
	return fmt.Sprintf("branch %s shares no history with origin/%s: it was rebuilt from scratch or from an unrelated root, so there is nothing to diff it against", e.Branch, e.Base)
}

// deepenRounds bounds EnsureMergeBase: this many `--deepen` fetches (the
// step doubling from FetchDepth), then one `--unshallow`.
const deepenRounds = 3

// EnsureMergeBase makes origin/<base> and HEAD share a merge base in dir,
// fetching the base branch explicitly (a single-branch clone lacks it) and
// deepening a shallow checkout in a fixed number of steps. A branch that
// still shares none yields a *NoMergeBaseError.
func (g *Git) EnsureMergeBase(ctx context.Context, dir, base string) error {
	remote := "refs/remotes/origin/" + base
	if _, err := g.run(ctx, dir, "git", "fetch", "--quiet", "origin", "+refs/heads/"+base+":"+remote); err != nil {
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
		if _, err := g.run(ctx, dir, "git", "fetch", "--quiet", "--deepen="+strconv.Itoa(step), "origin"); err != nil {
			return err
		}
		if has() {
			return nil
		}
		step *= 2
	}
	// a repository the deepening completed is no longer shallow: nothing to unshallow
	if shallow() {
		if _, err := g.run(ctx, dir, "git", "fetch", "--quiet", "--unshallow", "origin"); err != nil {
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
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		_, err := g.run(ctx, filepath.Dir(dir), "gh", "repo", "clone", repo, dir, "--", "--depth", strconv.Itoa(FetchDepth))
		return err
	}
	if _, err := g.run(ctx, dir, "git", "fetch", "--depth", strconv.Itoa(FetchDepth), "origin"); err != nil {
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

// Push implements Workspacer.
func (g *Git) Push(ctx context.Context, dir, branch string) (string, error) {
	// --force-with-lease: a reworked branch was rebased; a fresh one is
	// unaffected, and a remote that moved under us still refuses
	if _, err := g.run(ctx, dir, "git", "push", "-u", "--force-with-lease", "origin", branch); err != nil {
		return "", err
	}
	return g.run(ctx, dir, "git", "rev-parse", "HEAD")
}

// PushedHead implements Workspacer.
func (g *Git) PushedHead(ctx context.Context, dir, branch, base string) (string, error) {
	if _, err := g.run(ctx, dir, "git", "fetch", "--prune", "origin"); err != nil {
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

// ContextDir is where the goal's Prologue documents land in a workspace.
const ContextDir = ".mesh/context"

// ContextKinds are the Prologue kinds a session reads first.
var ContextKinds = map[string]bool{"instructions": true, "adr": true, "prd": true, "trd": true}

// Context is the goal's context: the documents on disk by file name,
// the whole index of the project (every document, on disk or not), the
// project (org/slug) and its PRD slug when it lists one.
type Context struct {
	Docs    map[string]string
	Index   []gateway.Document
	OnDisk  map[string]string // slug → file name, for the index
	Project string
	PRDSlug string
	// Live is the project's goals and their items as the platform holds
	// them (adr-003 decision 3), rendered into INDEX.md; nil when the
	// platform's read failed or found none.
	Live []LiveGoal
}

// LiveGoal is one goal of the project with its items; a goal that is
// over carries none, it is listed as one line.
type LiveGoal struct {
	Goal  gateway.Goal
	Items []gateway.WorkItem
}

// FetchLive reads the project's goals and, for each live one, its items
// through the gateway. Any read failing fails the whole: a partial
// status is worse than none.
func FetchLive(ctx context.Context, gw Gateway, orgID, projectID string) ([]LiveGoal, error) {
	goals, err := gw.Goals(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var out []LiveGoal
	for _, g := range goals {
		if g.ProjectID != projectID {
			continue
		}
		lg := LiveGoal{Goal: g}
		if isLiveGoal(g.Status) {
			items, err := gw.GoalWorkItems(ctx, g.ID)
			if err != nil {
				return nil, fmt.Errorf("items of %s: %w", g.Slug, err)
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Seq < items[j].Seq })
			lg.Items = items
		}
		out = append(out, lg)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Goal.Slug < out[j].Goal.Slug })
	return out, nil
}

// AddLive puts the project's live goals and items on cx for INDEX.md.
// When the platform cannot say (an old platform, a failed read) it logs
// a warning and cx keeps its documents only.
func AddLive(ctx context.Context, gw Gateway, cx *Context, orgID, projectID string, logf func(string, ...any)) {
	live, err := FetchLive(ctx, gw, orgID, projectID)
	if err != nil {
		logf("warning: live goals and items unavailable, INDEX.md lists documents only: %v", err)
		return
	}
	cx.Live = live
}

// The size budget for design documents on disk: a PRD or TRD past
// DocBytesMax, or past what is left of ContextBytesMax, is listed in
// the index and fetched on demand instead. Instructions and ADRs are
// binding and always on disk.
const (
	DocBytesMax     = 60 * 1024
	ContextBytesMax = 240 * 1024
)

// FetchContext lists the project's Prologue documents and fetches the
// bodies of the kinds in ContextKinds within the size budget.
func FetchContext(ctx context.Context, gw Gateway, orgLogin, projectSlug string) (*Context, error) {
	docs, err := gw.PrologueDocuments(ctx, orgLogin, projectSlug)
	if err != nil {
		return nil, err
	}
	out := &Context{Docs: map[string]string{}, Index: docs, OnDisk: map[string]string{}, Project: orgLogin + "/" + projectSlug}
	total := 0
	for _, d := range docs {
		if d.Kind == "prd" && out.PRDSlug == "" {
			out.PRDSlug = d.Slug
		}
		if !ContextKinds[d.Kind] {
			continue
		}
		body, err := gw.PrologueDocument(ctx, orgLogin, projectSlug, d.Slug)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", d.Slug, err)
		}
		binding := d.Kind == "instructions" || d.Kind == "adr"
		if !binding && (len(body) > DocBytesMax || total+len(body) > ContextBytesMax) {
			continue // in the index, fetched on demand
		}
		name := d.Kind + "-" + Slugify(d.Slug) + ".md"
		if d.Kind == "instructions" && strings.TrimSpace(d.Repo) != "" {
			name = d.Kind + "-" + Slugify(d.Repo) + "-" + Slugify(d.Slug) + ".md"
		}
		out.Docs[name] = body
		out.OnDisk[d.Slug] = name
		total += len(body)
	}
	return out, nil
}

// IndexFile is the context directory's index of the whole project.
const IndexFile = "INDEX.md"

// RenderIndex lists every document of the project: kind, title, slug,
// where it lives, and whether it is on disk here or fetched on demand.
func RenderIndex(cx *Context) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Project %s — every indexed document\n\n", cx.Project)
	b.WriteString("On disk here: read the file. Not on disk: fetch it with the `prologue_document` tool (project " + fmt.Sprintf("%q", cx.Project) + ", the slug). Search with `prologue_search`.\n\n")
	b.WriteString("| kind | title | what it says | slug | repo/path | here |\n| --- | --- | --- | --- | --- | --- |\n")
	cell := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", "\\|") }
	for _, d := range cx.Index {
		here := "fetch"
		if n, ok := cx.OnDisk[d.Slug]; ok {
			here = "`" + n + "`"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | `%s` | %s/%s | %s |\n", d.Kind, cell(d.Title), cell(d.Summary), d.Slug, d.Repo, d.Path, here)
	}
	if len(cx.Live) > 0 {
		renderLive(&b, cx.Live, cell)
	}
	return b.String()
}

// renderLive is INDEX.md's live section: each live goal with a row per
// item, then the goals that are over, one line each.
func renderLive(b *strings.Builder, live []LiveGoal, cell func(string) string) {
	b.WriteString("\n## Live goals and items\n\n")
	b.WriteString("Read from the platform when this session started — the one place to read status from; no status table in git is current. Not committed.\n")
	or := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "—"
		}
		return s
	}
	var over []LiveGoal
	for _, lg := range live {
		g := lg.Goal
		if !isLiveGoal(g.Status) {
			over = append(over, lg)
			continue
		}
		fmt.Fprintf(b, "\n### `%s` — %s (%s)\n\n", g.Slug, cell(g.Title), g.Status)
		if len(lg.Items) == 0 {
			b.WriteString("No items yet.\n")
			continue
		}
		b.WriteString("| seq | title | status | branch | PR |\n| --- | --- | --- | --- | --- |\n")
		for _, it := range lg.Items {
			branch := or(it.Branch)
			if branch != "—" {
				branch = "`" + branch + "`"
			}
			fmt.Fprintf(b, "| %d | %s | %s | %s | %s |\n", it.Seq, cell(it.Title), it.Status, branch, or(it.PRURL))
		}
	}
	if len(over) > 0 {
		b.WriteString("\n### Finished goals\n\n")
		for _, lg := range over {
			fmt.Fprintf(b, "- `%s` — %s (%s)\n", lg.Goal.Slug, cell(lg.Goal.Title), lg.Goal.Status)
		}
	}
}

// PrologueMCP is the MCP server a session gets for Prologue: the
// platform's /mcp with the node's own token, so the session reads what
// the node's operator may.
func PrologueMCP(gatewayURL, token string) map[string]any {
	if gatewayURL == "" || token == "" {
		return nil
	}
	return map[string]any{"prologue": map[string]any{
		"type": "http", "url": strings.TrimRight(gatewayURL, "/") + "/mcp",
		"headers": map[string]string{"Authorization": "Bearer " + token},
	}}
}

// WriteContext writes the context's documents under <dir>/.mesh/context
// — and INDEX.md, the whole project's listing — and excludes .mesh from
// git via .git/info/exclude. It returns the document file names written,
// in reading order (instructions, adr, prd, trd); the index is beside
// them, not among them.
func WriteContext(dir string, cx *Context) ([]string, error) {
	if cx == nil || (len(cx.Docs) == 0 && len(cx.Index) == 0 && len(cx.Live) == 0) {
		return nil, nil
	}
	docs := cx.Docs
	cdir := filepath.Join(dir, ContextDir)
	if err := os.RemoveAll(cdir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(docs))
	for n := range docs {
		names = append(names, n)
	}
	order := map[string]int{"instructions": 0, "adr": 1, "prd": 2, "trd": 3}
	rank := func(n string) int {
		k, _, _ := strings.Cut(n, "-")
		if r, ok := order[k]; ok {
			return r
		}
		return 9
	}
	sort.Slice(names, func(i, j int) bool {
		if rank(names[i]) != rank(names[j]) {
			return rank(names[i]) < rank(names[j])
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(cdir, n), []byte(docs[n]), 0o644); err != nil {
			return nil, err
		}
	}
	if len(cx.Index) > 0 || len(cx.Live) > 0 {
		if err := os.WriteFile(filepath.Join(cdir, IndexFile), []byte(RenderIndex(cx)), 0o644); err != nil {
			return nil, err
		}
	}
	if err := excludeFromGit(dir, ".mesh/"); err != nil {
		return nil, err
	}
	return names, nil
}

func excludeFromGit(dir, pattern string) error {
	info := filepath.Join(dir, ".git", "info")
	if st, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !st.IsDir() {
		return nil // not a git checkout (tests)
	}
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

// Gateway is what the node needs of the platform; *gateway.Client is one.
type Gateway interface {
	MyOrgs(ctx context.Context) ([]gateway.Org, error)
	HeartbeatNode(ctx context.Context, nodeID, version string, running int, extras ...gateway.HeartbeatExtras) error
	GetDesiredVersion(ctx context.Context, nodeID string) (string, error)
	ReadyWorkItems(ctx context.Context, orgID string) ([]gateway.WorkItem, error)
	GoalWorkItems(ctx context.Context, goalID string) ([]gateway.WorkItem, error)
	OrgWorkItems(ctx context.Context, orgID, status string) ([]gateway.WorkItem, error)
	WorkItem(ctx context.Context, id string) (*gateway.WorkItem, error)
	AgentModel(ctx context.Context, agentID string) (string, error)
	ClaimWorkItem(ctx context.Context, itemID, nodeID, harness string) error
	ReportWorkItemProgress(ctx context.Context, itemID, note string, costCents int, sessionID string) error
	FinishWorkItem(ctx context.Context, itemID, branch, headSha string, costCents int) error
	FinishWorkItemAlreadyDone(ctx context.Context, itemID, evidence string, costCents int) error
	FailWorkItem(ctx context.Context, itemID, errMsg string) error
	ReleaseWorkItem(ctx context.Context, itemID, reason string) error
	IssueItemRepoToken(ctx context.Context, itemID, nodeID string) (*gateway.GitHubToken, error)
	EvaluateWorkItem(ctx context.Context, itemID string, passed bool, notes string, costCents int) error
	Goal(ctx context.Context, id string) (*gateway.Goal, error)
	Goals(ctx context.Context, orgID string) ([]gateway.Goal, error)
	ClaimPlan(ctx context.Context, goalID, nodeID string) error
	PlanGoal(ctx context.Context, goalID, by string, items []gateway.PlannedItem, costCents int) error
	Project(ctx context.Context, id string) (*gateway.Project, error)
	Projects(ctx context.Context, orgID string) ([]gateway.Project, error)
	Directions(ctx context.Context, orgID, projectID string) ([]gateway.Direction, error)
	ClaimDirectionResearch(ctx context.Context, directionID, nodeID string) error
	RequestedRebases(ctx context.Context, orgID string) ([]gateway.WorkItem, error)
	ClaimWorkItemRebase(ctx context.Context, itemID, nodeID string) error
	FinishWorkItemRebase(ctx context.Context, itemID, headSha string, resolvedFiles []string) error
	FailWorkItemRebase(ctx context.Context, itemID, reason string) error
	RecordDirectionResearch(ctx context.Context, directionID, nodeID string, proposals []gateway.GoalProposal, costCents int, notes string) error
	Repo(ctx context.Context, id string) (*gateway.Repo, error)
	Repos(ctx context.Context, accountID, ownerLogin string) ([]gateway.Repo, error)
	PrologueDocuments(ctx context.Context, org, project string) ([]gateway.Document, error)
	PrologueDocument(ctx context.Context, org, project, doc string) (string, error)
}
