package validate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestSuiteHashChangesWhenAnAffectedFileChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	affected := filepath.Join(dir, "pkg", "a.go")
	other := filepath.Join(dir, "pkg", "b.txt")
	if err := os.WriteFile(affected, []byte("package pkg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("unaffected"), 0o644); err != nil {
		t.Fatal(err)
	}
	step := Step{Name: "unit", Command: "go test ./...", Paths: []string{"**/*.go"}}

	before, err := SuiteHash(dir, step)
	if err != nil {
		t.Fatalf("SuiteHash() err = %v", err)
	}

	if err := os.WriteFile(other, []byte("still unaffected, but changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	unaffected, err := SuiteHash(dir, step)
	if err != nil {
		t.Fatalf("SuiteHash() err = %v", err)
	}
	if unaffected != before {
		t.Fatalf("SuiteHash() changed after an unaffected file changed")
	}

	if err := os.WriteFile(affected, []byte("package pkg\n\nfunc x() {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := SuiteHash(dir, step)
	if err != nil {
		t.Fatalf("SuiteHash() err = %v", err)
	}
	if after == before {
		t.Fatalf("SuiteHash() did not change after an affected file changed")
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
		SuiteHash:   "deadbeef",
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
	if s.Name == "" || s.Command == "" || s.SuiteHash == "" {
		t.Errorf("step missing name/command/suiteHash: %+v", s)
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
	if att.Validation.Hash == "" {
		t.Error("validation.hash is empty")
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
