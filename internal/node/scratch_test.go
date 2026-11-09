package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveScratchTakesReadOnlyDirs(t *testing.T) {
	root := t.TempDir()
	dir, err := MakeScratch(root, "validate", "item-1")
	if err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(dir, "gopath", "pkg", "mod", "x@v1")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "f.go"), []byte("package x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mod, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := RemoveScratch(dir); err != nil {
		t.Fatalf("RemoveScratch: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("%s still there: %v", dir, err)
	}
	if err := RemoveScratch(dir); err != nil {
		t.Fatalf("RemoveScratch of a gone dir: %v", err)
	}
}

// TestMakeScratchNameIsShort: a run's scratch dir keeps its subject's
// first eight characters, so a step's own nesting under its TMPDIR — a
// test Postgres's socket, <tmp>/latchkey-testenv.XXXXXX/.s.PGSQL.5432 —
// still fits the 107 bytes a Unix socket path may have under the node's
// usual work root.
func TestMakeScratchNameIsShort(t *testing.T) {
	root := t.TempDir()
	dir, err := MakeScratch(root, "validate", "847dc509-c104-5a3f-932a-992bd9c2edb3")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(dir)
	if !strings.HasPrefix(base, "validate-847dc509-") || len(base) != len("validate-847dc509-")+8 {
		t.Fatalf("scratch name %q, want validate-847dc509-<8 hex>", base)
	}
	socket := filepath.Join("/home/anythink/.mesh/work", ScratchDirName, base, "latchkey-testenv.C3G7QE", ".s.PGSQL.5432")
	if len(socket) > 107 {
		t.Fatalf("a test Postgres socket under the scratch is %d bytes, over 107: %s", len(socket), socket)
	}
	if dir, err := MakeScratch(root, "validate", "a-b-c-d-e-f-g"); err != nil || strings.Contains(filepath.Base(dir), "--") {
		t.Fatalf("a cut subject ends on a dash: %q %v", dir, err)
	}
}
