package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeModule(t *testing.T, gomod string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":       gomod,
		"main.go":      "package main\n\nfunc main() {}\n",
		"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFillGoModCache(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	// no network in the test: a module the cache lacks cannot be fetched
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	cache := filepath.Join(t.TempDir(), "gomod")

	t.Run("no go.mod", func(t *testing.T) {
		if ok, why := FillGoModCache(context.Background(), t.TempDir(), cache); ok || why != "no go.mod" {
			t.Fatalf("= (%v, %q)", ok, why)
		}
	})
	t.Run("a module with no dependencies is covered", func(t *testing.T) {
		dir := writeModule(t, "module example.com/x\n\ngo 1.21\n")
		if ok, why := FillGoModCache(context.Background(), dir, cache); !ok {
			t.Fatalf("not covered: %s", why)
		}
	})
	t.Run("a module the node cannot fetch is not", func(t *testing.T) {
		dir := writeModule(t, "module example.com/y\n\ngo 1.21\n\nrequire example.invalid/missing v1.0.0\n")
		ok, why := FillGoModCache(context.Background(), dir, cache)
		if ok || !strings.HasPrefix(why, "go mod download: ") {
			t.Fatalf("= (%v, %q), want a download failure", ok, why)
		}
	})
}

// A fill sets the shared cache and clears a node-wide GOFLAGS, once each.
func TestGoModFillEnv(t *testing.T) {
	env := goModFillEnv([]string{"PATH=/bin", "GOMODCACHE=/home/me/go/pkg/mod", "GOFLAGS=-mod=vendor"}, "/c/gomod")
	got := strings.Join(env, " ")
	if got != "PATH=/bin GOMODCACHE=/c/gomod GOFLAGS=" {
		t.Fatalf("env = %q", got)
	}
}
