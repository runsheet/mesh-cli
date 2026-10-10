package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSumsLinesAreSha256sumFormat(t *testing.T) {
	dir := t.TempDir()
	artefacts := map[string][]byte{
		"mesh-linux-arm64":  []byte("arm64 binary"),
		"mesh-darwin-arm64": []byte("darwin binary"),
		"mesh-linux-amd64":  []byte("amd64 binary"),
	}
	var names []string
	for name, data := range artefacts {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}

	got, err := WriteSums(dir, names)
	if err != nil {
		t.Fatalf("WriteSums: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read SHA256SUMS: %v", err)
	}
	if !bytes.Equal(got, onDisk) {
		t.Fatalf("returned %q, wrote %q", got, onDisk)
	}

	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	order := []string{"mesh-darwin-arm64", "mesh-linux-amd64", "mesh-linux-arm64"}
	if len(lines) != len(order) {
		t.Fatalf("got %d lines, want %d: %q", len(lines), len(order), got)
	}
	for i, line := range lines {
		hexsum, name, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("line %q is not `<sha256 hex>  <name>`", line)
		}
		if name != order[i] {
			t.Errorf("line %d names %q, want %q (sorted by name)", i, name, order[i])
		}
		want := sha256.Sum256(artefacts[name])
		if hexsum != hex.EncodeToString(want[:]) {
			t.Errorf("%s: sum %s, want %x", name, hexsum, want)
		}
	}
}

func TestWriteSumsMissingArtefactIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mesh-linux-amd64"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteSums(dir, []string{"mesh-linux-amd64", "mesh-darwin-arm64"}); err == nil {
		t.Fatal("WriteSums with a missing artefact: want an error, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "SHA256SUMS")); !os.IsNotExist(err) {
		t.Errorf("SHA256SUMS written despite a missing artefact (stat: %v)", err)
	}
}
