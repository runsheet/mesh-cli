package nodetools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/validate"
)

// handbookCheckout is a git checkout of a repository that holds only
// documents and declares no validation, as GrapevineNetwork's handbooks
// do; extra files join it.
func handbookCheckout(t *testing.T, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	files := map[string]string{
		"README.md":             "# Handbook\n\nRead [the overview](specs/overview.md).\n",
		"specs/overview.md":     "---\ntitle: Overview\nkind: spec\n---\n\n# Overview\n\nBack to the [readme](../README.md).\n",
		".runsheet/release.yml": "notes: true\n",
		"releases/.gitkeep":     "",
	}
	for k, v := range extra {
		files[k] = v
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "handbook")
	return dir
}

// A repository that holds only documents and declares no validation is
// validated by the docs suite the node infers, run in process: green
// with every document a passing test, and the lease's runner — its pod,
// which has no mesh binary — never asked to run it (ADR-053 amendment
// 2026-11-16; on 16 Nov every validation of business-portal-handbook
// was handed back "declares no validation").
func TestRunSuiteValidatesADocumentsOnlyRepository(t *testing.T) {
	dir := handbookCheckout(t, nil)
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	calls := 0
	run := func(_ context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		calls++
		return nil, nil
	}
	ans := answerOf(t)((&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{}))
	if calls != 0 {
		t.Fatalf("the runner was asked %d times; the docs check runs in process", calls)
	}
	summary, _ := ans["summary"].(string)
	if ans["exit"] != float64(0) || !strings.Contains(summary, "inferred suites docs from 2 Markdown files") ||
		!strings.Contains(summary, "1 of 1 steps passed; 2 tests passed, 0 failed") {
		t.Fatalf("answer %v", ans)
	}
	post := m.records[len(m.records)-1]
	st, _ := statementOf(t, post)
	var out SuiteOutput
	if err := json.Unmarshal(m.blobs[st.Predicate.OutputDigest], &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Steps) != 1 || out.Steps[0].Name != "docs" || out.Steps[0].Command != "mesh docs-check" || out.Steps[0].Exit != 0 ||
		out.Inferred == nil || !reflect.DeepEqual(out.Inferred.Suites, []string{"docs"}) {
		t.Fatalf("the recorded run %+v", out)
	}
	if st.Predicate.Exit != 0 {
		t.Fatalf("the record's exit %d, want 0", st.Predicate.Exit)
	}
}

// A broken link makes the docs suite red, naming the document and its
// line as a failing test, so the rework reads what to fix.
func TestRunSuiteDocsCheckRedNamesTheDocument(t *testing.T) {
	dir := handbookCheckout(t, map[string]string{"specs/new.md": "# New\n\nSee [the plan](plan.md).\n"})
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	run := func(_ context.Context, _ string, _ []validate.Step, _ []string) ([]validate.StepResult, error) {
		t.Fatal("the runner ran the docs check")
		return nil, nil
	}
	text, err := (&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var ans struct {
		Exit    int      `json:"exit"`
		Failing []string `json:"failing_tests"`
		Tests   []struct {
			Name    string `json:"name"`
			Outcome string `json:"outcome"`
			Output  string `json:"output"`
		} `json:"tests"`
	}
	if err := json.Unmarshal([]byte(text), &ans); err != nil {
		t.Fatal(err)
	}
	if ans.Exit != 1 || !reflect.DeepEqual(ans.Failing, []string{"docs.specs/new.md"}) {
		t.Fatalf("answered %s", text)
	}
	found := false
	for _, tr := range ans.Tests {
		if tr.Name == "specs/new.md" && tr.Outcome == "fail" && strings.Contains(tr.Output, `specs/new.md:3: the link to "plan.md" names specs/plan.md, which is not in the repository`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failing test names the broken document: %s", text)
	}
}

// A declared validate.yml naming the docs check beside a command runs
// the check in process and only the command through the runner, the
// results in the file's order.
func TestRunSuiteRunsADeclaredDocsCheckBesideItsRunner(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: docs\n    kind: lint\n    command: mesh docs-check\n  - name: lint\n    kind: lint\n    command: echo ok\n")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# x\n"), 0o644)
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	var ran []string
	run := func(_ context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		var out []validate.StepResult
		for _, st := range steps {
			ran = append(ran, st.Name)
			out = append(out, validate.StepResult{Name: st.Name, Command: st.Command})
		}
		return out, nil
	}
	text, err := (&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ran, []string{"lint"}) {
		t.Fatalf("the runner ran %q, want only lint", ran)
	}
	var ans struct {
		Exit  int `json:"exit"`
		Steps []struct {
			Name string `json:"name"`
		} `json:"steps"`
	}
	json.Unmarshal([]byte(text), &ans)
	if ans.Exit != 0 || len(ans.Steps) != 2 || ans.Steps[0].Name != "docs" || ans.Steps[1].Name != "lint" {
		t.Fatalf("answered %s", text)
	}
}
