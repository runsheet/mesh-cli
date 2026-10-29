package nodetools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/validate"
)

// run_suite's output blob keeps each step's outcome and, from a go test
// -json report, only the final events — a test's or a package's pass,
// fail or skip — as {package, test, action, elapsed}.
func TestNormaliseGoTestJSON(t *testing.T) {
	report, err := os.ReadFile("testdata/gotest.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		results []validate.StepResult
		want    SuiteOutput
	}{
		{name: "none", want: SuiteOutput{Steps: []SuiteStep{}, Tests: []TestResult{}}},
		{name: "a step with no report", results: []validate.StepResult{{Name: "vet", Command: "go vet ./...", DurationMs: 5}},
			want: SuiteOutput{Steps: []SuiteStep{{Name: "vet", Command: "go vet ./...", DurationMs: 5}}, Tests: []TestResult{}}},
		{name: "go test -json", results: []validate.StepResult{{Name: "unit", Command: "go test -json ./...", ExitCode: 1, TestsRun: 3, TestsPassed: 1, TestsFailed: 1, Report: string(report)}},
			want: SuiteOutput{
				Steps: []SuiteStep{{Name: "unit", Command: "go test -json ./...", Exit: 1, TestsRun: 3, TestsPassed: 1, TestsFailed: 1}},
				Tests: []TestResult{
					{Package: "pkg", Test: "TestA", Action: "pass", Elapsed: 0.01},
					{Package: "pkg", Test: "TestB", Action: "fail", Elapsed: 0.02},
					{Package: "pkg", Test: "TestC", Action: "skip"},
					{Package: "pkg", Action: "pass"},
				}}},
		{name: "lines that are not events", results: []validate.StepResult{{Name: "u", Report: "PASS\nok pkg 0.1s\n{not json\n" + `{"Action":"output","Package":"p","Output":"x"}` + "\n"}},
			want: SuiteOutput{Steps: []SuiteStep{{Name: "u"}}, Tests: []TestResult{}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Normalise(c.results); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// gitIn runs git in dir for a test's repository.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func recorder(t *testing.T, m *fakeMesh, required ...string) (*Recorder, *Signer) {
	s := &Signer{NodeKey: nodeKey(t)}
	return &Recorder{Lease: Lease{JobID: "job-1", LeaseID: "lease-1", LeaseN: 1, Product: "conform", Tenant: "acme", NodeID: "node-1",
		EvidenceRequired: required}, Signer: s, Mesh: m}, s
}

// run_suite runs the checkout's validate.yml, uploads the normalised
// results as its output blob, posts a signed record whose statement
// names the lease and the blob, and answers its evidence id; the
// recorder keeps the id for the finish.
func TestRunSuiteRecordsItsRun(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755)
	os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte(`suites:
  - name: unit
    kind: unit
    command: cat events.json
    results: stdout
  - name: lint
    kind: lint
    command: "false"
`), 0o644)
	ev, _ := os.ReadFile("testdata/gotest.json")
	os.WriteFile(filepath.Join(dir, "events.json"), ev, 0o644)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "base")

	m := &fakeMesh{}
	rec, signer := recorder(t, m, RunSuiteTool)
	var heard []string
	tool := (&RunSuite{Workspace: dir, Recorder: rec, OnHost: true,
		Escalate: func(why string) string { heard = append(heard, why); return "opus" }}).Tool()
	for _, c := range []struct {
		args      map[string]any
		exit      int
		steps     int
		failures  []string
		escalated string
	}{
		{args: map[string]any{"suites": []any{"unit"}}, exit: 0, steps: 1, failures: []string{"pkg.TestB"}},
		{args: map[string]any{}, exit: 1, steps: 2, failures: []string{"pkg.TestB"}, escalated: "opus"},
	} {
		text, err := tool.Call(context.Background(), c.args)
		if err != nil {
			t.Fatal(err)
		}
		var ans struct {
			Exit         int      `json:"exit"`
			Steps        []any    `json:"steps"`
			Failing      []string `json:"failing_tests"`
			EvidenceID   string   `json:"evidence_id"`
			OutputDigest string   `json:"output_digest"`
			Head         string   `json:"head"`
			Escalated    string   `json:"escalated_to"`
		}
		json.Unmarshal([]byte(text), &ans)
		if ans.Exit != c.exit || len(ans.Steps) != c.steps || !reflect.DeepEqual(ans.Failing, c.failures) || ans.EvidenceID == "" || ans.Escalated != c.escalated {
			t.Fatalf("%v answered %s", c.args, text)
		}
		post := m.records[len(m.records)-1]
		st, payload := statementOf(t, post)
		if Digest(payload) != ans.EvidenceID {
			t.Fatalf("the evidence id is not the statement's digest")
		}
		p := st.Predicate
		if st.PredicateType != meshapi.ToolPredicateType || p.Tool != RunSuiteTool || p.JobID != "job-1" || p.LeaseID != "lease-1" || p.LeaseN != 1 ||
			p.Tenant != "acme" || p.Product != "conform" || p.Exit != c.exit || p.OutputDigest != ans.OutputDigest || len(st.Subject) != 2 {
			t.Fatalf("statement %+v", st)
		}
		var out SuiteOutput
		if err := json.Unmarshal(m.blobs[p.OutputDigest], &out); err != nil || len(out.Tests) != 4 {
			t.Fatalf("the blob %s is not the normalised run: %v %+v", p.OutputDigest, err, out)
		}
		verifyNodeSigned(t, post, signer.NodeKey)
	}
	if got := rec.IDs(); len(got) != 2 {
		t.Fatalf("the recorder keeps %v", got)
	}
	// only the red run escalates (PR 2.11, trigger "red")
	if len(heard) != 1 || !strings.HasPrefix(heard[0], "run_suite went red: ") {
		t.Fatalf("escalations heard: %q", heard)
	}
	if m.order[0] != "blob" || m.order[1] != "evidence" {
		t.Fatalf("the blob goes up before its record: %v", m.order)
	}
	if _, err := tool.Call(context.Background(), map[string]any{"suites": []any{"nope"}}); err == nil {
		t.Fatal("an unknown suite ran")
	}
}

// A record Mesh names other than by its statement's digest is refused:
// the id the model passes on must be the record's own.
func TestRecordRefusesAForeignID(t *testing.T) {
	m := &fakeMesh{wrongID: true}
	rec, _ := recorder(t, m)
	if _, err := rec.Record(context.Background(), Record{Tool: "push", InputDigest: Digest(nil)}, nil); err == nil || !strings.Contains(err.Error(), "not its digest") {
		t.Fatalf("got %v", err)
	}
	if len(rec.IDs()) != 0 {
		t.Fatal("a refused id was kept")
	}
	var none *Recorder
	if _, err := none.Record(context.Background(), Record{Tool: "push"}, nil); err == nil {
		t.Fatal("a nil recorder recorded")
	}
}

// A run_suite with no runner and no explicit opt-in to the host refuses
// before it reads validate.yml: the steps are the session's own
// commands, and the zero value never runs them as the node. A step
// that would touch a host marker does not run, and nothing is recorded.
func TestRunSuiteWithoutARunnerRefuses(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	marker := filepath.Join(t.TempDir(), "ran-on-the-host")
	os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755)
	os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte("suites:\n  - name: unit\n    kind: unit\n    command: touch "+marker+"\n"), 0o644)
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	_, err := (&RunSuite{Workspace: dir, Recorder: rec}).Tool().Call(context.Background(), map[string]any{})
	if !errors.Is(err, ErrNoSuiteRunner) {
		t.Fatalf("run_suite with no runner answered %v, want ErrNoSuiteRunner", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the session's validate.yml step ran on the host")
	}
	if len(m.records) != 0 {
		t.Fatalf("a refused run_suite recorded %d records", len(m.records))
	}
}

// With a runner, run_suite runs every step through it and never on the
// host, whatever OnHost says: a sandboxed lease's runner is its pod's.
func TestRunSuiteRunsThroughItsRunner(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	marker := filepath.Join(t.TempDir(), "ran-on-the-host")
	os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755)
	os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte("suites:\n  - name: unit\n    kind: unit\n    command: touch "+marker+"\n"), 0o644)
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	var ran []string
	run := func(_ context.Context, ws string, steps []validate.Step) ([]validate.StepResult, error) {
		var out []validate.StepResult
		for _, st := range steps {
			ran = append(ran, ws+": "+st.Command)
			out = append(out, validate.StepResult{Name: st.Name, Command: st.Command})
		}
		return out, nil
	}
	if _, err := (&RunSuite{Workspace: dir, Recorder: rec, Run: run, OnHost: true}).Tool().Call(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{dir + ": touch " + marker}; !reflect.DeepEqual(ran, want) {
		t.Fatalf("the runner ran %q, want %q", ran, want)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a step ran on the host beside its runner")
	}
}

// A suite on the host is the session's own commands, so it sees none of
// the node's model variables (ADR 001 amendment "model relay").
func TestHostSuitesSeeNoModelKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-the-nodes-own-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-the-nodes-own-token")
	t.Setenv("MESH_TEST_KEPT", "kept")
	steps := []validate.Step{{Name: "env", Command: `test -z "$ANTHROPIC_API_KEY$CLAUDE_CODE_OAUTH_TOKEN" && test "$MESH_TEST_KEPT" = kept`}}
	res, err := HostSuites(context.Background(), t.TempDir(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ExitCode != 0 {
		t.Fatalf("the step saw a model key, or lost the node's other variables: %+v", res)
	}
}
