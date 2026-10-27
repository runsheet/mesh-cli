package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The protected-path glob: path.Match per segment, ** any number of
// segments, a trailing / the directory.
func TestSecondOpinionGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"schema.sql", "schema.sql", true},
		{"schema.sql", "db/schema.sql", false},
		{"**/schema.sql", "db/schema.sql", true},
		{"**/schema.sql", "schema.sql", true},
		{"migrations/**", "migrations/001.sql", true},
		{"migrations/**", "migrations/a/b.sql", true},
		{"migrations/", "migrations/a/b.sql", true},
		{"migrations/*", "migrations/a/b.sql", false},
		{"internal/*/client.go", "internal/gateway/client.go", true},
		{"docs/*.md", "docs/adr/x.md", false},
		{"", "anything", false},
	} {
		if got := globMatch(c.pattern, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.pattern, c.name, got)
		}
	}
}

func writeMeshConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".mesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".mesh", "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertChoice checks every evaluateWorkItem the fake recorded carries
// evaluatedModel and the evaluators line.
func assertChoice(t *testing.T, f *gatewaytest.Fake, model, line string) map[string]any {
	t.Helper()
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 {
		t.Fatalf("evaluate calls: %d", len(calls))
	}
	in := calls[0].Input
	if in[gateway.EvaluatedModelField] != model {
		t.Errorf("evaluatedModel = %v, want %q", in[gateway.EvaluatedModelField], model)
	}
	if notes, _ := in["notes"].(string); !strings.Contains(notes, "\n\n"+line+"\n(evaluated on ") {
		t.Errorf("notes lack %q before the tail:\n%s", line, notes)
	}
	return in
}

const correctnessFail = "No test covers it.\nFAILED: the evaluator has a test || SATISFY: add TestSecondOpinionWanted\nVERDICT: FAIL"

// The checkout's .mesh/config.yml protected_paths, read beside its
// second_opinion; absent or unreadable is none.
func TestSecondOpinionCheckoutProtectedPaths(t *testing.T) {
	dir := t.TempDir()
	if got := readCheckoutConfig(dir); got.ProtectedPaths != nil {
		t.Fatalf("no file: %+v", got)
	}
	writeMeshConfig(t, dir, "second_opinion: always\nprotected_paths:\n  - schema.json\n  - db/**\n")
	got := readCheckoutConfig(dir)
	if strings.Join(got.ProtectedPaths, ",") != "schema.json,db/**" {
		t.Fatalf("read %+v", got)
	}
	writeMeshConfig(t, dir, ":\n- not yaml")
	if got := readCheckoutConfig(dir); got.ProtectedPaths != nil {
		t.Fatalf("unreadable: %+v", got)
	}
}
