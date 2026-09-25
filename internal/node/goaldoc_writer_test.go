package node

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// goaldoc.go is the only writer of specs/goal-*.md (ADR 003, decision
// 4): no other non-test Go file in the module names the path or calls
// the functions that render and write it. An item's branch carries no
// goal status table.
func TestOnlyGoalDocWritesTheGoalDoc(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not at %s: %v", root, err)
	}
	writer := filepath.Join(root, "internal", "node", "goaldoc.go")
	pattern := regexp.MustCompile(`specs/goal-|"goal-"|\bWriteGoalDoc\b|\bGoalDocPath\b|\bRenderGoalDoc\b`)
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || path == writer {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(body), "\n") {
			if pattern.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("only internal/node/goaldoc.go may write specs/goal-*.md; found:\n%s", strings.Join(offenders, "\n"))
	}
}
