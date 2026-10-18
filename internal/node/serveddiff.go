package node

// serveddiff.go is a main validation's checkout without a clone (goal
// a-node-validates-from-a-served-diff-with-a-shared-cache, item 3): a
// merge batch candidate or a merged item's merge commit is read from
// GitHub's Git Data API on the minted token rather than fetched into a
// worktree of the shared clone. The commit and the compare base...head
// are read (internal/gitdata); the merge base is materialised from the
// node's shared tree cache (sharedcache.go) into a scratch dir of the
// node's own under <work root>/.scratch (scratch.go); the diff is
// applied — added and modified blobs written through the cache, so they
// are cached too, removed files deleted, renamed ones moved — and what
// is on disk is checked against the commit's tree before anything runs.
// validate.yml runs there, its suites selected by the compare's paths,
// and the attestation names the commit's sha and tree. The scratch goes
// on every path. A diff GitHub caps (gitdata.DiffTooLargeError), a tree
// it truncates (gitdata.TruncatedError), no cache, no minted token or a
// materialised tree that does not match falls back to the worktree,
// said once per run for each reason.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"projectmesh.io/mesh-cli/internal/gitdata"
	"projectmesh.io/mesh-cli/internal/validate"
)

// GitDataSource is the shared cache's BlobSource on GitHub's Git Data
// API: a commit's tree read by its commit and flattened, a blob by its
// sha. Its token rides each call's ctx (gitdata.WithToken).
type GitDataSource struct {
	Client *gitdata.Client
}

// Tree reads commit's tree recursively: its files and its submodules
// (mode 160000), never its directories.
func (s GitDataSource) Tree(ctx context.Context, repo, commit string) (CacheTree, error) {
	c, err := s.Client.Commit(ctx, repo, commit)
	if err != nil {
		return CacheTree{}, err
	}
	entries, err := s.Client.Tree(ctx, repo, c.TreeSHA)
	if err != nil {
		return CacheTree{}, err
	}
	t := CacheTree{SHA: c.TreeSHA}
	for _, e := range entries {
		switch e.Type {
		case "blob":
			t.Entries = append(t.Entries, CacheTreeEntry{Path: e.Path, Mode: e.Mode, SHA: e.SHA})
		case "commit":
			t.Entries = append(t.Entries, CacheTreeEntry{Path: e.Path, Mode: "160000", SHA: e.SHA})
		}
	}
	return t, nil
}

// Blob reads sha's bytes.
func (s GitDataSource) Blob(ctx context.Context, repo, sha string) ([]byte, error) {
	return s.Client.Blob(ctx, repo, sha)
}

// servedTree is a commit materialised from the served diff: its sha,
// its tree's sha, and the paths the compare names, a rename's old path
// beside its new.
type servedTree struct {
	head, tree string
	changed    []string
}

// servedFallback is why a main validation takes the worktree path: the
// served diff cannot give this commit (its reason, said once per run).
type servedFallback struct {
	reason string
	err    error
}

func (e *servedFallback) Error() string {
	if e.err == nil {
		return e.reason
	}
	return fmt.Sprintf("%s: %v", e.reason, e.err)
}
func (e *servedFallback) Unwrap() error { return e.err }

// fallbackOf is err a reason to take the worktree path: a fallback
// already, a diff past GitHub's cap or a tree it truncated.
func fallbackOf(err error) (*servedFallback, bool) {
	var fb *servedFallback
	switch {
	case errors.As(err, &fb):
		return fb, true
	case gitdata.IsDiffTooLarge(err):
		return &servedFallback{reason: "diff too large", err: err}, true
	case gitdata.IsTruncated(err):
		return &servedFallback{reason: "tree truncated", err: err}, true
	}
	return nil, false
}

// noteServedFallback says once per run per reason that a main validation
// took the worktree path.
func (r *Runner) noteServedFallback(prefix string, fb *servedFallback) {
	if r.servedFallbackSaid == nil {
		return
	}
	if _, said := r.servedFallbackSaid.LoadOrStore(fb.reason, true); said {
		return
	}
	r.logf("%s: served diff: %v; validating from a worktree (said once per run)", prefix, fb)
}

// servedToken is the token the served diff is read on: t's minted one,
// fresh. None — the operator's own login — is a fallback; one that
// cannot be renewed the validation's error, as a fetch's would be.
func (vr validator) servedToken(ctx context.Context, t validationTarget, name string) (string, error) {
	if t.cred == nil && t.repoTok != nil {
		return t.repoTok.Token, nil
	}
	if t.cred == nil {
		return "", &servedFallback{reason: "no minted token"}
	}
	tok, err := t.cred.ForFetch(ctx)
	if err != nil {
		return "", &fetchTokenError{name: name, err: err}
	}
	if tok == nil || tok.Token == "" {
		return "", &servedFallback{reason: "no minted token"}
	}
	return tok.Token, nil
}

// checkoutServed materialises c at a fresh scratch dir from the served
// diff and answers the dir and what was read. A *CandidateHeadError or
// *CommitNotOnBranchError reads nothing more; an error fallbackOf
// accepts is the worktree's turn; the scratch is removed on every error.
func (r *Runner) checkoutServed(ctx context.Context, t validationTarget, c mainCommit) (string, *servedTree, error) {
	if r.GitData == nil || r.Cache == nil {
		return "", nil, &servedFallback{reason: "no shared cache"}
	}
	vr := r.validator()
	tok, err := vr.servedToken(ctx, t, c.token)
	if err != nil {
		return "", nil, err
	}
	gctx := gitdata.WithToken(ctx, tok)
	gd := r.GitData
	if c.atHead {
		head, err := gd.Commit(gctx, c.repo, c.branch)
		if err != nil {
			return "", nil, validate.Classify(fmt.Errorf("read %s: %w", c.branch, err))
		}
		if head.SHA != c.sha {
			return "", nil, &CandidateHeadError{Branch: c.branch, Want: c.sha, Got: head.SHA}
		}
	} else {
		// sha is on branch when it is their merge base: branch...sha
		// then holds nothing (a diff past the cap holds something)
		on, err := gd.Compare(gctx, c.repo, c.branch, c.sha)
		if err != nil && !gitdata.IsDiffTooLarge(err) {
			return "", nil, validate.Classify(fmt.Errorf("compare %s...%s: %w", c.branch, short(c.sha), err))
		}
		if err != nil || on.MergeBaseSHA != c.sha {
			return "", nil, &CommitNotOnBranchError{Branch: c.branch, SHA: c.sha}
		}
	}
	commit, err := gd.Commit(gctx, c.repo, c.sha)
	if err != nil {
		return "", nil, validate.Classify(fmt.Errorf("read %s: %w", short(c.sha), err))
	}
	base := t.base
	if t.diffFrom != "" {
		if len(commit.Parents) == 0 {
			return "", nil, &servedFallback{reason: "no parent to diff from"}
		}
		base = commit.Parents[0]
	}
	cmp, err := gd.Compare(gctx, c.repo, base, c.sha)
	if err != nil {
		if _, ok := fallbackOf(err); ok {
			return "", nil, err
		}
		return "", nil, validate.Classify(fmt.Errorf("compare %s...%s: %w", base, short(c.sha), err))
	}
	root, err := scratchRootFor(r.Roots, r.Scopes, t.item.OrgID, r.WorkRoot)
	if err != nil {
		return "", nil, fmt.Errorf("scratch: %w", err)
	}
	subject := t.item.ID
	if t.batchID != "" {
		subject = t.batchID
	}
	dir, err := MakeScratch(root, "served", subject)
	if err != nil {
		return "", nil, err
	}
	fail := func(err error) (string, *servedTree, error) {
		if rerr := RemoveScratch(dir); rerr != nil {
			r.logf("%s: remove scratch %s: %v", t.prefix, dir, rerr)
		}
		return "", nil, err
	}
	if err := r.Cache.Materialize(gctx, c.repo, cmp.MergeBaseSHA, dir); err != nil {
		if _, ok := fallbackOf(err); ok {
			return fail(err)
		}
		return fail(fmt.Errorf("materialise %s: %w", short(cmp.MergeBaseSHA), err))
	}
	if err := r.Cache.ApplyDiff(gctx, c.repo, dir, cmp.Files); err != nil {
		if _, ok := fallbackOf(err); ok {
			return fail(err)
		}
		return fail(fmt.Errorf("apply %s...%s: %w", short(cmp.MergeBaseSHA), short(c.sha), err))
	}
	want, err := r.Cache.Tree(gctx, c.repo, c.sha)
	if err != nil {
		if _, ok := fallbackOf(err); ok {
			return fail(err)
		}
		return fail(fmt.Errorf("tree of %s: %w", short(c.sha), err))
	}
	if want.SHA != commit.TreeSHA {
		return fail(&servedFallback{reason: "tree mismatch", err: fmt.Errorf("cached tree %s, commit's %s", short(want.SHA), short(commit.TreeSHA))})
	}
	if err := checkMaterialized(dir, want); err != nil {
		return fail(&servedFallback{reason: "tree mismatch", err: err})
	}
	var changed []string
	for _, f := range cmp.Files {
		changed = append(changed, f.Filename)
		if f.PreviousFilename != "" && f.PreviousFilename != f.Filename {
			changed = append(changed, f.PreviousFilename)
		}
	}
	r.logf("%s: materialised %s from %s and %d changed file(s) of the served diff", t.prefix, short(c.sha), short(cmp.MergeBaseSHA), len(cmp.Files))
	return dir, &servedTree{head: c.sha, tree: commit.TreeSHA, changed: changed}, nil
}

// Tree is commit's flattened tree, from the cache or filled.
func (c *SharedCache) Tree(ctx context.Context, repo, commit string) (CacheTree, error) {
	if !objectID(commit) {
		return CacheTree{}, fmt.Errorf("cache: %q is not a commit sha", commit)
	}
	c.mu.RLock()
	t, _, err := c.tree(ctx, repo, commit)
	c.mu.RUnlock()
	c.persistCounts()
	return t, err
}

// ApplyDiff applies a compare's files to dst, a materialisation of its
// merge base: every removed file and every renamed file's old path
// deleted first (and the directories that leaves empty), then every
// other file's blob written at its path and mode through the cache — a
// submodule an empty dir. The cap is enforced after.
func (c *SharedCache) ApplyDiff(ctx context.Context, repo, dst string, files []gitdata.ChangedFile) error {
	err := c.applyDiff(ctx, repo, dst, files)
	c.persistCounts()
	if err != nil {
		return err
	}
	return c.Enforce(ctx)
}

func (c *SharedCache) applyDiff(ctx context.Context, repo, dst string, files []gitdata.ChangedFile) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	local := func(p string) (string, error) {
		if !filepath.IsLocal(p) {
			return "", fmt.Errorf("cache: diff path %q leaves the tree", p)
		}
		return filepath.Join(dst, filepath.FromSlash(p)), nil
	}
	for _, f := range files {
		var gone string
		switch {
		case f.Status == gitdata.StatusRemoved:
			gone = f.Filename
		case f.Status == gitdata.StatusRenamed && f.PreviousFilename != "":
			gone = f.PreviousFilename
		default:
			continue
		}
		out, err := local(gone)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(out); err != nil {
			return err
		}
		pruneEmpty(dst, filepath.Dir(out))
	}
	for _, f := range files {
		if f.Status == gitdata.StatusRemoved {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := local(f.Filename)
		if err != nil {
			return err
		}
		if f.Mode == "160000" {
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.SHA == "" {
			return fmt.Errorf("cache: %s: %s with no blob", f.Filename, f.Status)
		}
		src, _, err := c.blob(ctx, repo, f.SHA)
		if err != nil {
			return fmt.Errorf("cache: %s %s: %w", f.Filename, f.SHA, err)
		}
		// a path that was a directory at the merge base and is a file now
		if fi, err := os.Lstat(out); err == nil && fi.IsDir() {
			if err := os.RemoveAll(out); err != nil {
				return err
			}
		}
		if err := copyOut(src, out, f.Mode); err != nil {
			return fmt.Errorf("cache: %s: %w", f.Filename, err)
		}
	}
	return nil
}

// pruneEmpty removes dir and each parent below root while empty.
func pruneEmpty(root, dir string) {
	for dir != root && len(dir) > len(root) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// checkMaterialized checks dir holds want's files and nothing else: each
// path at its mode (a symlink 120000, an executable 100755, else 100644)
// with the content its blob sha names, each submodule a directory.
// Directories git does not track are ignored.
func checkMaterialized(dir string, want CacheTree) error {
	expect := make(map[string]CacheTreeEntry, len(want.Entries))
	for _, e := range want.Entries {
		expect[e.Path] = e
	}
	seen := make(map[string]bool, len(want.Entries))
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		e, ok := expect[rel]
		if d.IsDir() {
			if ok && e.Mode == "160000" {
				seen[rel] = true
				return fs.SkipDir
			}
			if ok {
				return fmt.Errorf("%s is a directory, the commit's is mode %s", rel, e.Mode)
			}
			return nil
		}
		if !ok {
			return fmt.Errorf("%s is not in the commit's tree", rel)
		}
		seen[rel] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		var data []byte
		mode := "100644"
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			mode = "120000"
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			data = []byte(target)
		case info.Mode().IsRegular():
			if info.Mode()&0o111 != 0 {
				mode = "100755"
			}
			if data, err = os.ReadFile(path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is not a file", rel)
		}
		if mode != e.Mode {
			return fmt.Errorf("%s is mode %s, the commit's %s", rel, mode, e.Mode)
		}
		if got := gitBlobID(data, len(e.SHA)); got != e.SHA {
			return fmt.Errorf("%s is blob %s, the commit's %s", rel, short(got), short(e.SHA))
		}
		return nil
	})
	if err != nil {
		return err
	}
	var missing []string
	for p := range expect {
		if !seen[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%d path(s) of the commit's tree missing, first %s", len(missing), missing[0])
	}
	return nil
}
