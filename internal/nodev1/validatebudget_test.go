package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeValidate(t *testing.T, dir, budget string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755)
	yml := "budget: " + budget + "\nsuites:\n  - name: unit\n    kind: unit\n    command: go test ./...\n"
	if err := os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// checkoutWithMain is a checkout whose origin/HEAD (main) states mainBudget
// and whose own head, a branch cut from it, states branchBudget.
func checkoutWithMain(t *testing.T, mainBudget, branchBudget string) string {
	t.Helper()
	ws := t.TempDir()
	gitT(t, ws, "init", "-q", "-b", "main")
	writeValidate(t, ws, mainBudget)
	gitT(t, ws, "add", ".")
	gitT(t, ws, "commit", "-qm", "main")
	gitT(t, ws, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitT(t, ws, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	gitT(t, ws, "checkout", "-q", "-b", "item")
	writeValidate(t, ws, branchBudget)
	gitT(t, ws, "commit", "-qam", "branch")
	return ws
}

// A branch cut before main tightened its budget is held to main's, as
// runsheet's stale branches were not to 25m after #7245; a branch may
// tighten its own budget but never loosen main's.
func TestAValidationIsHeldToTheSmallerOfTheBranchsAndMainsBudget(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, c := range []struct {
		main, branch string
		want         time.Duration
	}{
		{"25m", "80m", 25 * time.Minute},
		{"25m", "10m", 10 * time.Minute},
	} {
		ws := checkoutWithMain(t, c.main, c.branch)
		if got := validateBudget(ws); got != c.want {
			t.Fatalf("main %s, branch %s: held to %s, want %s", c.main, c.branch, got, c.want)
		}
		if got := (&Runtime{}).suiteBudget(&meshapi.Brief{Kind: "validate"}, ws); got != c.want {
			t.Fatalf("suiteBudget: main %s, branch %s: held to %s, want %s", c.main, c.branch, got, c.want)
		}
	}
	// no origin/HEAD: the checkout's own
	ws := t.TempDir()
	writeValidate(t, ws, "40m")
	if got := validateBudget(ws); got != 40*time.Minute {
		t.Fatalf("no origin: held to %s, want 40m", got)
	}
}

// run_suite's runs end a grace before the budget, so the session hears
// them out of time before the node stops it.
func TestSuiteRunsEndAGraceBeforeTheBudget(t *testing.T) {
	for budget, want := range map[time.Duration]time.Duration{
		25 * time.Minute: 23 * time.Minute,
		10 * time.Minute: 9 * time.Minute,
		0:                0,
	} {
		if got := suiteRunBudget(budget); got != want {
			t.Fatalf("budget %s: runs end at %s, want %s", budget, got, want)
		}
	}
}

// A budget a validation is held to is never extended (request_time).
func TestAFixedTimeBudgetIsNeverExtended(t *testing.T) {
	tb := newTimeBudget(context.Background(), time.Hour, nil)
	defer tb.Stop()
	tb.fixed = true
	if tb.Extend(time.Minute) {
		t.Fatal("a fixed budget was extended")
	}
}

// The node's report of a validation its budget stopped: red, triaged
// infra (never the builder's), "budget spent", each suite's latest
// outcome and every failing test run_suite named.
func TestTheBudgetFinishReportsWhatRunSuiteAnswered(t *testing.T) {
	a := &suiteAnswers{}
	a.add(`{"summary":"1 of 2 steps passed","steps":[{"name":"unit","exit":1,"tests_passed":5,"tests_failed":1,"duration_ms":1000},{"name":"lint","exit":0}],"failing_tests":["p.TestA"]}`)
	a.add(`{"summary":"rerun","steps":[{"name":"unit","exit":1,"tests_passed":5,"tests_failed":2}],"failing_tests":["p.TestA","p.TestB"]}`)
	a.add(`not json`)
	var f struct {
		Green    bool          `json:"green"`
		Evidence []string      `json:"evidence"`
		Suites   []budgetSuite `json:"suites"`
		Triage   struct {
			Class   string   `json:"class"`
			Note    string   `json:"note"`
			Failing []string `json:"failing_tests"`
		} `json:"triage"`
		Learnings []any `json:"learnings"`
	}
	if err := json.Unmarshal(a.budgetFinish(25*time.Minute, []string{"ev-1"}), &f); err != nil {
		t.Fatal(err)
	}
	if f.Green || f.Triage.Class != "infra" || !strings.HasPrefix(f.Triage.Note, "budget spent") || !strings.Contains(f.Triage.Note, "25m0s") {
		t.Fatalf("finish %+v", f)
	}
	if len(f.Evidence) != 1 || f.Learnings == nil {
		t.Fatalf("evidence %v learnings %v", f.Evidence, f.Learnings)
	}
	if len(f.Suites) != 2 || f.Suites[0].Name != "unit" || f.Suites[0].Failed != 2 || f.Suites[1].Name != "lint" {
		t.Fatalf("suites %+v", f.Suites)
	}
	if strings.Join(f.Triage.Failing, ",") != "p.TestA,p.TestB" {
		t.Fatalf("failing %v", f.Triage.Failing)
	}
	// nothing answered yet still reports, red infra
	if err := json.Unmarshal((&suiteAnswers{}).budgetFinish(time.Minute, nil), &f); err != nil || f.Triage.Class != "infra" || f.Evidence == nil {
		t.Fatalf("empty finish %+v, %v", f, err)
	}
}

// A validation's whole lease ends with its repository's budget: a
// session still going when it is spent is stopped, and the node reports
// the validation red, triaged infra, "budget spent", and releases —
// under the kind's far longer minutes.
func TestAValidationSessionIsStoppedAtTheBudgetAndReported(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g := newRig(t, sess)
	// main gains a validate.yml holding validations to 2s
	seed := t.TempDir()
	gitT(t, seed, "clone", "-q", g.github.bare, ".")
	writeValidate(t, seed, "2s")
	gitT(t, seed, "add", ".")
	gitT(t, seed, "commit", "-qm", "budget")
	gitT(t, seed, "push", "-q", "origin", "HEAD:main")
	head := gitT(t, seed, "rev-parse", "HEAD")
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Kind = "validate"
		b.Code.BaseSHA = head
		b.Harness.Budget.Minutes = 30
	})
	start := time.Now()
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("the lease took %s past a 2s budget", took)
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease ended %q; hand-backs %v fails %v", g.fake.Ended(lease), g.fake.Calls("hand-back"), g.fake.Calls("fail"))
	}
	var fin meshapi.FinishRequest
	json.Unmarshal(g.fake.Calls("finish")[0].Body, &fin)
	var r struct {
		Green  bool `json:"green"`
		Triage struct {
			Class string `json:"class"`
			Note  string `json:"note"`
		} `json:"triage"`
	}
	if err := json.Unmarshal(fin.Result, &r); err != nil {
		t.Fatalf("finish result %s: %v", fin.Result, err)
	}
	if r.Green || r.Triage.Class != "infra" || !strings.HasPrefix(r.Triage.Note, "budget spent") {
		t.Fatalf("finish result %s", fin.Result)
	}
}

// A lease that waits longer than its budget in admission still gets
// its full budget once admitted: the wait is the node's queue.
func TestBudgetLeftExcludesAdmissionWait(t *testing.T) {
	began := time.Date(2026, 11, 21, 11, 21, 0, 0, time.UTC)
	budget := 38 * time.Minute
	wait := 1*time.Hour + 53*time.Minute
	setup := 90 * time.Second
	admitted := began.Add(setup + wait)

	if got := budgetLeft(budget, began, wait, admitted); got != budget-setup {
		t.Fatalf("left at admission = %s, want %s (full budget less setup)", got, budget-setup)
	}
	if got := budgetBegan(began, wait); !got.Equal(began.Add(wait)) {
		t.Fatalf("budgetBegan = %s, want %s", got, began.Add(wait))
	}
	// without the wait excluded the lease would have only the floor
	if got := budgetLeft(budget, began, 0, admitted); got != budgetFloor {
		t.Fatalf("left with no wait recorded = %s, want the floor %s", got, budgetFloor)
	}
	// the rest of the whole-lease budget still holds: time after
	// admission counts, down to the floor
	if got := budgetLeft(budget, began, wait, admitted.Add(10*time.Minute)); got != budget-setup-10*time.Minute {
		t.Fatalf("left 10m after admission = %s", got)
	}
	if got := budgetLeft(budget, began, wait, admitted.Add(budget)); got != budgetFloor {
		t.Fatalf("left past the budget = %s, want the floor", got)
	}
}
