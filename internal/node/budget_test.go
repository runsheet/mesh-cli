package node

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A goal with $50 left (5000 cents, none spent) never binds: the kind's
// own budget for the model's tier wins.
func goalWith(cents int) *gateway.Goal {
	return &gateway.Goal{BudgetCents: cents}
}

func TestRunnerBudgetDefaultsByModelTier(t *testing.T) {
	runner := &Runner{Cfg: &Config{}}
	goal := goalWith(5000) // $50 left
	cases := []struct {
		model string
		want  float64
	}{
		{"sonnet", 4},
		{"haiku", 2},
		{"opus", 10},
	}
	for _, c := range cases {
		if b := runner.Budget(goal, "build", c.model); b != c.want {
			t.Fatalf("%s build budget = %v, want %v", c.model, b, c.want)
		}
	}
}

func TestRunnerBudgetGoalRemainderCapsBelowDefault(t *testing.T) {
	runner := &Runner{Cfg: &Config{}}
	goal := goalWith(150) // $1.50 left
	if b := runner.Budget(goal, "build", "sonnet"); b != 1.5 {
		t.Fatalf("budget = %v, want 1.5", b)
	}
}

func TestRunnerBudgetNodeYAMLOverridesDefault(t *testing.T) {
	runner := &Runner{Cfg: &Config{Budgets: map[string]map[string]float64{
		"build": {"sonnet": 6},
	}}}
	goal := goalWith(5000)
	if b := runner.Budget(goal, "build", "sonnet"); b != 6 {
		t.Fatalf("budget = %v, want 6", b)
	}
}

func TestRunnerBudgetFullModelIDResolvesToTier(t *testing.T) {
	runner := &Runner{Cfg: &Config{}}
	goal := goalWith(5000)
	if b := runner.Budget(goal, "build", "claude-sonnet-5"); b != 4 {
		t.Fatalf("budget = %v, want 4 (sonnet's default)", b)
	}
}

func TestBudgetModelTier(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"sonnet":          "sonnet",
		"haiku":           "haiku",
		"opus":            "opus",
		"claude-sonnet-5": "sonnet",
		"claude-opus-5-5": "opus",
		"gpt-6-astra":     "",
	}
	for in, want := range cases {
		if got := ModelTier(in); got != want {
			t.Fatalf("ModelTier(%q) = %q, want %q", in, got, want)
		}
	}
}

// A budget with no goal at all (a research session, which has none)
// falls back to the kind/model figure or the fallback: no crash on a
// nil goal.
func TestSessionBudgetNilGoal(t *testing.T) {
	kindBudget, kindSource := kindModelBudget(nil, "build", "sonnet")
	amount, source := SessionBudget(kindBudget, kindSource, nil, 0)
	if amount != 4 {
		t.Fatalf("amount = %v, want 4", amount)
	}
	if source == "" {
		t.Fatal("expected a non-empty source")
	}
}

// Live-reloaded budgets (Loop.KindModelBudget) win over Cfg's own once
// a reload has run, the way accounts and project budgets do.
func TestLoopKindModelBudgetIsLiveReloaded(t *testing.T) {
	l := &Loop{Cfg: &Config{Budgets: map[string]map[string]float64{"build": {"sonnet": 4}}}}
	if amount, _ := l.KindModelBudget("build", "sonnet"); amount != 4 {
		t.Fatalf("before reload: amount = %v, want 4 (Cfg's own)", amount)
	}
	l.mu.Lock()
	l.spending = Spending{Budgets: map[string]map[string]float64{"build": {"sonnet": 7}}}
	l.spendingSet = true
	l.mu.Unlock()
	if amount, _ := l.KindModelBudget("build", "sonnet"); amount != 7 {
		t.Fatalf("after reload: amount = %v, want 7 (live)", amount)
	}
}
