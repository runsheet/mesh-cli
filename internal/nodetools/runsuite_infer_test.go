package nodetools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A checkout with no .runsheet/validate.yml runs the suites the node
// infers from it (ADR-053): a Go module's go-build, go-vet and go-test
// run on the host through the same validate path a declared file's do,
// the tests counted from go test -json, and the answer, the summary,
// the signed record's blob and the node's log each say what was
// inferred, from what.
func TestRunSuiteInfersTheSuitesOfARepoWithNoValidateYml(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	for name, body := range map[string]string{
		"go.mod":      "module example.com/inferred\n\ngo 1.21\n",
		"add.go":      "package inferred\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": "package inferred\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "base")
	// the module stands alone, whatever workspace the test runs in
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")

	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	var logged []string
	tool := (&RunSuite{Workspace: dir, Recorder: rec, OnHost: true,
		Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }}).Tool()
	text, err := tool.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var ans struct {
		Exit     int         `json:"exit"`
		Summary  string      `json:"summary"`
		Steps    []SuiteStep `json:"steps"`
		Inferred struct {
			From   []string `json:"from"`
			Suites []string `json:"suites"`
			File   string   `json:"validate_yml"`
		} `json:"inferred"`
		Tests []struct {
			Name    string `json:"name"`
			Outcome string `json:"outcome"`
		} `json:"tests"`
	}
	if err := json.Unmarshal([]byte(text), &ans); err != nil {
		t.Fatal(err)
	}
	if ans.Exit != 0 {
		t.Fatalf("exit %d: %s", ans.Exit, text)
	}
	want := []string{"go-build", "go-vet", "go-test"}
	var ran []string
	for _, st := range ans.Steps {
		ran = append(ran, st.Name)
	}
	if !reflect.DeepEqual(ran, want) || !reflect.DeepEqual(ans.Inferred.Suites, want) || !reflect.DeepEqual(ans.Inferred.From, []string{"go.mod"}) {
		t.Fatalf("ran %q, inferred %+v; want %q from go.mod", ran, ans.Inferred, want)
	}
	if ans.Steps[2].TestsPassed != 1 || len(ans.Tests) != 1 || ans.Tests[0].Name != "TestAdd" || ans.Tests[0].Outcome != "pass" {
		t.Fatalf("go-test %+v, tests %+v; want TestAdd passed", ans.Steps[2], ans.Tests)
	}
	if !strings.Contains(ans.Summary, "no .runsheet/validate.yml: inferred suites go-build, go-vet, go-test from go.mod") {
		t.Fatalf("summary %q", ans.Summary)
	}
	if !strings.Contains(ans.Inferred.File, "name: go-test") {
		t.Fatalf("inferred file %q", ans.Inferred.File)
	}
	if want := "run_suite: no validate.yml: inferred suites go-build, go-vet, go-test from go.mod"; !reflect.DeepEqual(logged, []string{want}) {
		t.Fatalf("logged %q, want %q", logged, want)
	}
	st, _ := statementOf(t, m.records[len(m.records)-1])
	var out SuiteOutput
	if err := json.Unmarshal(m.blobs[st.Predicate.OutputDigest], &out); err != nil || out.Inferred == nil || !reflect.DeepEqual(out.Inferred.Suites, want) {
		t.Fatalf("the blob carries no inferred profile: %v %+v", err, out.Inferred)
	}
}

// A checkout with nothing the node can infer suites from is refused,
// the refusal naming what was looked for.
func TestRunSuiteRefusesARepoWithNothingToInfer(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644)
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	_, err := (&RunSuite{Workspace: dir, Recorder: rec, OnHost: true}).Tool().Call(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "the suites cannot run") || !strings.Contains(err.Error(), "looked for go.mod") {
		t.Fatalf("err %v, want a cannot-run naming what was looked for", err)
	}
	if len(m.records) != 0 {
		t.Fatal("a refused run was recorded")
	}
}
