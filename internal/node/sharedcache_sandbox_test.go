package node

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// sandboxedServed is the served-diff fixture on a fake sandbox whose
// every step records its spec and leaves what a Go module cache leaves —
// a 0444 file in a 0555 dir — in its TMPDIR and in the workspace.
func sandboxedServed(t *testing.T) (*servedFixture, *[]sandbox.AgentSpec, string) {
	t.Helper()
	s := newServedFixture(t)
	var mu sync.Mutex
	var specs []sandbox.AgentSpec
	leave := func(dir string) error {
		ro := filepath.Join(dir, "pkg", "mod", "x@v1")
		if err := os.MkdirAll(ro, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(ro, "go.mod"), []byte("module x\n"), 0o444); err != nil {
			return err
		}
		return os.Chmod(ro, 0o555)
	}
	rt := &sandbox.Fake{Agent: func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		mu.Lock()
		specs = append(specs, spec)
		mu.Unlock()
		for _, kv := range spec.Env {
			if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
				if err := leave(v); err != nil {
					return sandbox.Exit{}, err
				}
			}
		}
		return sandbox.Exit{}, leave(filepath.Join(spec.Workdir, ".step"))
	}}
	homes := filepath.Join(s.root, "homes")
	if err := os.MkdirAll(homes, 0o755); err != nil {
		t.Fatal(err)
	}
	s.r.Sandbox = &sandbox.Sandbox{Runtime: rt, TempDir: homes, Logf: t.Logf, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:v1"}}
	s.r.Inspect = func(context.Context, string, ...string) ([]byte, error) { return []byte("sha256:feedface\n"), nil }
	return s, &specs, homes
}

// A sandboxed validation of a batch candidate from the served diff runs
// each step in a pod whose mounts are the served scratch as its
// workspace and the run's own scratch (read-write), the shared cache's
// Go build cache (read-write) and its blob store (read-only) — nothing
// else of the cache root — with TMPDIR and GOTMPDIR the run's scratch
// and GOCACHE the Go cache.
func TestSharedCacheSandboxMountsTheScratchAndTheCache(t *testing.T) {
	s, specs, _ := sandboxedServed(t)
	claimBatch(t, s.r, s.batch)
	if _, err := s.r.ValidateCandidate(context.Background(), s.batch); err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, s.logs.String())
	}
	if len(*specs) == 0 {
		t.Fatalf("no step ran in a pod\n%s", s.logs.String())
	}
	cacheRoot := s.r.Cache.Root
	scratchRoot, _ := ScratchRoot(s.r.WorkRoot)
	goCache, blobs := s.r.BuildCache, filepath.Join(cacheRoot, "blobs")
	for _, spec := range *specs {
		t.Logf("mounts = %+v", spec.Mounts)
		ws := spec.Workdir
		if filepath.Dir(ws) != scratchRoot || !strings.HasPrefix(filepath.Base(ws), "served-") {
			t.Fatalf("workspace %s is not a served scratch under %s", ws, scratchRoot)
		}
		env := map[string][]string{}
		for _, kv := range spec.Env {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = append(env[k], v)
		}
		if len(env["TMPDIR"]) != 1 {
			t.Fatalf("TMPDIR = %v, want one", env["TMPDIR"])
		}
		tmp := env["TMPDIR"][0]
		if filepath.Dir(tmp) != scratchRoot || !strings.HasPrefix(filepath.Base(tmp), "validate-") {
			t.Fatalf("TMPDIR %s is not a validate scratch under %s", tmp, scratchRoot)
		}
		if !slices.Equal(env["GOTMPDIR"], []string{tmp}) || !slices.Equal(env["GOCACHE"], []string{goCache}) {
			t.Fatalf("GOTMPDIR = %v, GOCACHE = %v; want [%s], [%s]", env["GOTMPDIR"], env["GOCACHE"], tmp, goCache)
		}
		want := map[string]bool{ws: false, tmp: false, goCache: false, blobs: true}
		for _, m := range spec.Mounts {
			if ro, ok := want[m.Source]; ok {
				if m.ReadOnly != ro || m.Target != m.Source {
					t.Fatalf("mount %+v, want at its own path, read-only %v", m, ro)
				}
				delete(want, m.Source)
				continue
			}
			if m.Source == cacheRoot || strings.HasPrefix(m.Source, cacheRoot+string(filepath.Separator)) {
				t.Fatalf("mounted %s of the cache root", m.Source)
			}
			if strings.HasPrefix(m.Source, scratchRoot+string(filepath.Separator)) {
				t.Fatalf("mounted another scratch %s", m.Source)
			}
		}
		if len(want) != 0 {
			t.Fatalf("not mounted: %v", want)
		}
	}
}

// The blob store is never mounted writable: every pod of the run has it
// read-only, and the cache's blobs are read-only files.
func TestSharedCacheSandboxMountsTheBlobStoreReadOnly(t *testing.T) {
	s, specs, _ := sandboxedServed(t)
	claimBatch(t, s.r, s.batch)
	if _, err := s.r.ValidateCandidate(context.Background(), s.batch); err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, s.logs.String())
	}
	blobs := filepath.Join(s.r.Cache.Root, "blobs")
	for _, spec := range *specs {
		for _, m := range spec.Mounts {
			if (m.Source == blobs || strings.HasPrefix(blobs, m.Source+string(filepath.Separator))) && !m.ReadOnly {
				t.Fatalf("blob store writable through %+v", m)
			}
		}
		if !slices.Contains(sandbox.AgentRunArgs(sandbox.Pod{Name: "p"}, spec), blobs+":"+blobs+":ro") {
			t.Fatalf("no read-only --volume of %s", blobs)
		}
	}
	n := 0
	_ = filepath.WalkDir(blobs, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			n++
			if info, _ := d.Info(); info.Mode().Perm()&0o222 != 0 {
				t.Fatalf("blob %s is writable: %v", path, info.Mode())
			}
		}
		return nil
	})
	if n == 0 {
		t.Fatalf("no blob cached under %s", blobs)
	}
}

// Close removes the run's scratch and the served scratch once the pod
// has ended, even with a 0444 file in a 0555 dir in each; nothing is
// left under the work root, in TMPDIR or among the sandbox's HOMEs.
func TestSharedCacheSandboxMountsCloseRemovesTheScratch(t *testing.T) {
	s, specs, homes := sandboxedServed(t)
	claimBatch(t, s.r, s.batch)
	if _, err := s.r.ValidateCandidate(context.Background(), s.batch); err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, s.logs.String())
	}
	if len(*specs) == 0 {
		t.Fatal("no step ran in a pod")
	}
	for _, spec := range *specs {
		if _, err := os.Lstat(spec.Workdir); !os.IsNotExist(err) {
			t.Fatalf("served scratch %s left: %v", spec.Workdir, err)
		}
	}
	s.assertNothingLeft(t)
	if left := listTree(t, homes); len(left) != 0 {
		t.Fatalf("sandbox HOMEs left: %v", left)
	}
	if strings.Contains(s.logs.String(), "remove scratch") {
		t.Fatalf("a scratch removal failed:\n%s", s.logs.String())
	}
}

// The validator's close removes its scratch through the sandbox's
// removal even when RemoveAll alone cannot (a read-only tree), and
// after the session's pod is closed.
func TestSharedCacheSandboxMountsCloseOnAReadOnlyTree(t *testing.T) {
	base := t.TempDir()
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, TempDir: base, Logf: t.Logf, Config: sandbox.Config{Image: "img"}}
	dir := filepath.Join(base, "served")
	if err := os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte("suites:\n  - name: a\n    kind: unit\n    command: \"true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vr := validator{logf: t.Logf, Sandbox: sb, WorkRoot: filepath.Join(base, "work"), CacheRoot: filepath.Join(base, "cache"),
		Inspect: func(context.Context, string, ...string) ([]byte, error) { return []byte("sha256:x\n"), nil }}
	tg := validationTarget{dir: dir, prefix: "batch", label: "batch", session: func() string { return "s" }, served: &servedTree{}}
	v, err := vr.start(context.Background(), tg)
	if err != nil || v == nil {
		t.Fatalf("start: %v, %v", v, err)
	}
	sr, ok := v.runner.(sandboxRunner)
	if !ok || sr.cache == nil {
		t.Fatalf("runner = %#v, want a sandboxRunner with the cache", v.runner)
	}
	for _, d := range []string{sr.cache.Scratch, dir} {
		ro := filepath.Join(d, "ro")
		if err := os.MkdirAll(ro, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ro, "f"), []byte("x"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(ro, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	v.close()
	for _, d := range []string{sr.cache.Scratch, dir} {
		if _, err := os.Lstat(d); !os.IsNotExist(err) {
			t.Fatalf("%s left after close: %v", d, err)
		}
	}
	if sr.cache.GoCache != filepath.Join(base, "cache", "go") || sr.cache.Blobs != filepath.Join(base, "cache", "blobs") {
		t.Fatalf("cache = %+v", *sr.cache)
	}
}
