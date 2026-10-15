package node

// The node's one cache of base trees (goal
// a-node-validates-from-a-served-diff-with-a-shared-cache, item 2). A
// validation materialises the commit it runs on into a scratch dir
// rather than checking it out of a clone of its own, and every
// validation on the node shares what is cached: a blob fetched once is
// copied into every scratch that wants it after.
//
// The cache lives under the node's cache root, SharedCacheRoot
// (<user cache dir>/mesh/cache), beside the Go build cache
// (BuildCacheDir, its go/), owned by the node's user and never a
// session's. It is content-addressed:
//
//	blobs/<sha[:2]>/<sha>          a blob's bytes, read-only
//	trees/<sha[:2]>/<sha>.json     a commit's tree, flattened: path, mode, blob
//	commits/<sha[:2]>/<sha>        a commit's tree sha
//
// A miss is filled from the node's shared clone of the repo (ADR-028,
// SharedClonePath) when the clone has the object, else from the
// BlobSource, once however many validations want it at the same time.
// The whole root, go/ included, is held under a byte cap by Enforce.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SharedCacheRoot is the node's cache root, <user cache dir>/mesh/cache:
// the shared tree cache and, as go/, the Go build cache.
func SharedCacheRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mesh", "cache"), nil
}

// CacheTreeEntry is one file of a commit's tree: its path from the
// repo's root, its git mode (100644, 100755, 120000, 160000) and its
// blob's sha.
type CacheTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	SHA  string `json:"sha"`
}

// CacheTree is a commit's tree, flattened to its files.
type CacheTree struct {
	SHA     string           `json:"sha"`
	Entries []CacheTreeEntry `json:"entries"`
}

// BlobSource is where a miss the shared clone cannot fill is read from:
// a commit's flattened tree and a blob's bytes. Item 3 wires GitHub's
// Git Data reads to it.
type BlobSource interface {
	Tree(ctx context.Context, repo, commit string) (CacheTree, error)
	Blob(ctx context.Context, repo, sha string) ([]byte, error)
}

// SharedCache is the node's one tree cache. Root and MaxBytes are
// required; the rest have defaults or are optional.
type SharedCache struct {
	// Root is the cache root (SharedCacheRoot); go/ under it is the Go
	// build cache, counted against MaxBytes and cleaned last.
	Root string
	// ReposRoot is where the shared clones are (ReposRoot()); "" asks
	// no clone.
	ReposRoot string
	// Source fills what the shared clone cannot; nil, such a miss is an
	// error.
	Source BlobSource
	// MaxBytes is the cap on everything under Root (node.yaml's
	// validation_cache_max_bytes, Config.ValidationCacheCap); 0 is none.
	MaxBytes int64
	// State, when set, is where the hit and miss counts are persisted
	// (RunState.WriteCacheCounts) and read back from at first use.
	State *RunState
	// Logf is node.log; nil logs nothing.
	Logf func(format string, args ...any)
	// Now stamps an entry's last use; nil is time.Now.
	Now func() time.Time
	// Git runs git in dir and returns its stdout; nil runs git.
	Git func(ctx context.Context, dir string, args ...string) ([]byte, error)
	// CleanGo empties the Go build cache at dir; nil runs `go clean
	// -cache` with GOCACHE=dir.
	CleanGo func(ctx context.Context, dir string) error

	// mu is held shared by Materialize and exclusively by Enforce, so
	// nothing is evicted from under a copy.
	mu sync.RWMutex
	// fills is the miss in flight per key: one fill, the rest wait.
	fills sync.Map // key → *cacheFill

	countsMu sync.Mutex
	// persistMu holds a snapshot and its write together, so a later
	// snapshot is never overwritten on disk by an earlier one.
	persistMu   sync.Mutex
	countsOnce  sync.Once
	counts      CacheCounts
	overCapSaid bool
}

type cacheFill struct {
	done chan struct{}
	err  error
}

// Counts is the hits and misses so far, those persisted before this
// process included.
func (c *SharedCache) Counts() CacheCounts {
	c.loadCounts()
	c.countsMu.Lock()
	defer c.countsMu.Unlock()
	return c.counts
}

func (c *SharedCache) loadCounts() {
	c.countsOnce.Do(func() {
		if c.State == nil {
			return
		}
		if got, ok := c.State.CacheCounts(); ok {
			c.countsMu.Lock()
			c.counts = got
			c.countsMu.Unlock()
		}
	})
}

func (c *SharedCache) count(f func(*CacheCounts)) {
	c.loadCounts()
	c.countsMu.Lock()
	f(&c.counts)
	c.countsMu.Unlock()
}

func (c *SharedCache) persistCounts() {
	if c.State == nil {
		return
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	if err := c.State.WriteCacheCounts(c.Counts()); err != nil {
		c.logf("cache: counts: %v", err)
	}
}

func (c *SharedCache) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *SharedCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *SharedCache) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if c.Git != nil {
		return c.Git(ctx, dir, args...)
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (c *SharedCache) blobPath(sha string) string {
	return filepath.Join(c.Root, "blobs", shardOf(sha), sha)
}

func (c *SharedCache) treePath(sha string) string {
	return filepath.Join(c.Root, "trees", shardOf(sha), sha+".json")
}

func (c *SharedCache) commitPath(sha string) string {
	return filepath.Join(c.Root, "commits", shardOf(sha), sha)
}

func shardOf(sha string) string {
	if len(sha) < 2 {
		return "_"
	}
	return sha[:2]
}

// objectID is a hex git object id, sha1 or sha256; anything else is
// refused before it names a path.
func objectID(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}

// Materialize writes commit's files of repo into dst, a scratch dir:
// each from the cached blob, a miss filled first. A file in dst is the
// node's own copy, writable, its executable bit as the tree says; a
// symlink is a symlink, a submodule an empty dir. Every entry is
// counted a hit or a miss, and the counts are persisted after. Every
// Materialize enforces the cap after it, a hit too: go/ beside the
// trees grows with every validation, whatever this one filled.
func (c *SharedCache) Materialize(ctx context.Context, repo, commit, dst string) error {
	if !objectID(commit) {
		return fmt.Errorf("cache: %q is not a commit sha", commit)
	}
	_, err := c.materialize(ctx, repo, commit, dst)
	c.persistCounts()
	if err != nil {
		return err
	}
	return c.Enforce(ctx)
}

func (c *SharedCache) materialize(ctx context.Context, repo, commit, dst string) (bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tree, filled, err := c.tree(ctx, repo, commit)
	if err != nil {
		return filled, err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return filled, err
	}
	for _, e := range tree.Entries {
		if err := ctx.Err(); err != nil {
			return filled, err
		}
		if !filepath.IsLocal(e.Path) {
			return filled, fmt.Errorf("cache: tree %s: path %q leaves the tree", tree.SHA, e.Path)
		}
		out := filepath.Join(dst, filepath.FromSlash(e.Path))
		if e.Mode == "160000" {
			if err := os.MkdirAll(out, 0o755); err != nil {
				return filled, err
			}
			continue
		}
		src, f, err := c.blob(ctx, repo, e.SHA)
		filled = filled || f
		if err != nil {
			return filled, fmt.Errorf("cache: %s %s: %w", e.Path, e.SHA, err)
		}
		if err := copyOut(src, out, e.Mode); err != nil {
			return filled, fmt.Errorf("cache: %s: %w", e.Path, err)
		}
	}
	return filled, nil
}

// copyOut writes the cached blob at src to out as mode says: a symlink
// to the blob's text, else a regular file 0755 or 0644.
func copyOut(src, out, mode string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if mode == "120000" {
		target, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		_ = os.Remove(out)
		return os.Symlink(string(target), out)
	}
	perm := fs.FileMode(0o644)
	if mode == "100755" {
		perm = 0o755
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(out, perm)
}

// tree is commit's flattened tree, from the cache (a hit) or filled (a
// miss: the shared clone, else the source).
func (c *SharedCache) tree(ctx context.Context, repo, commit string) (CacheTree, bool, error) {
	if t, ok := c.cachedTree(commit); ok {
		c.count(func(n *CacheCounts) { n.TreeHits++ })
		return t, false, nil
	}
	var t CacheTree
	filled, err := c.fillOnce("tree:"+commit, func() (bool, error) {
		if got, ok := c.cachedTree(commit); ok {
			t = got
			return false, nil
		}
		got, err := c.treeFromClone(ctx, repo, commit)
		if err != nil {
			if c.Source == nil {
				return false, fmt.Errorf("cache: tree of %s: %w", commit, err)
			}
			got, err = c.Source.Tree(ctx, repo, commit)
			if err != nil {
				return false, fmt.Errorf("cache: tree of %s: %w", commit, err)
			}
		}
		if !objectID(got.SHA) {
			return false, fmt.Errorf("cache: tree of %s: %q is not a tree sha", commit, got.SHA)
		}
		data, err := json.Marshal(got)
		if err != nil {
			return false, err
		}
		if err := c.writeEntry(c.treePath(got.SHA), data); err != nil {
			return false, err
		}
		if err := c.writeEntry(c.commitPath(commit), []byte(got.SHA+"\n")); err != nil {
			return false, err
		}
		t = got
		return true, nil
	})
	if err != nil {
		return CacheTree{}, false, err
	}
	if filled {
		c.count(func(n *CacheCounts) { n.TreeMisses++ })
		return t, true, nil
	}
	// another fill had it first: read what it wrote
	if t.SHA == "" {
		got, ok := c.cachedTree(commit)
		if !ok {
			return CacheTree{}, false, fmt.Errorf("cache: tree of %s: filled but not found", commit)
		}
		t = got
	}
	c.count(func(n *CacheCounts) { n.TreeHits++ })
	return t, false, nil
}

// cachedTree reads commit's tree from the cache and marks both its
// entries used.
func (c *SharedCache) cachedTree(commit string) (CacheTree, bool) {
	ptr, err := os.ReadFile(c.commitPath(commit))
	if err != nil {
		return CacheTree{}, false
	}
	treeSHA := strings.TrimSpace(string(ptr))
	if !objectID(treeSHA) {
		return CacheTree{}, false
	}
	data, err := os.ReadFile(c.treePath(treeSHA))
	if err != nil {
		return CacheTree{}, false
	}
	var t CacheTree
	if json.Unmarshal(data, &t) != nil || t.SHA != treeSHA {
		return CacheTree{}, false
	}
	c.touch(c.commitPath(commit))
	c.touch(c.treePath(treeSHA))
	return t, true
}

// clonePath is repo's shared clone when it is there, else "".
func (c *SharedCache) clonePath(repo string) string {
	if c.ReposRoot == "" || repo == "" {
		return ""
	}
	dir := SharedClonePath(c.ReposRoot, repo)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return ""
	}
	return dir
}

var errNotInClone = errors.New("not in the shared clone")

func (c *SharedCache) treeFromClone(ctx context.Context, repo, commit string) (CacheTree, error) {
	dir := c.clonePath(repo)
	if dir == "" {
		return CacheTree{}, errNotInClone
	}
	out, err := c.git(ctx, dir, "rev-parse", "--verify", "--quiet", commit+"^{tree}")
	if err != nil {
		return CacheTree{}, errNotInClone
	}
	t := CacheTree{SHA: strings.TrimSpace(string(out))}
	out, err = c.git(ctx, dir, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return CacheTree{}, err
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return CacheTree{}, fmt.Errorf("ls-tree: cannot read %q", rec)
		}
		t.Entries = append(t.Entries, CacheTreeEntry{Path: path, Mode: fields[0], SHA: fields[2]})
	}
	return t, nil
}

// blob is the cached path of blob sha, filled first on a miss; filled
// says this call fetched it.
func (c *SharedCache) blob(ctx context.Context, repo, sha string) (string, bool, error) {
	if !objectID(sha) {
		return "", false, fmt.Errorf("%q is not a blob sha", sha)
	}
	path := c.blobPath(sha)
	if c.touch(path) {
		c.count(func(n *CacheCounts) { n.BlobHits++ })
		return path, false, nil
	}
	filled, err := c.fillOnce("blob:"+sha, func() (bool, error) {
		if c.touch(path) {
			return false, nil
		}
		data, err := c.blobFromClone(ctx, repo, sha)
		if err != nil {
			if c.Source == nil {
				return false, err
			}
			if data, err = c.Source.Blob(ctx, repo, sha); err != nil {
				return false, err
			}
		}
		if got := gitBlobID(data, len(sha)); got != sha {
			return false, fmt.Errorf("content hashes to %s", got)
		}
		return true, c.writeEntry(path, data)
	})
	if err != nil {
		return "", false, err
	}
	if filled {
		c.count(func(n *CacheCounts) { n.BlobMisses++ })
	} else {
		c.count(func(n *CacheCounts) { n.BlobHits++ })
	}
	return path, filled, nil
}

func (c *SharedCache) blobFromClone(ctx context.Context, repo, sha string) ([]byte, error) {
	dir := c.clonePath(repo)
	if dir == "" {
		return nil, errNotInClone
	}
	return c.git(ctx, dir, "cat-file", "blob", sha)
}

// gitBlobID is the git object id of data as a blob, sha1 for a 40-hex
// id and sha256 for a 64.
func gitBlobID(data []byte, hexLen int) string {
	var h hash.Hash = sha1.New()
	if hexLen == 64 {
		h = sha256.New()
	}
	h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// fillOnce runs fill for key unless a fill of key is in flight, in
// which case it waits for that one and returns its error, filled
// false. fill re-checks the cache first: a fill that finished before
// this one started has left the entry there.
func (c *SharedCache) fillOnce(key string, fill func() (bool, error)) (bool, error) {
	f := &cacheFill{done: make(chan struct{})}
	if v, loaded := c.fills.LoadOrStore(key, f); loaded {
		w := v.(*cacheFill)
		<-w.done
		return false, w.err
	}
	filled, err := fill()
	f.err = err
	c.fills.Delete(key)
	close(f.done)
	return filled, err
}

// writeEntry writes data to path atomically, read-only: the cache's
// files are never changed in place, only replaced or evicted.
func (c *SharedCache) writeEntry(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fill-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	c.touch(path)
	return nil
}

// touch marks path used now; false when it is not there.
func (c *SharedCache) touch(path string) bool {
	now := c.now()
	return os.Chtimes(path, now, now) == nil
}

type cacheFile struct {
	path string
	size int64
	used time.Time
}

// Enforce holds the cache root under MaxBytes: least recently used
// trees, commits and blobs first, then `go clean -cache` on go/ when
// the rest is not enough. Still over after both is a finding, logged
// once until the cache is back under: `cache over cap: <size> of
// <cap>`.
func (c *SharedCache) Enforce(ctx context.Context) error {
	if c.MaxBytes <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	total, entries, err := c.scan()
	if err != nil {
		return err
	}
	if total > c.MaxBytes {
		sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
		for _, e := range entries {
			if total <= c.MaxBytes {
				break
			}
			if err := os.Remove(e.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				c.logf("cache: evict %s: %v", e.path, err)
				continue
			}
			total -= e.size
		}
	}
	goDir := filepath.Join(c.Root, "go")
	if total > c.MaxBytes {
		if _, err := os.Stat(goDir); err == nil {
			if err := c.cleanGo(ctx, goDir); err != nil {
				c.logf("cache: go clean -cache: %v", err)
			}
			if total, _, err = c.scan(); err != nil {
				return err
			}
		}
	}
	if total > c.MaxBytes {
		if !c.overCapSaid {
			c.overCapSaid = true
			c.logf("cache over cap: %s of %s", formatBytes(total), formatBytes(c.MaxBytes))
		}
		return nil
	}
	c.overCapSaid = false
	return nil
}

// scan is the size of everything under Root and the evictable entries
// — blobs, trees and commits — with when each was last used.
func (c *SharedCache) scan() (int64, []cacheFile, error) {
	var total int64
	var entries []cacheFile
	evictable := map[string]bool{"blobs": true, "trees": true, "commits": true}
	err := filepath.WalkDir(c.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		rel, _ := filepath.Rel(c.Root, path)
		top, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
		if evictable[top] && !strings.HasPrefix(d.Name(), ".fill-") {
			entries = append(entries, cacheFile{path: path, size: info.Size(), used: info.ModTime()})
		}
		return nil
	})
	return total, entries, err
}

func (c *SharedCache) cleanGo(ctx context.Context, dir string) error {
	if c.CleanGo != nil {
		return c.CleanGo(ctx, dir)
	}
	cmd := exec.CommandContext(ctx, "go", "clean", "-cache")
	cmd.Env = append(os.Environ(), "GOCACHE="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// formatBytes is n in the largest binary unit it reaches: 512 B, 3.2
// GiB.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
