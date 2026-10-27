package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The finish step hands the item's change to the platform as a changeset
// (runsheet ADR 058, step 4): on the node, outside the pod, the
// changeset's tree is the tree of HEAD — the commit the builder made and
// the node validated — never the working directory's, so what the
// validation wrote into the checkout (scan-out/, scan-report.json,
// coverage files, caches) and anything else left uncommitted stays out
// of it, as it stays out of the node's push; the objects that differ from the base tree are offered to
// the platform, only those it lacks are uploaded, the changeset is
// recorded and the item finished naming it and the base commit — the
// commit whose tree is the changeset's base_tree (baseCommit), which the
// platform writes the branch's commit on (FinishWorkItemInput's
// baseShaAtFinish; a fresh item has no other base commit on record). The
// platform makes the branch from the changeset. Nothing here runs in the
// job container, and the changeset path runs no git push at all; a
// platform without the changeset routes gets the node's own push
// instead, from the node process on a per-command token (Push), never by
// exec in the pod.

// ChangesetGateway is the platform's half the finish step speaks to;
// *gateway.Client is one.
type ChangesetGateway interface {
	ChangesetMissing(ctx context.Context, itemID string, objects []gateway.ChangesetObject) ([]gateway.ChangesetObject, error)
	PutChangesetObject(ctx context.Context, itemID string, obj gateway.ChangesetObject, content []byte) error
	RecordChangeset(ctx context.Context, itemID string, cs gateway.Changeset) error
	// FinishWorkItemChangeset finishes the item on the changeset, naming
	// the base commit it is against when baseSHA is not "".
	FinishWorkItemChangeset(ctx context.Context, itemID, branch, changesetID, baseSHA string, costCents int, usage *gateway.UsageReport, description ...string) error
}

// GitFunc runs one git command in dir on the node and answers its
// stdout.
type GitFunc func(ctx context.Context, dir string, args ...string) ([]byte, error)

// Finish is the finish step's configuration.
type Finish struct {
	ItemID string
	Branch string
	// BaseRef is the ref the item's change is against (origin/main, a
	// dependency's branch); State.BaseRef when empty.
	BaseRef string
	// BaseSHA is the commit BaseRef was at when the checkout was made;
	// State.BaseSHA when empty. The changeset's base commit is it while
	// HEAD still descends from it (baseCommit).
	BaseSHA string
	// ParentChangesetID is the item's previous changeset on a rework, ""
	// on its first.
	ParentChangesetID string
	CostCents         int
	Usage             *gateway.UsageReport
	Description       string

	// Gateway is the platform's changeset half; nil takes Push.
	Gateway ChangesetGateway
	// Git runs git on the node; nil is the git binary in the workspace.
	Git GitFunc
	// Logf is where the step notes a base ref that moved on since the
	// checkout and the paths left uncommitted, which the changeset does
	// not carry; nil logs nothing.
	Logf func(format string, args ...any)
	// NewID mints the changeset's id; nil is a random uuid.
	NewID func() string
	// Push is the fallback for a platform without changesets: the node's
	// own push of Branch from dir, from the node process on a
	// per-command token, answering the pushed head. FinishPushed then
	// finishes the item on it. With either nil, a platform without
	// changesets fails the step.
	Push         func(ctx context.Context, dir, branch string) (string, error)
	FinishPushed func(ctx context.Context, itemID, branch, headSHA string) error
	// PushPinned, when set, is the node pushing the item's branch and
	// pinning its head at pinRef (refs/runsheet/changesets/<id>) through
	// its git relay, answering the commit pushed (runsheet ADR 076): the
	// changeset is then recorded as that commit and nothing is uploaded.
	// A push that fails, or a record the platform refuses, falls back to
	// the upload, as before.
	PushPinned func(ctx context.Context, dir, branch, pinRef string) (string, error)
	// Recorded runs once the changeset is recorded and before the item
	// is finished on it, with the result so far (its ChangesetID and
	// Tree set): the node's attestation of the tree it validated goes
	// here, because the platform holds an attestation to the item's
	// latest recorded changeset's tree (runsheet gateway/attestation.go,
	// signChangeset) — sent before the record it names the previous
	// attempt's changeset, or none, and is refused. nil does nothing.
	Recorded func(ctx context.Context, res *FinishResult)
}

// FinishResult is what the finish step leaves in State.Results["finish"].
type FinishResult struct {
	ChangesetID string
	// BaseSHA is the commit the changeset is against, whose tree is
	// BaseTree; the finish named it.
	BaseSHA      string
	BaseTree     string
	Tree         string
	PathsChanged []string
	Uploaded     []gateway.ChangesetObject
	// Pushed is the fallback taken: the node pushed HeadSHA.
	Pushed  bool
	HeadSHA string
	// Pinned: the node pushed the branch and pinned HeadSHA as the
	// changeset (runsheet ADR 076), and nothing was uploaded.
	Pinned bool
}

// FinishStepName is the finish step's name in a Job.
const FinishStepName = "finish"

// Step is the finish step.
func (f *Finish) Step() Step {
	return Step{Name: FinishStepName, Run: f.Run}
}

// Run computes, uploads and records the changeset and finishes the item
// on it, falling back to the node's push when the platform holds no
// changesets.
func (f *Finish) Run(ctx context.Context, state *State) error {
	if state.WorkspaceDir == "" {
		return errors.New("finish: no workspace")
	}
	if f.Gateway == nil {
		// no changeset gateway at all: the node's push, as a platform
		// without the routes gets
		res, err := f.push(ctx, state.WorkspaceDir, errors.New("no changeset gateway"))
		if res != nil {
			if state.Results == nil {
				state.Results = map[string]any{}
			}
			state.Results[FinishStepName] = res
		}
		return err
	}
	base := f.BaseRef
	if base == "" {
		base = state.BaseRef
	}
	if base == "" {
		return errors.New("finish: no base ref")
	}
	baseSHA := f.BaseSHA
	if baseSHA == "" {
		baseSHA = state.BaseSHA
	}
	res, err := f.changeset(ctx, state.WorkspaceDir, base, baseSHA)
	if gateway.IsUnsupported(err) {
		res, err = f.push(ctx, state.WorkspaceDir, err)
	}
	if res != nil {
		if state.Results == nil {
			state.Results = map[string]any{}
		}
		state.Results[FinishStepName] = res
	}
	return err
}

func (f *Finish) changeset(ctx context.Context, dir, base, checkoutSHA string) (*FinishResult, error) {
	git := f.git()
	// the builder's change is what it committed: HEAD's tree, the one the
	// node validated and attests. Nothing is staged — a sweep of the
	// working directory would carry the validation's outputs (runsheet's
	// scan suite writes scan-out/ and scan-report.json at the root) into
	// the changeset, a tree the attestation does not name and files the
	// pull request should not hold
	tree, err := gitLine(ctx, git, dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, fmt.Errorf("finish: tree of HEAD: %w", err)
	}
	if f.Logf != nil {
		if left := uncommitted(ctx, git, dir); len(left) > 0 {
			f.Logf("finish: %d path(s) left uncommitted are not in the changeset, which is HEAD's tree %s: %s", len(left), tree, strings.Join(capPaths(left, 10), ", "))
		}
	}
	baseSHA, baseTree, err := baseCommit(ctx, git, dir, base, checkoutSHA, f.Logf)
	if err != nil {
		return nil, err
	}
	paths, objects, err := changedObjects(ctx, git, dir, baseTree, tree)
	if err != nil {
		return nil, err
	}
	res := &FinishResult{BaseSHA: baseSHA, BaseTree: baseTree, Tree: tree, PathsChanged: paths}

	id := f.newID()
	cs := gateway.Changeset{ChangesetID: id, BaseTree: baseTree, Tree: tree, PathsChanged: paths, ParentChangesetID: f.ParentChangesetID}
	if f.pinned(ctx, git, dir, &cs, res) {
		return f.finishOn(ctx, res, id, baseSHA)
	}

	missing, err := f.Gateway.ChangesetMissing(ctx, f.ItemID, objects)
	if err != nil {
		return nil, fmt.Errorf("finish: changeset missing: %w", err)
	}
	offered := make(map[gateway.ChangesetObject]bool, len(objects))
	for _, o := range objects {
		offered[o] = true
	}
	for _, o := range missing {
		if !offered[o] {
			return nil, fmt.Errorf("finish: the platform asked for %s %s, not one offered", o.Type, o.SHA)
		}
		content, err := git(ctx, dir, "cat-file", o.Type, o.SHA)
		if err != nil {
			return nil, fmt.Errorf("finish: read %s %s: %w", o.Type, o.SHA, err)
		}
		if err := f.Gateway.PutChangesetObject(ctx, f.ItemID, o, content); err != nil {
			return nil, fmt.Errorf("finish: upload %s %s: %w", o.Type, o.SHA, err)
		}
		res.Uploaded = append(res.Uploaded, o)
	}

	if err := f.Gateway.RecordChangeset(ctx, f.ItemID, cs); err != nil {
		return nil, fmt.Errorf("finish: record changeset: %w", err)
	}
	return f.finishOn(ctx, res, id, baseSHA)
}

// pinned pushes the item's branch and pins its head at the changeset's
// ref through the node's git relay (PushPinned), and records the
// changeset as that commit (runsheet ADR 076 decision 5). False — no
// PushPinned, a push that failed, a head whose tree is not the
// changeset's, or a record the platform refused — leaves the upload to
// do, as before; each is logged.
func (f *Finish) pinned(ctx context.Context, git GitFunc, dir string, cs *gateway.Changeset, res *FinishResult) bool {
	if f.PushPinned == nil || f.Branch == "" {
		return false
	}
	note := func(format string, args ...any) {
		if f.Logf != nil {
			f.Logf("finish: "+format+"; uploading the changeset instead", args...)
		}
	}
	commit, err := f.PushPinned(ctx, dir, f.Branch, PinnedRefPrefix+cs.ChangesetID)
	if err != nil {
		note("pinned push: %v", err)
		return false
	}
	held, err := gitLine(ctx, git, dir, "rev-parse", commit+"^{tree}")
	if err != nil || held != cs.Tree {
		note("pushed %s holds tree %q, not the changeset's %s (%v)", commit, held, cs.Tree, err)
		return false
	}
	cs.Commit = commit
	if err := f.Gateway.RecordChangeset(ctx, f.ItemID, *cs); err != nil {
		note("record as pinned commit %s: %v", commit, err)
		cs.Commit = ""
		return false
	}
	res.HeadSHA, res.Pinned = commit, true
	return true
}

// PinnedRefPrefix is where a changeset's commit is pinned in its
// repository (runsheet ADR 076): refs/runsheet/changesets/<id>.
const PinnedRefPrefix = "refs/runsheet/changesets/"

// finishOn finishes the item on the recorded changeset id.
func (f *Finish) finishOn(ctx context.Context, res *FinishResult, id, baseSHA string) (*FinishResult, error) {
	res.ChangesetID = id
	if f.Recorded != nil {
		f.Recorded(ctx, res)
	}
	if err := f.Gateway.FinishWorkItemChangeset(ctx, f.ItemID, f.Branch, id, baseSHA, f.CostCents, f.Usage, f.Description); err != nil {
		return res, fmt.Errorf("finish: finish on changeset %s: %w", id, err)
	}
	return res, nil
}

// uncommitted lists the paths the working directory holds beyond HEAD —
// tracked changes and untracked files git does not ignore — as `git
// status --porcelain` names them; nil when git cannot say.
func uncommitted(ctx context.Context, git GitFunc, dir string) []string {
	out, err := git(ctx, dir, "status", "--porcelain", "-z", "--untracked-files=normal")
	if err != nil {
		return nil
	}
	var paths []string
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		if e[0] == 'R' || e[0] == 'C' {
			i++ // the rename's source follows
		}
		paths = append(paths, e[3:])
	}
	return paths
}

// capPaths is paths with at most n named and the rest counted.
func capPaths(paths []string, n int) []string {
	if len(paths) <= n {
		return paths
	}
	return append(append([]string(nil), paths[:n]...), fmt.Sprintf("and %d more", len(paths)-n))
}

// baseCommit is the commit the changeset is against and its tree, which
// is the changeset's base_tree: checkoutSHA — the commit base was at
// when the checkout was made — while HEAD still descends from it; else
// the merge-base of HEAD and base (a rework continued from an earlier
// attempt's branch, forked off an older base; a builder that rebased);
// else base itself. The base tree is read from that commit, so the
// finish never names a commit whose tree is not the base_tree the
// changeset's paths are diffed from. base having moved on since the
// checkout — another item's fetch of the shared clone, the base merged
// and deleted — is noted on logf and not followed: the change was built
// on the commit the checkout was made from, and the branch is written on
// it.
func baseCommit(ctx context.Context, git GitFunc, dir, base, checkoutSHA string, logf func(string, ...any)) (sha, tree string, err error) {
	refSHA, err := gitLine(ctx, git, dir, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("finish: base commit of %s: %w", base, err)
	}
	refTree, err := gitLine(ctx, git, dir, "rev-parse", refSHA+"^{tree}")
	if err != nil {
		return "", "", fmt.Errorf("finish: base tree of %s: %w", base, err)
	}
	sha = refSHA
	switch {
	case checkoutSHA != "" && isAncestor(ctx, git, dir, checkoutSHA):
		sha = checkoutSHA
	default:
		if mb, err := gitLine(ctx, git, dir, "merge-base", "HEAD", refSHA); err == nil {
			sha = mb
		}
	}
	tree = refTree
	if sha != refSHA {
		tree, err = gitLine(ctx, git, dir, "rev-parse", sha+"^{tree}")
		if err != nil {
			return "", "", fmt.Errorf("finish: tree of base commit %s: %w", sha, err)
		}
	}
	if tree != refTree && logf != nil {
		logf("finish: %s is at %s (tree %s), not the changeset's base %s (tree %s): the base moved on since the checkout; the branch is written on the commit the change was built from", base, refSHA, refTree, sha, tree)
	}
	return sha, tree, nil
}

// isAncestor says HEAD descends from sha (a commit is its own ancestor).
func isAncestor(ctx context.Context, git GitFunc, dir, sha string) bool {
	_, err := git(ctx, dir, "merge-base", "--is-ancestor", sha, "HEAD")
	return err == nil
}

// push is the fallback: the node's own push from the node process, then
// the finish on the head it pushed.
func (f *Finish) push(ctx context.Context, dir string, why error) (*FinishResult, error) {
	if f.Push == nil || f.FinishPushed == nil {
		return nil, fmt.Errorf("finish: the platform holds no changesets and no push is configured: %w", why)
	}
	head, err := f.Push(ctx, dir, f.Branch)
	if err != nil {
		return nil, fmt.Errorf("finish: push %s: %w", f.Branch, err)
	}
	res := &FinishResult{Pushed: true, HeadSHA: head}
	if err := f.FinishPushed(ctx, f.ItemID, f.Branch, head); err != nil {
		return res, fmt.Errorf("finish: finish on %s: %w", head, err)
	}
	return res, nil
}

// changedObjects answers the paths that differ between baseTree and tree
// and the objects of tree that do not appear in baseTree — every changed
// blob, every tree on a changed path, and tree itself — sorted.
// Deletions offer no object; a submodule's commit is not an object the
// changeset carries.
func changedObjects(ctx context.Context, git GitFunc, dir, baseTree, tree string) ([]string, []gateway.ChangesetObject, error) {
	out, err := git(ctx, dir, "diff-tree", "-r", "-z", "--no-renames", "--name-only", baseTree, tree)
	if err != nil {
		return nil, nil, fmt.Errorf("finish: changed paths: %w", err)
	}
	paths := splitZ(out)
	sort.Strings(paths)

	out, err = git(ctx, dir, "diff-tree", "-r", "-t", "-z", "--no-renames", baseTree, tree)
	if err != nil {
		return nil, nil, fmt.Errorf("finish: changed objects: %w", err)
	}
	seen := map[gateway.ChangesetObject]bool{}
	var objects []gateway.ChangesetObject
	add := func(o gateway.ChangesetObject) {
		if !seen[o] {
			seen[o] = true
			objects = append(objects, o)
		}
	}
	if tree != baseTree {
		add(gateway.ChangesetObject{Type: gateway.ObjectTree, SHA: tree})
	}
	// -z raw output: ":<old mode> <new mode> <old sha> <new sha> <status>\0<path>\0"
	fields := splitZ(out)
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 {
			continue
		}
		mode, sha := meta[1], meta[3]
		if strings.Trim(sha, "0") == "" {
			continue
		}
		switch {
		case mode == "040000":
			add(gateway.ChangesetObject{Type: gateway.ObjectTree, SHA: sha})
		case mode == "160000":
		default:
			add(gateway.ChangesetObject{Type: gateway.ObjectBlob, SHA: sha})
		}
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Type != objects[j].Type {
			return objects[i].Type < objects[j].Type
		}
		return objects[i].SHA < objects[j].SHA
	})
	return paths, objects, nil
}

func splitZ(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func gitLine(ctx context.Context, git GitFunc, dir string, args ...string) (string, error) {
	out, err := git(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("git %s answered nothing", strings.Join(args, " "))
	}
	return s, nil
}

func (f *Finish) git() GitFunc {
	if f.Git != nil {
		return f.Git
	}
	return RunGit
}

func (f *Finish) newID() string {
	if f.NewID != nil {
		return f.NewID()
	}
	return uuid.NewString()
}

// RunGit runs git in dir as the node process.
func RunGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
