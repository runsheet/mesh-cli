package validate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// gitRepo inits a git repo at dir with one commit containing the given
// files (path -> content), and returns its HEAD sha. See
// internal/node/workspace_test.go for the pattern.
func gitRepo(t *testing.T, files map[string]string) (dir, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "-b", "main")
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "--quiet", "-m", "seed")
	head = git("rev-parse", "HEAD")
	return dir, head
}

func TestSelectRunsAffectedAndAlwaysSteps(t *testing.T) {
	cfg := Config{Steps: []Step{
		{Name: "lint", Command: "lint", Always: true},
		{Name: "pkgA", Command: "test a", Paths: []string{"pkgA/**"}},
		{Name: "pkgB", Command: "test b", Paths: []string{"pkgB/**"}},
	}}

	got := Select(cfg, []string{"pkgA/file.go"})

	if len(got) != 2 {
		t.Fatalf("Select() = %d steps, want 2: %v", len(got), got)
	}
	if got[0].Name != "lint" || got[1].Name != "pkgA" {
		t.Fatalf("Select() = %v, want [lint pkgA] in that order", got)
	}
}

func TestLoadMissingFileIsErrNoValidation(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if !errors.Is(err, ErrNoValidation) {
		t.Fatalf("Load() err = %v, want ErrNoValidation", err)
	}
}

func TestLoadParsesConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	yml := `steps:
  - name: vet
    command: go vet ./...
    always: true
  - name: unit
    command: go test -json ./...
    paths: ["**/*.go", "go.mod"]
    timeout: 15m
    results: stdout
`
	if err := os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	if len(cfg.Steps) != 2 {
		t.Fatalf("Load() steps = %d, want 2", len(cfg.Steps))
	}
	if cfg.Steps[0].Timeout != DefaultTimeout {
		t.Fatalf("vet timeout = %v, want default %v", cfg.Steps[0].Timeout, DefaultTimeout)
	}
	if cfg.Steps[1].Timeout != 15*time.Minute {
		t.Fatalf("unit timeout = %v, want 15m", cfg.Steps[1].Timeout)
	}
	if cfg.Steps[1].Results != "stdout" {
		t.Fatalf("unit results = %q, want stdout", cfg.Steps[1].Results)
	}
}

// Load reads the platform's `suites:` — name, kind, command, paths,
// reports, jobs — and this node's own timeout, always, artifacts and
// results beside them, ignoring a key it does not know.
func TestLoadReadsSuitesWithKindPathsReportsJobsAndTheNodesKeys(t *testing.T) {
	dir := t.TempDir()
	yml := `version: 2
suites:
  - name: vet
    kind: lint
    command: go vet ./...
    always: true
  - name: e2e
    kind: integration
    command: " go test ./e2e -json "
    paths: [e2e/**, "platform/**", e2e/**]
    reports: [e2e.json, "  "]
    jobs: [shard (e2e 1/3), shard (e2e 2/3)]
    timeout: 20m
    artifacts: [cover.out]
    results: go-test-json:e2e.json
    flaky: retry
`
	writeFile(t, filepath.Join(dir, ".runsheet", "validate.yml"), yml)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Step{
		{Name: "vet", Kind: KindLint, Command: "go vet ./...", Paths: []string{}, Always: true, Timeout: DefaultTimeout},
		{Name: "e2e", Kind: KindIntegration, Command: "go test ./e2e -json", Paths: []string{"e2e/**", "platform/**"},
			Reports: []string{"e2e.json"}, Jobs: []string{"shard (e2e 1/3)", "shard (e2e 2/3)"},
			Timeout: 20 * time.Minute, Artifacts: []string{"cover.out"}, Results: "go-test-json:e2e.json"},
	}
	if !reflect.DeepEqual(cfg.Steps, want) {
		t.Fatalf("Load =\n %+v\nwant\n %+v", cfg.Steps, want)
	}
}

// A file the platform refuses is refused here too: an unknown kind, a
// repeated name, a path that is not a repo-relative glob.
func TestLoadRefusesWhatThePlatformRefuses(t *testing.T) {
	for name, yml := range map[string]string{
		"kind":     "suites:\n  - name: a\n    kind: smoke\n    command: x\n",
		"repeated": "suites:\n  - name: a\n    command: x\n  - name: a\n    command: y\n",
		"path":     "suites:\n  - name: a\n    command: x\n    paths: [../up]\n",
		"command":  "suites:\n  - name: a\n",
	} {
		if _, err := Parse([]byte(yml)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// A suite with no paths is affected by every change, as the platform's
// Affected has it — `always` or not; one with paths only by a match.
func TestSelectTakesASuiteWithNoPathsForAnyChange(t *testing.T) {
	cfg, err := Parse([]byte("suites:\n  - name: lint\n    kind: lint\n    command: go vet ./...\n  - name: e2e\n    kind: integration\n    command: go test ./e2e\n    paths: [e2e/**]\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]string{{"docs/readme.md"}, {"a/b/c.go"}, {".runsheet/validate.yml"}} {
		got := Select(cfg, changed)
		if len(got) != 1 || got[0].Name != "lint" {
			t.Errorf("Select(%v) = %v, want [lint]", changed, got)
		}
	}
	if got := Select(cfg, []string{"e2e/x_test.go"}); len(got) != 2 {
		t.Errorf("Select(e2e) = %v, want lint and e2e", got)
	}
}

// Nothing affected selects nothing — and that is a selection, not an
// error: the run attests it ran none.
func TestSelectCanBeEmpty(t *testing.T) {
	cfg := Config{Steps: []Step{{Name: "e2e", Command: "x", Paths: []string{"e2e/**"}}}}
	if got := Select(cfg, []string{"docs/x.md"}); len(got) != 0 {
		t.Fatalf("Select = %v, want none", got)
	}
}

// The reports of one run are cut at 256 KiB in all, not per step.
func TestRunCutsReportsAtTheCapInAll(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 200*1024)
	writeFile(t, filepath.Join(dir, "a.json"), big)
	writeFile(t, filepath.Join(dir, "b.json"), big)
	steps := []Step{
		{Name: "a", Command: "true", Reports: []string{"a.json"}, Timeout: time.Second},
		{Name: "b", Command: "true", Reports: []string{"b.json", "missing.json"}, Timeout: time.Second},
	}
	results, err := Run(context.Background(), steps, HostRunner{Dir: dir}, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(results[0].Report) != 200*1024 || len(results[1].Report) != 56*1024 {
		t.Fatalf("reports = %d, %d bytes, want %d and %d", len(results[0].Report), len(results[1].Report), 200*1024, 56*1024)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunStopsAtTheFirstFailingStepAndNamesIt(t *testing.T) {
	dir := t.TempDir()
	steps := []Step{
		{Name: "ok", Command: "true", Timeout: time.Second},
		{Name: "bad", Command: "exit 3", Timeout: time.Second},
		{Name: "never", Command: "true", Timeout: time.Second},
	}

	results, err := Run(context.Background(), steps, HostRunner{Dir: dir}, Options{Dir: dir})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Run() = %d results, want 2 (stopped at the failing step): %+v", len(results), results)
	}
	att := Attestation{Steps: results}
	step, ok := att.Failed()
	if !ok || step != "bad" {
		t.Fatalf("Failed() = %q, %v, want bad, true", step, ok)
	}
}

func TestRunTimesOutAStep(t *testing.T) {
	dir := t.TempDir()
	steps := []Step{
		{Name: "slow", Command: "sleep 5", Timeout: 200 * time.Millisecond},
	}

	start := time.Now()
	results, err := Run(context.Background(), steps, HostRunner{Dir: dir}, Options{Dir: dir})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("Run() took %v, the timeout did not kill the step", time.Since(start))
	}
	if len(results) != 1 {
		t.Fatalf("Run() = %d results, want 1", len(results))
	}
	if results[0].ExitCode != 124 || !results[0].TimedOut {
		t.Fatalf("slow step = exitCode %d timedOut %v, want 124 true", results[0].ExitCode, results[0].TimedOut)
	}
}

func TestParseGoTestJSONCountsRunPassedFailed(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "gotest.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	run, passed, failed, err := ParseGoTestJSON(f)
	if err != nil {
		t.Fatalf("ParseGoTestJSON() err = %v", err)
	}
	if run != 3 || passed != 1 || failed != 1 {
		t.Fatalf("ParseGoTestJSON() = run %d passed %d failed %d, want 3 1 1", run, passed, failed)
	}
}

func TestParseJUnitCountsRunPassedFailed(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "junit.xml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	run, passed, failed, err := ParseJUnit(f)
	if err != nil {
		t.Fatalf("ParseJUnit() err = %v", err)
	}
	if run != 4 || passed != 2 || failed != 1 {
		t.Fatalf("ParseJUnit() = run %d passed %d failed %d, want 4 2 1", run, passed, failed)
	}
}

// hashBase is a validate.yml in the platform's shape with this node's
// extra keys; the hash cases below change it one way at a time.
const hashBase = `# the suites
suites:
  - name: vet
    kind: lint
    command: go vet ./...
    always: true
    timeout: 5m
  - name: unit
    kind: unit
    command: go test -json ./...
    paths: ["**/*.go", go.mod]
    reports: [unit.json]
    jobs: [test]
    results: stdout
`

func mustHash(t *testing.T, content string) string {
	t.Helper()
	h, err := SuiteHash([]byte(content))
	if err != nil {
		t.Fatalf("SuiteHash(%q): %v", content, err)
	}
	return h
}

// SuiteHash reads what runs and when: comments, blank lines, key order,
// reports, jobs and this node's own keys leave it; name, kind, command,
// paths and the suites' order change it.
func TestSuiteHashIsTheCanonicalSuitesNotTheBytes(t *testing.T) {
	want := mustHash(t, hashBase)
	if len(want) != 64 || strings.ToLower(want) != want {
		t.Fatalf("hash %q is not lower-case hex sha256", want)
	}
	same := map[string]string{
		"comments and blank lines": "# a comment\n\n" + strings.ReplaceAll(hashBase, "  - name: unit\n", "\n  # unit tests\n  - name: unit # here\n"),
		"key order": `suites:
  - command: go vet ./...
    kind: lint
    name: vet
  - paths:
      - "**/*.go"
      - go.mod
    command: go test -json ./...
    name: unit
    kind: unit
`,
		"reports":        strings.Replace(hashBase, "reports: [unit.json]", "reports: [other.json, more.xml]", 1),
		"jobs":           strings.Replace(hashBase, "jobs: [test]", "jobs: [shard (e2e 1/3)]", 1),
		"timeout":        strings.Replace(hashBase, "timeout: 5m", "timeout: 50m", 1),
		"always":         strings.Replace(hashBase, "always: true", "always: false", 1),
		"results":        strings.Replace(hashBase, "results: stdout", "results: junit:out.xml", 1),
		"an unknown key": strings.Replace(hashBase, "suites:", "version: 2\nsuites:", 1),
	}
	for name, content := range same {
		if got := mustHash(t, content); got != want {
			t.Errorf("%s changed the hash: %s != %s", name, got, want)
		}
	}
	changed := map[string]string{
		"name":       strings.Replace(hashBase, "name: vet", "name: lint", 1),
		"kind":       strings.Replace(hashBase, "kind: lint", "kind: typecheck", 1),
		"command":    strings.Replace(hashBase, "go vet ./...", "go vet ./internal/...", 1),
		"paths":      strings.Replace(hashBase, `["**/*.go", go.mod]`, `["**/*.go"]`, 1),
		"path order": strings.Replace(hashBase, `["**/*.go", go.mod]`, `[go.mod, "**/*.go"]`, 1),
		"suite order": `suites:
  - name: unit
    kind: unit
    command: go test -json ./...
    paths: ["**/*.go", go.mod]
  - name: vet
    kind: lint
    command: go vet ./...
`,
	}
	for name, content := range changed {
		if got := mustHash(t, content); got == want {
			t.Errorf("a change to %s left the hash unchanged", name)
		}
	}
	if _, err := SuiteHash([]byte("suites: [\n")); err == nil {
		t.Error("an invalid file hashed")
	}
}

// The platform's own hash of runsheet/runsheet's validate.yml (at
// 1d08f03b6c84804ef46d3e9306424400057b03df), computed by its
// platform/validatespec.SuiteHash: the node's must be byte for byte the
// same, or every verify answers wrong_suite.
func TestSuiteHashIsThePlatformsOnRunsheetsFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "runsheet-validate.yml"))
	if err != nil {
		t.Fatal(err)
	}
	const platform = "84015ff30d2d0be7f7ab0e827a42303b747c8f201b964d4027eee810c1cf64e7"
	if got := mustHash(t, string(data)); got != platform {
		t.Fatalf("SuiteHash = %s, the platform's is %s", got, platform)
	}
}

func TestAttestationHasEveryField(t *testing.T) {
	dir, head := gitRepo(t, map[string]string{
		".runsheet/validate.yml": "steps:\n  - name: vet\n    command: go vet ./...\n    always: true\n",
		"artifact.bin":           "built output",
	})

	steps := []StepResult{{
		Name:        "vet",
		Command:     "go vet ./...",
		ExitCode:    0,
		DurationMs:  1500,
		TestsRun:    2,
		TestsPassed: 2,
		TestsFailed: 0,
		Artifacts:   []Artifact{{Path: "artifact.bin", SHA256: "abc123"}},
	}}
	meta := Meta{
		RunnerImage: "host:test-runner",
		Sandboxed:   false,
		NodeID:      "node-1",
		SessionID:   "session-1",
		ItemID:      "item-1",
	}

	att, err := Build(dir, head, steps, meta)
	if err != nil {
		t.Fatalf("Build() err = %v", err)
	}

	if att.Commit == "" {
		t.Error("commit is empty")
	}
	if att.Tree == "" {
		t.Error("tree is empty")
	}
	if att.RunnerImage == "" {
		t.Error("runnerImage is empty")
	}
	if att.Sandboxed {
		t.Error("sandboxed should be false")
	}
	if len(att.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(att.Steps))
	}
	s := att.Steps[0]
	if s.Name == "" || s.Command == "" {
		t.Errorf("step missing name/command: %+v", s)
	}
	if s.DurationMs == 0 {
		t.Error("step durationMs is zero")
	}
	if s.TestsRun == 0 {
		t.Error("step tests are zero")
	}
	if len(s.Artifacts) != 1 || s.Artifacts[0].SHA256 == "" {
		t.Errorf("step artifacts missing sha256: %+v", s.Artifacts)
	}
	if att.NodeID == "" || att.SessionID == "" || att.ItemID == "" {
		t.Errorf("nodeId/sessionId/itemId missing: %+v", att)
	}
	if att.At.IsZero() {
		t.Error("at is zero")
	}
	if want := mustHash(t, "steps:\n  - name: vet\n    command: go vet ./...\n"); att.Validation.Hash != want {
		t.Errorf("validation.hash = %q, want the file's SuiteHash %q", att.Validation.Hash, want)
	}
}

func TestMinutesByStepRoundsUp(t *testing.T) {
	att := Attestation{Steps: []StepResult{
		{Name: "instant", DurationMs: 0},
		{Name: "quick", DurationMs: 1},
		{Name: "exact", DurationMs: 120000},
		{Name: "over", DurationMs: 120001},
	}}

	got := att.MinutesByStep()
	want := map[string]int{"instant": 0, "quick": 1, "exact": 2, "over": 3}
	for name, minutes := range want {
		if got[name] != minutes {
			t.Errorf("MinutesByStep()[%s] = %d, want %d", name, got[name], minutes)
		}
	}
}

// This repository's own validate.yml is in the platform's shape: a
// `suites:` key and a kind on every suite, so the platform's Parse reads
// it and its hash is the one the node attests.
func TestThisRepositorysFileIsThePlatformsShape(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".runsheet", "validate.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\nsuites:\n") || strings.Contains(string(data), "\nsteps:") {
		t.Fatal("validate.yml has no top-level suites: key")
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Steps) == 0 {
		t.Fatal("no suites")
	}
	for _, s := range cfg.Steps {
		if !s.Kind.Valid() {
			t.Errorf("suite %s has kind %q", s.Name, s.Kind)
		}
	}
}
