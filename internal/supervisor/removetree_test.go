package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

// A job directory holding a read-only Go module cache is removed.
func TestRemoveTreeRemovesAReadOnlyModuleCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lease")
	mod := filepath.Join(dir, "mesh-home", "go", "pkg", "mod", "example.com", "m@v1.0.0")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module m\\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{mod, filepath.Dir(mod)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeTree(dir); err != nil {
		t.Fatalf("removeTree: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still there: %v", err)
	}
}
