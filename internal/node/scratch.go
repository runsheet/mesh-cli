package node

// A host session's scratch is the node's, not /tmp's (goal
// a-host-sessions-scratch-dies-with-the-session). An unsandboxed
// validation used to inherit the node's TMPDIR, so every step's mktemp,
// `go test`'s t.TempDir, go-build* and a project's own prefixes landed in
// /tmp and stayed there (dl360 2026-10-15: 67G runsheet-*, 42G
// latchkey-*, 13G go-build*). Now each run gets a directory of its own
// under the node's scratch root — <work root>/.scratch, on the work
// root's filesystem, where the node's disk already is and where the
// node, not the OS, decides what goes — set as its TMPDIR and GOTMPDIR
// and removed when the run ends; and every run shares one build cache,
// <user cache dir>/mesh/cache/go, as its GOCACHE, so the cache is reused
// across runs rather than rebuilt into a scratch that is thrown away.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ScratchDirName is the scratch root's name under the work root. It is a
// dot directory, as .claims and .locks are, so nothing that walks the
// work root for checkouts or sessions takes it for one.
const ScratchDirName = ".scratch"

// ScratchRoot is <workRoot>/.scratch; with workRoot "" it is under the
// node's own WorkRoot (~/.mesh/work/.scratch).
func ScratchRoot(workRoot string) (string, error) {
	if workRoot == "" {
		wr, err := WorkRoot()
		if err != nil {
			return "", err
		}
		workRoot = wr
	}
	return filepath.Join(workRoot, ScratchDirName), nil
}

// BuildCacheDir is the node's one Go build cache, shared by every host
// validation: <user cache dir>/mesh/cache/go (os.UserCacheDir:
// $XDG_CACHE_HOME or ~/.cache on Linux, ~/Library/Caches on macOS).
func BuildCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mesh", "cache", "go"), nil
}

// MakeScratch makes a fresh directory for one run under root,
// <root>/<kind>-<subject>-<rand> (validate-<item>-1a2b3c4d), root made
// if missing.
func MakeScratch(root, kind, subject string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("scratch root: %w", err)
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	name := kind
	if s := Slugify(subject); s != "" {
		name += "-" + s
	}
	dir := filepath.Join(root, name+"-"+hex.EncodeToString(b[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("scratch: %w", err)
	}
	return dir, nil
}

// RemoveScratch removes a run's scratch dir and everything in it. The Go
// module cache a step may have put there (`go mod download` under a
// GOPATH in it) is read-only, which RemoveAll alone cannot delete from:
// a failed first pass makes every directory under dir writable and tries
// again. A dir already gone is nil.
func RemoveScratch(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	err := os.RemoveAll(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// scratchEnv is env with TMPDIR and GOTMPDIR set to scratch and GOCACHE
// to cache, each once: an inherited value is replaced, not shadowed by a
// second entry after it. An empty value leaves its variable as it was.
func scratchEnv(env []string, scratch, cache string) []string {
	set := map[string]string{}
	if scratch != "" {
		set["TMPDIR"], set["GOTMPDIR"] = scratch, scratch
	}
	if cache != "" {
		set["GOCACHE"] = cache
	}
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := set[name]; ok {
			continue
		}
		out = append(out, kv)
	}
	for _, name := range []string{"TMPDIR", "GOTMPDIR", "GOCACHE"} {
		if v, ok := set[name]; ok {
			out = append(out, name+"="+v)
		}
	}
	return out
}
