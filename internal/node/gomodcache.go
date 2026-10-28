package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// goModFillTimeout bounds one fill of the shared Go module cache: the
// download of a repository's whole build list on a cold cache.
const goModFillTimeout = 10 * time.Minute

// goModCacheDir is the node's shared Go module cache under the cache
// root: beside go/ (the build cache) and blobs/.
func goModCacheDir(root string) string { return filepath.Join(root, "gomod") }

// FillGoModCache fills the node's shared Go module cache for the
// checkout at dir and says whether a validation's steps can build it
// from that cache alone, mounted read-only as their GOMODCACHE. On
// dl360, 2026-10-28, every validation downloaded its own ~770 MB module
// cache into its pod's HOME; read-only and shared, a module is fetched
// once per node and its pages are cached once.
//
// The fill runs on the host as the node: `go mod download` with
// GOMODCACHE the shared cache, then `go list -deps -test ./...` with
// GOPROXY=off to prove nothing the build needs is missing. Neither runs
// the repository's code. A checkout with no go.mod, a node with no go on
// its PATH, a module the node cannot fetch (a private one), or a build
// list the cache does not cover answers false with why, and the run
// keeps its own writable cache as before. The Go module cache's own
// file locks make concurrent fills safe.
func FillGoModCache(ctx context.Context, dir, cache string) (bool, string) {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return false, "no go.mod"
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return false, "no go on the node's PATH"
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return false, err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, goModFillTimeout)
	defer cancel()
	env := goModFillEnv(os.Environ(), cache)
	if out, err := runGo(ctx, goBin, dir, env, "mod", "download"); err != nil {
		return false, "go mod download: " + lastLine(out, err)
	}
	if out, err := runGo(ctx, goBin, dir, append(env, "GOPROXY=off"), "list", "-deps", "-test", "./..."); err != nil {
		return false, "the cache does not cover the build: " + lastLine(out, err)
	}
	return true, ""
}

// goModFillEnv is the node's environment for a fill: GOMODCACHE the
// shared cache and GOFLAGS cleared, so a node-wide -mod=vendor or
// -modcacherw does not change what lands in it.
func goModFillEnv(environ []string, cache string) []string {
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if name == "GOMODCACHE" || name == "GOFLAGS" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GOMODCACHE="+cache, "GOFLAGS=")
}

func runGo(ctx context.Context, goBin, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, goBin, args...)
	cmd.Dir, cmd.Env = dir, env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out.Bytes(), fmt.Errorf("timed out after %s", goModFillTimeout)
	}
	return out.Bytes(), err
}

// lastLine is the last non-empty line of out, or err's text.
func lastLine(out []byte, err error) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
}
