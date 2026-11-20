package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

// A validation is held to its repository's budget (validate.yml
// budget:, ADR-049) for its whole lease: the suites, their reruns and
// the session's diagnosis of them. Before 2026-11-20 only run_suite
// was: the session went on under the kind's own minutes, and on dl360
// runsheet validations held to 25m took a median 46 minutes, the
// session diagnosing red suites long after the budget was spent.
//
// The budget is the smaller of the checkout's and the repository's
// default branch's (origin/HEAD as the checkout's clone found it): a
// branch cut before main tightened its budget would otherwise hold the
// lease to the old, longer one, as every stale runsheet branch held
// leases to 80m after main said 25m (#7245). A branch may tighten its
// own budget, never loosen main's.
//
// run_suite's runs end suiteGrace before the budget, so the session
// hears their out-of-time answers and can report them itself; at the
// budget the node stops the session and reports the validation with
// what run_suite answered (budgetFinish): red, triaged infra, "budget
// spent" — the runsheet gateway fails a red triaged infra classed
// infrastructure, never on the builder.

// suiteGrace is how long before the lease's budget run_suite's runs
// stop: a tenth of the budget, at most two minutes.
func suiteGrace(budget time.Duration) time.Duration {
	return min(budget/10, 2*time.Minute)
}

// budgetFloor is the least time a session held to a budget already
// spent (a long admission wait) is given to report what it has, never
// more than the budget itself.
const budgetFloor = time.Minute

// validateBudget is the lease's validation budget, as validateBudget
// describes: zero when neither the checkout nor its default branch
// states one.
func validateBudget(workspace string) time.Duration {
	var budget time.Duration
	if cfg, err := validate.Load(workspace); err == nil {
		budget = cfg.Budget
	}
	if main := defaultBranchBudget(workspace); main > 0 && (budget <= 0 || main < budget) {
		budget = main
	}
	return budget
}

// defaultBranchBudget is the budget origin/HEAD's validate.yml states,
// zero when the checkout has no origin/HEAD or the file states none.
func defaultBranchBudget(workspace string) time.Duration {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := leaseGit(ctx, workspace, "show", "refs/remotes/origin/HEAD:.runsheet/validate.yml").Output()
	if err != nil {
		return 0
	}
	cfg, err := validate.Parse(out)
	if err != nil {
		return 0
	}
	return cfg.Budget
}

// suiteAnswers keeps what run_suite answered over the lease, each
// suite's latest outcome, for the report the node makes when the
// budget stops the session.
type suiteAnswers struct {
	mu        sync.Mutex
	order     []string
	suites    map[string]budgetSuite
	failing   []string
	summaries []string
}

type budgetSuite struct {
	Name       string `json:"name"`
	Exit       int    `json:"exit"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	DurationMs int64  `json:"duration_ms"`
}

// addFunc is add, nil on a nil suiteAnswers.
func (a *suiteAnswers) addFunc() func(string) {
	if a == nil {
		return nil
	}
	return a.add
}

// add takes one run's answer (nodetools.RunSuite.OnFinished).
func (a *suiteAnswers) add(answer string) {
	var ans struct {
		Summary string `json:"summary"`
		Steps   []struct {
			Name        string `json:"name"`
			Exit        int    `json:"exit"`
			DurationMs  int64  `json:"duration_ms"`
			TestsPassed int    `json:"tests_passed"`
			TestsFailed int    `json:"tests_failed"`
		} `json:"steps"`
		Failing []string `json:"failing_tests"`
	}
	if json.Unmarshal([]byte(answer), &ans) != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.suites == nil {
		a.suites = map[string]budgetSuite{}
	}
	for _, st := range ans.Steps {
		if _, ok := a.suites[st.Name]; !ok {
			a.order = append(a.order, st.Name)
		}
		a.suites[st.Name] = budgetSuite{Name: st.Name, Exit: st.Exit, Passed: st.TestsPassed, Failed: st.TestsFailed, DurationMs: st.DurationMs}
	}
	for _, t := range ans.Failing {
		if !slices.Contains(a.failing, t) {
			a.failing = append(a.failing, t)
		}
	}
	if s := strings.TrimSpace(ans.Summary); s != "" {
		a.summaries = append(a.summaries, s)
	}
}

// budgetFinish is the validate finish the node reports for a session
// its budget stopped: red, triaged infra, the reason "budget spent",
// each suite's latest outcome and the failures run_suite found.
func (a *suiteAnswers) budgetFinish(budget time.Duration, evidence []string) json.RawMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	suites := make([]budgetSuite, 0, len(a.order))
	for _, n := range a.order {
		suites = append(suites, a.suites[n])
	}
	note := fmt.Sprintf("budget spent: the lease reached the repository's validation budget of %s (validate.yml budget:) and the node stopped the session before it reported", budget)
	if len(a.summaries) == 0 {
		note += "; no suite run had ended"
	} else {
		note += "; run_suite answered: " + clip(strings.Join(a.summaries, " | "), 4000)
	}
	if evidence == nil {
		evidence = []string{}
	}
	failing := a.failing
	if failing == nil {
		failing = []string{}
	}
	out, _ := json.Marshal(map[string]any{
		"green":     false,
		"evidence":  evidence,
		"suites":    suites,
		"triage":    map[string]any{"class": "infra", "note": note, "failing_tests": failing},
		"learnings": []any{},
	})
	return out
}
