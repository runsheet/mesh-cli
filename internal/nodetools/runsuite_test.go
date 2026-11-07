package nodetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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

// run_suite's answer names every test the step ran — one entry with
// name, package, outcome and duration each — the two failing tests'
// output carried whole, the skipped one's own counts matching the
// summary.
func TestExecuteAnswersEveryTestsResult(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: unit\n    kind: unit\n    command: go test ./...\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	run := func(_ context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		return []validate.StepResult{{Name: steps[0].Name, Command: steps[0].Command, ExitCode: 1, TestsRun: 3, TestsPassed: 0, TestsFailed: 2,
			Tests: []validate.TestResult{
				{Name: "TestFailA", Package: "pkg", Action: "fail", ElapsedMs: 12, Output: "boom a\n"},
				{Name: "TestFailB", Package: "pkg", Action: "fail", ElapsedMs: 34, Output: "boom b\n"},
				{Name: "TestSkipC", Package: "pkg", Action: "skip", ElapsedMs: 0},
			}}}, nil
	}
	ans := answerOf(t)((&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{}))
	tests, ok := ans["tests"].([]any)
	if !ok || len(tests) != 3 {
		t.Fatalf("answer %v, want a tests list of 3", ans)
	}
	byName := map[string]map[string]any{}
	for _, raw := range tests {
		e := raw.(map[string]any)
		byName[e["name"].(string)] = e
	}
	a, b, c := byName["TestFailA"], byName["TestFailB"], byName["TestSkipC"]
	if a == nil || b == nil || c == nil {
		t.Fatalf("tests %v, missing an entry", tests)
	}
	if a["package"] != "pkg" || a["outcome"] != "fail" || a["duration_ms"] != float64(12) || a["output"] != "boom a\n" {
		t.Fatalf("TestFailA entry %v", a)
	}
	if b["outcome"] != "fail" || b["duration_ms"] != float64(34) || b["output"] != "boom b\n" {
		t.Fatalf("TestFailB entry %v", b)
	}
	if c["outcome"] != "skip" || c["output"] != nil {
		t.Fatalf("TestSkipC entry %v, want no output", c)
	}
	summary, _ := ans["summary"].(string)
	if !strings.Contains(summary, "0 tests passed, 2 failed, 1 skipped") {
		t.Fatalf("summary %q", summary)
	}
	if ans["tests_truncated"] != nil {
		t.Fatalf("answer %v, want no truncation", ans)
	}
}

// A step whose tests' combined output exceeds answerTestsCap keeps the
// earlier failing tests' output whole and strips the rest's, naming
// tests_truncated and staying at or under the cap.
func TestExecuteTruncatesTestsPastTheCap(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: unit\n    kind: unit\n    command: go test ./...\n")
	frontOutput := strings.Repeat("A", 500)
	const backN = 40
	backOutput := strings.Repeat("B", 600)
	var tests []validate.TestResult
	for i := 0; i < 3; i++ {
		tests = append(tests, validate.TestResult{Name: fmt.Sprintf("TestFront%d", i), Package: "pkg", Action: "fail", ElapsedMs: 10, Output: frontOutput})
	}
	for i := 0; i < backN; i++ {
		tests = append(tests, validate.TestResult{Name: fmt.Sprintf("TestBack%d", i), Package: "pkg", Action: "fail", ElapsedMs: 10, Output: backOutput})
	}

	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	run := func(_ context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		return []validate.StepResult{{Name: steps[0].Name, Command: steps[0].Command, ExitCode: 1, TestsRun: len(tests), TestsFailed: len(tests), Tests: tests}}, nil
	}
	text, err := (&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Tests          json.RawMessage `json:"tests"`
		TestsTruncated bool            `json:"tests_truncated"`
	}
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.TestsTruncated {
		t.Fatal("tests_truncated was not set")
	}
	if len(decoded.Tests) > answerTestsCap {
		t.Fatalf("the tests list is %d bytes, over the cap of %d", len(decoded.Tests), answerTestsCap)
	}
	var list []map[string]any
	if err := json.Unmarshal(decoded.Tests, &list); err != nil || len(list) != len(tests) {
		t.Fatalf("tests list %v, err %v", list, err)
	}
	byName := map[string]map[string]any{}
	for _, e := range list {
		byName[e["name"].(string)] = e
	}
	for i := 0; i < 3; i++ {
		e := byName[fmt.Sprintf("TestFront%d", i)]
		if e == nil || e["output"] != frontOutput {
			t.Fatalf("TestFront%d entry %v, want its output whole", i, e)
		}
	}
	if e := byName[fmt.Sprintf("TestBack%d", backN-1)]; e["output"] != nil {
		t.Fatalf("the last test's output was not stripped: %v", e)
	}
}

// A step killed at its timeout with tests mid-run names each of them,
// outcome timeout, with its own running duration.
func TestExecuteNamesTestsStillRunningAtTheTimeout(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: integration\n    kind: integration\n    command: go test ./e2e\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	run := func(_ context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		return []validate.StepResult{{Name: steps[0].Name, Command: steps[0].Command, ExitCode: 1, TimedOut: true,
			Tests: []validate.TestResult{
				{Name: "TestSlowA", Package: "e2e", Action: "timeout", ElapsedMs: 1500},
				{Name: "TestSlowB", Package: "e2e", Action: "timeout", ElapsedMs: 2500},
				{Name: "TestSlowC", Package: "e2e", Action: "timeout", ElapsedMs: 3200},
			}}}, nil
	}
	ans := answerOf(t)((&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{}))
	tests, ok := ans["tests"].([]any)
	if !ok || len(tests) != 3 {
		t.Fatalf("answer %v, want 3 tests named", ans)
	}
	want := map[string]float64{"TestSlowA": 1500, "TestSlowB": 2500, "TestSlowC": 3200}
	for _, raw := range tests {
		e := raw.(map[string]any)
		name := e["name"].(string)
		ms, has := want[name]
		if !has {
			t.Fatalf("unexpected test %v", e)
		}
		if e["outcome"] != "timeout" || e["duration_ms"] != ms {
			t.Fatalf("%s entry %v, want outcome timeout and duration_ms %v", name, e, ms)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("missing tests %v", want)
	}
	summary, _ := ans["summary"].(string)
	if !strings.Contains(summary, "3 still running at the timeout") {
		t.Fatalf("summary %q", summary)
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
		// the real stream's package-level rollup event (empty Test) is
		// skipped by validate.ParseGoTestJSONTests, so the "unit" step's
		// r.Tests (preferred over the goTestEvents Report reparse) holds
		// 3 tests, not the 4 raw events in testdata/gotest.json
		if err := json.Unmarshal(m.blobs[p.OutputDigest], &out); err != nil || len(out.Tests) != 3 {
			t.Fatalf("the blob %s is not the normalised run: %v %+v", p.OutputDigest, err, out)
		}
		verifyNodeSigned(t, post, signer.NodeKey)
	}
	if !rec.Recorded(RunSuiteTool) || rec.Recorded(PushTool) {
		t.Fatal("the recorder does not say which tools it recorded")
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
	run := func(_ context.Context, ws string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
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
	res, err := HostSuites(context.Background(), t.TempDir(), steps, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ExitCode != 0 {
		t.Fatalf("the step saw a model key, or lost the node's other variables: %+v", res)
	}
}

// run_suite {suite, targets?, tests?} exports the selection to the
// suite's command as MESH_SUITE_TARGETS and MESH_SUITE_TESTS, space
// separated and unset when the call named none, and its signed record
// carries the selection exactly as named, the input digest covering it;
// a whole run's record has no selection key at all (projectmesh-io/api
// ADR 001 amendment, evidence waivers).
func TestRunSuiteRunsASelection(t *testing.T) {
	t.Setenv(SuiteTargetsEnv, "the-node-s-own")
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755)
	os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte(`suites:
  - name: unit
    kind: unit
    command: 'echo "${MESH_SUITE_TARGETS-unset}|${MESH_SUITE_TESTS-unset}" >> seen.txt'
  - name: lint
    kind: lint
    command: "true"
`), 0o644)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "base")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	tool := (&RunSuite{Workspace: dir, Recorder: rec, OnHost: true}).Tool()
	cases := []struct {
		args map[string]any
		seen string
		want *Selection
	}{
		{args: map[string]any{"suite": "unit"}, seen: "unset|unset"},
		{args: map[string]any{"suite": "unit", "tests": []any{"TestX"}}, seen: "unset|TestX", want: &Selection{Tests: []string{"TestX"}}},
		{args: map[string]any{"suite": "unit", "targets": []any{"./a/...", " ./b "}, "tests": []any{"TestX", "TestY"}},
			seen: "./a/... ./b|TestX TestY", want: &Selection{Targets: []string{"./a/...", "./b"}, Tests: []string{"TestX", "TestY"}}},
		{args: map[string]any{"suite": "unit", "targets": []any{"./a/..."}, "tests": []any{}}, seen: "./a/...|unset", want: &Selection{Targets: []string{"./a/..."}}},
	}
	inputs := map[string]bool{}
	for i, c := range cases {
		text, err := tool.Call(context.Background(), c.args)
		if err != nil {
			t.Fatal(err)
		}
		seen, _ := os.ReadFile(filepath.Join(dir, "seen.txt"))
		lines := strings.Split(strings.TrimSpace(string(seen)), "\n")
		if len(lines) != i+1 || lines[i] != c.seen {
			t.Fatalf("%v: the suite's command saw %q, want %q", c.args, lines, c.seen)
		}
		st, payload := statementOf(t, m.records[len(m.records)-1])
		if !reflect.DeepEqual(st.Predicate.Selection, c.want) {
			t.Fatalf("%v: the record's selection is %+v, want %+v (%s)", c.args, st.Predicate.Selection, c.want, text)
		}
		var raw struct {
			Predicate map[string]json.RawMessage `json:"predicate"`
		}
		json.Unmarshal(payload, &raw)
		if _, has := raw.Predicate["selection"]; has != (c.want != nil) {
			t.Fatalf("%v: the predicate's selection key %s", c.args, raw.Predicate["selection"])
		}
		if c.want != nil {
			var sel map[string]json.RawMessage
			json.Unmarshal(raw.Predicate["selection"], &sel)
			if _, has := sel["targets"]; has != (len(c.want.Targets) > 0) {
				t.Fatalf("an empty targets list was kept: %s", raw.Predicate["selection"])
			}
			if !strings.Contains(text, `"selection"`) {
				t.Fatalf("the answer does not name the selection: %s", text)
			}
		}
		if inputs[st.Predicate.InputDigest] {
			t.Fatalf("%v: two selections of one suite share an input digest", c.args)
		}
		inputs[st.Predicate.InputDigest] = true
	}
	// a name with whitespace in it cannot be told apart in its variable
	for _, bad := range []map[string]any{
		{"suite": "unit", "tests": []any{"TestX TestY"}},
		{"suite": "unit", "targets": "./..."},
		{"suite": "unit", "tests": []any{1}},
	} {
		if _, err := tool.Call(context.Background(), bad); err == nil {
			t.Fatalf("%v ran", bad)
		}
	}
}

// run_suite wired to a results location (meshapi.Brief.Results) sends
// its complete raw output there, never to Mesh's own blob storage: the
// answer names the upload's digest and location, and the record
// posted to the Recorder carries no occurrence of the raw output text
// (projectmesh-io/api docs/adr/adr-001-amendment-learnings-and-
// transcripts.md's results location).
func TestRunSuiteUploadsToTheResultsLocationNotMeshsBlobs(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: unit\n    kind: unit\n    command: go test ./...\n")

	var mu sync.Mutex
	var puts [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		puts = append(puts, b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	const rawOutput = "boom, the complete raw failure text nobody else should see\n"
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	run := func(_ context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		return []validate.StepResult{{Name: steps[0].Name, Command: steps[0].Command, ExitCode: 1, TestsRun: 1, TestsFailed: 1,
			Tests: []validate.TestResult{{Name: "TestBoom", Package: "pkg", Action: "fail", ElapsedMs: 5, Output: rawOutput}}}}, nil
	}
	loc := meshapi.ResultsLocation{UploadURL: srv.URL + "/mesh/v1/results/run-1", ExpiresAt: time.Now().Add(time.Hour)}
	tool := (&RunSuite{Workspace: dir, Recorder: rec, Run: run, Results: loc}).Tool()
	ans := answerOf(t)(tool.Call(context.Background(), map[string]any{}))

	digest, _ := ans["output_digest"].(string)
	gotLoc, _ := ans["output_location"].(string)
	if digest == "" || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("answer %v, want an output_digest", ans)
	}
	if gotLoc != loc.UploadURL {
		t.Fatalf("answer output_location %q, want %q", gotLoc, loc.UploadURL)
	}

	mu.Lock()
	gotPuts := puts
	mu.Unlock()
	if len(gotPuts) != 1 {
		t.Fatalf("the results location saw %d puts, want 1", len(gotPuts))
	}
	var uploaded SuiteOutput
	if err := json.Unmarshal(gotPuts[0], &uploaded); err != nil || len(uploaded.Tests) != 1 || uploaded.Tests[0].Output != rawOutput {
		t.Fatalf("the results location did not receive the complete raw output: %v %+v", err, uploaded)
	}
	if Digest(gotPuts[0]) != digest {
		t.Fatalf("the uploaded bytes are not %s", digest)
	}

	if len(m.blobs) != 0 {
		t.Fatalf("Mesh's own blob storage kept %d blobs; the output must never land there", len(m.blobs))
	}
	if len(m.records) != 1 {
		t.Fatalf("the recorder posted %d records, want 1", len(m.records))
	}
	st, payload := statementOf(t, m.records[0])
	for _, raw := range [][]byte{payload, m.records[0].Envelope} {
		if strings.Contains(string(raw), "nobody else should see") {
			t.Fatal("the posted record carries the raw output text")
		}
	}
	if st.Predicate.OutputDigest != digest {
		t.Fatalf("the record's output digest %q, want the upload's %q", st.Predicate.OutputDigest, digest)
	}
}

func TestSelectionEnv(t *testing.T) {
	if env := SelectionEnv(nil); env != nil {
		t.Fatalf("no selection exports %v", env)
	}
	if got := SelectionEnv(&Selection{Tests: []string{"TestA", "TestB"}}); !reflect.DeepEqual(got, []string{"MESH_SUITE_TESTS=TestA TestB"}) {
		t.Fatalf("got %v", got)
	}
}
