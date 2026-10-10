//go:build e2e

package sandbox

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// storeManifest is a lockfile's worth of packages big enough that a
// second copy of them could not hide under a tenth.
const storeManifest = `{"name":"store-e2e","version":"1.0.0","private":true,"dependencies":{"lodash":"4.17.21","typescript":"5.6.3"}}`

// TestStoreSharedByTwoInstalls installs one lockfile in two dirs on the
// node's package variables (Stores.Env, mesh-specs ADR 017 §3): the
// second node_modules costs under a tenth of the first under `du -s`
// over both (hardlinks counted once), or pnpm says it reused every
// package from the store and downloaded none. It needs pnpm and the
// registry, and skips without them.
func TestStoreSharedByTwoInstalls(t *testing.T) {
	pnpm, err := exec.LookPath("pnpm")
	if err != nil {
		t.Skip("pnpm not found; the shared store is not exercised")
	}
	root := t.TempDir()
	st := Stores{Pnpm: filepath.Join(root, "store", "pnpm"), Npm: filepath.Join(root, "store", "npm")}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PNPM_HOME" || strings.HasPrefix(strings.ToLower(k), "npm_config_") || strings.HasPrefix(strings.ToLower(k), "pnpm_config_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, st.Env()...)
	run := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(pnpm, args...)
		cmd.Dir, cmd.Env = dir, env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	a, b := filepath.Join(root, "work", "a"), filepath.Join(root, "work", "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "package.json"), []byte(storeManifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := run(a, "install", "--lockfile-only"); err != nil {
		t.Skipf("pnpm cannot resolve the manifest (no registry?): %v\n%s", err, out)
	}
	lock, err := os.ReadFile(filepath.Join(a, "pnpm-lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "pnpm-lock.yaml"), lock, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(a, "install", "--frozen-lockfile"); err != nil {
		t.Fatalf("first install: %v\n%s", err, out)
	}
	out, err := run(b, "install", "--frozen-lockfile")
	if err != nil {
		t.Fatalf("second install: %v\n%s", err, out)
	}
	t.Logf("second install:\n%s", out)
	if store, err := run(a, "store", "path"); err == nil && !strings.HasPrefix(strings.TrimSpace(store), st.Pnpm) {
		t.Errorf("pnpm's store is %s, not under the node's %s", strings.TrimSpace(store), st.Pnpm)
	}

	du, err := exec.Command("du", "-sk", filepath.Join(a, "node_modules"), filepath.Join(b, "node_modules")).Output()
	if err != nil {
		t.Fatalf("du: %v", err)
	}
	var kb []int
	for _, line := range strings.Split(strings.TrimSpace(string(du)), "\n") {
		n, err := strconv.Atoi(strings.Fields(line)[0])
		if err != nil {
			t.Fatalf("du line %q: %v", line, err)
		}
		kb = append(kb, n)
	}
	if len(kb) != 2 {
		t.Fatalf("du: %s", du)
	}
	t.Logf("du -sk over both: first %d KB, second %d KB", kb[0], kb[1])
	if kb[1]*10 < kb[0] {
		return
	}
	// a filesystem that reflinks rather than hardlinks shows du no saving;
	// pnpm's own count says the store served the second install
	m := regexp.MustCompile(`reused (\d+), downloaded (\d+)`).FindStringSubmatch(out)
	if m != nil && m[1] != "0" && m[2] == "0" {
		t.Logf("pnpm reused %s packages from the store and downloaded none", m[1])
		return
	}
	t.Errorf("the second node_modules costs %d KB beside the first's %d KB, and pnpm reports no reuse:\n%s", kb[1], kb[0], out)
}
