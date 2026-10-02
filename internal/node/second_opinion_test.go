package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The rule, with the reason each case is recorded under (goal
// one-evaluator-for-ordinary-work).
func TestSecondOpinionWanted(t *testing.T) {
	correctness := []harness.FailedClause{{Clause: "the evaluator has a test", Satisfy: "add TestSecondOpinionWanted"}}
	host := []harness.FailedClause{{Clause: "the mini's load p90 under 1.5 × cores", Satisfy: "a quieter mini"}}
	for _, c := range []struct {
		name string
		d    evalDecision
		want bool
		why  string
	}{
		{"urgent", evalDecision{Priority: "urgent", FirstPassed: true}, true, "urgent/high goal"},
		{"high", evalDecision{Priority: "high", FirstPassed: true}, true, "urgent/high goal"},
		{"high, first fail", evalDecision{Priority: "high", FailedClauses: host}, true, "urgent/high goal"},
		{"normal pass", evalDecision{Priority: "normal", FirstPassed: true, Changed: []string{"internal/node/evaluate.go"}}, false, oneEvaluatorReason},
		{"low docs pass", evalDecision{Priority: "low", FirstPassed: true, Changed: []string{"docs/handbook.md"}}, false, oneEvaluatorReason},
		{"no priority", evalDecision{FirstPassed: true}, false, oneEvaluatorReason},
		{"protectedFiles", evalDecision{Priority: "normal", FirstPassed: true, ProtectedFiles: []string{"schema.sql"}}, true, "protected path schema.sql"},
		{"protectedRulePaths", evalDecision{Priority: "normal", FirstPassed: true, ProtectedRulePaths: []string{"db/**/*.sql"}, Changed: []string{"README.md", "db/migrations/0042_items.sql"}}, true, "protected path db/migrations/0042_items.sql"},
		{"protectedPaths", evalDecision{Priority: "low", FirstPassed: true, ProtectedPaths: []string{".github/workflows/*"}, Changed: []string{".github/workflows/release.yml"}}, true, "protected path .github/workflows/release.yml"},
		{"protectedPaths, no match", evalDecision{Priority: "low", FirstPassed: true, ProtectedPaths: []string{".github/workflows/*"}, Changed: []string{"docs/handbook.md"}}, false, oneEvaluatorReason},
		{"first fail, correctness", evalDecision{Priority: "normal", FailedClauses: correctness}, true, "first verdict FAIL on a correctness clause"},
		{"first fail, correctness beside a reading", evalDecision{Priority: "normal", FailedClauses: append(append([]harness.FailedClause(nil), host...), correctness...)}, true, "first verdict FAIL on a correctness clause"},
		{"first fail, host reading only", evalDecision{Priority: "normal", FailedClauses: host}, false, oneEvaluatorReason},
		{"always", evalDecision{Priority: "low", FirstPassed: true, Setting: secondOpinionAlways}, true, "second_opinion: always"},
		{"rule", evalDecision{Priority: "low", FirstPassed: true, Setting: "rule"}, false, oneEvaluatorReason},
	} {
		got, why := secondOpinionWanted(c.d)
		if got != c.want || why != c.why {
			t.Errorf("%s: (%v, %q), want (%v, %q)", c.name, got, why, c.want, c.why)
		}
	}
}

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

// The checkout's .mesh/config.yml second_opinion; anything else is the
// rule.
func TestSecondOpinionSetting(t *testing.T) {
	dir := t.TempDir()
	if got := secondOpinionSetting(dir); got != "rule" {
		t.Fatalf("no file: %q", got)
	}
	for body, want := range map[string]string{"second_opinion: always\n": "always", "second_opinion: rule\n": "rule", "second_opinion: sometimes\n": "rule", "other: 1\n": "rule", ":\n- not yaml": "rule"} {
		writeMeshConfig(t, dir, body)
		if got := secondOpinionSetting(dir); got != want {
			t.Errorf("%q: %q, want %q", body, got, want)
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

// A low goal's docs-only item is evaluated once, and the notes and
// node.log say why.
func TestOneEvaluatorLowDocsItem(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "low", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	ev.WS.(*fakeWS).changed = []string{"docs/handbook.md", "docs/adr/adr-099.md"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 0 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	in := assertChoice(t, f, "gpt-6-luna", "evaluators: 1 — normal/low goal, no protected path")
	if in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
	if !strings.Contains(logs.String(), "item ship-it/1: evaluators: 1 — normal/low goal, no protected path") {
		t.Errorf("node.log:\n%s", logs.String())
	}
}

// A high goal's platform item is evaluated twice.
func TestSecondOpinionHighPlatformItem(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "high", "opus", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	ev.WS.(*fakeWS).changed = []string{"internal/gateway/client.go"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	in := assertChoice(t, f, "gpt-6-sol", "evaluators: 2 — urgent/high goal")
	if in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
	if !strings.Contains(logs.String(), "item ship-it/1: evaluators: 2 — urgent/high goal") {
		t.Errorf("node.log:\n%s", logs.String())
	}
}

// A normal item whose first verdict fails a correctness clause is
// evaluated twice: both failing records the union of their clauses, and
// a second pass passes it with both notes.
func TestSecondOpinionOnAFirstFail(t *testing.T) {
	second := "Vet fails too.\nFAILED: the evaluator has a test || SATISFY: add it\nFAILED: go vet passes || SATISFY: fix the vet finding\nVERDICT: FAIL"
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", second, correctnessFail)
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	in := assertChoice(t, f, "gpt-6-luna", "evaluators: 2 — first verdict FAIL on a correctness clause")
	got, _ := in[gateway.FailedClausesField].([]any)
	if in["passed"] != false || len(got) != 2 {
		t.Fatalf("recorded %+v", in)
	}
	for i, want := range []string{"the evaluator has a test", "go vet passes"} {
		if c, _ := got[i].(map[string]any); c["clause"] != want {
			t.Errorf("clause %d = %v, want %q", i, c, want)
		}
	}
	notes, _ := in["notes"].(string)
	if !strings.Contains(notes, "Codex (first evaluation, gpt-6-luna): VERDICT: FAIL\nNo test covers it.") || !strings.Contains(notes, "Claude (second opinion, sonnet): VERDICT: FAIL") {
		t.Fatalf("notes:\n%s", notes)
	}

	// a second pass passes it, both notes kept
	f, ev, _, _, _, _ = familyFixture(t, "normal", "sonnet", "The test is there.\nVERDICT: PASS", correctnessFail)
	evaluateItem1(t, f, ev)
	in = assertChoice(t, f, "gpt-6-luna", "evaluators: 2 — first verdict FAIL on a correctness clause")
	notes, _ = in["notes"].(string)
	if in["passed"] != true || in[gateway.FailedClausesField] != nil || !strings.Contains(notes, "VERDICT: FAIL\nNo test covers it.") || !strings.Contains(notes, "Claude (second opinion, sonnet): VERDICT: PASS\nThe test is there.") {
		t.Fatalf("recorded %+v", in)
	}
}

// A normal item whose first fail names only a reading of the host is
// evaluated once (ADR-020: never failed for the host's number, and not
// worth a second judge).
func TestOneEvaluatorOnAHostReadingFail(t *testing.T) {
	first := "The mini was busy.\nFAILED: the mini's load p90 under 1.5 × cores || SATISFY: a quieter mini\nVERDICT: FAIL"
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", first)
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 0 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	if in := assertChoice(t, f, "gpt-6-luna", "evaluators: 1 — normal/low goal, no protected path"); in["passed"] != false {
		t.Fatalf("recorded %+v", in)
	}
}

// A normal item with protectedFiles is evaluated twice, as is one whose
// changed file matches the project's protectedPaths.
func TestSecondOpinionProtectedPath(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	f.WorkItems[item1]["protectedFiles"] = []any{"schema.sql"}
	f.WorkItems[item1]["protectedRulePaths"] = []any{"*.sql"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("protectedFiles: runs codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	assertChoice(t, f, "gpt-6-luna", "evaluators: 2 — protected path schema.sql")

	f, ev, claude, codex, _, _ = familyFixture(t, "low", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	f.Projects[projID]["protectedPaths"] = []any{"db/migrations/**"}
	ev.WS.(*fakeWS).changed = []string{"docs/x.md", "db/migrations/0042.sql"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("protectedPaths: runs codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	assertChoice(t, f, "gpt-6-luna", "evaluators: 2 — protected path db/migrations/0042.sql")
}

// A normal item under a checkout whose .mesh/config.yml says
// second_opinion: always is evaluated twice.
func TestSecondOpinionAlwaysInTheCheckout(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	ws := ev.WS.(*fakeWS)
	dir := WorkspaceSpec{Root: ws.root, GoalSlug: "ship-it", Seq: 1}.Dir()
	writeMeshConfig(t, dir, "second_opinion: always\n")
	evaluateItem1(t, f, ev)
	if len(ws.prepared) != 1 || ws.prepared[0].Dir() != dir {
		t.Fatalf("prepared %+v, want %s", ws.prepared, dir)
	}
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	assertChoice(t, f, "gpt-6-luna", "evaluators: 2 — second_opinion: always")
}
