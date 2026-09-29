package node

import "testing"

// TestKindModelsFor: a kind's own model wins, else the default, else the
// CLI's own (empty).
func TestKindModelsFor(t *testing.T) {
	var none Models
	for _, k := range []string{"build", "evaluate", "plan", "channel", "research", "rebase", "unknown"} {
		if got := none.For(k); got != "" {
			t.Fatalf("%s with nothing set: %q", k, got)
		}
	}
	m := Models{Default: "opus", Plan: "sonnet", Evaluate: "haiku", Rebase: "sonnet"}
	for _, c := range []struct{ kind, want string }{
		{"build", "opus"}, {"channel", "opus"}, {"unknown", "opus"},
		{"plan", "sonnet"}, {"evaluate", "haiku"}, {"rebase", "sonnet"}, {"research", "opus"},
	} {
		if got := m.For(c.kind); got != c.want {
			t.Fatalf("%s: got %q want %q", c.kind, got, c.want)
		}
	}
	if got := (Models{Plan: "sonnet"}).For("build"); got != "" {
		t.Fatalf("a kind with none and no default is the CLI's: %q", got)
	}
}

// TestKindModelRank: haiku < sonnet < opus, a full id by its family, and ""
// or anything unknown below all three.
func TestKindModelRank(t *testing.T) {
	order := []string{"", "haiku", "sonnet", "opus"}
	for i, m := range order {
		if got := ModelRank(m); got != i {
			t.Fatalf("ModelRank(%q) = %d, want %d", m, got, i)
		}
	}
	for m, want := range map[string]int{
		"claude-opus-5-5": 3, "claude-sonnet-5": 2, "claude-haiku-4-5-20251001": 1,
		"Opus": 3, "gpt-9": 0,
	} {
		if got := ModelRank(m); got != want {
			t.Fatalf("ModelRank(%q) = %d, want %d", m, got, want)
		}
	}
}

// TestStronger: the higher-ranked wins, a on a tie; Strongest is the
// same over a list.
func TestStronger(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{"haiku", "sonnet", "sonnet"}, {"opus", "sonnet", "opus"},
		{"", "haiku", "haiku"}, {"sonnet", "", "sonnet"},
		{"claude-sonnet-5", "sonnet", "claude-sonnet-5"}, {"sonnet", "claude-opus-5-5", "claude-opus-5-5"},
	} {
		if got := Stronger(c.a, c.b); got != c.want {
			t.Fatalf("Stronger(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
	if got := Strongest("haiku", "", "opus", "sonnet"); got != "opus" {
		t.Fatalf("Strongest = %q", got)
	}
	if got := Strongest(); got != "" {
		t.Fatalf("Strongest() = %q", got)
	}
	if got := Strongest("", "claude-haiku-4-5"); got != "claude-haiku-4-5" {
		t.Fatalf("Strongest over an empty = %q", got)
	}
}

// TestPlanModel: an explicit plan model wins; without one the planner
// runs on the strongest configured kind; with none at all, on opus.
func TestPlanModel(t *testing.T) {
	for _, c := range []struct {
		m    Models
		want string
	}{
		{Models{Plan: "haiku", Default: "opus"}, "haiku"},
		{Models{Default: "sonnet", Build: "opus"}, "opus"},
		{Models{Default: "haiku", Research: "claude-sonnet-5", Rebase: "opus"}, "claude-sonnet-5"},
		{Models{}, "opus"},
	} {
		if got := c.m.PlanModel(); got != c.want {
			t.Errorf("%+v.PlanModel() = %q, want %q", c.m, got, c.want)
		}
	}
}

func TestPlannableModel(t *testing.T) {
	for m, want := range map[string]bool{"haiku": true, "Sonnet": true, " opus ": true, "claude-opus-5-5": true, "claude-": false, "gpt-5": false, "fast": false, "": false} {
		if got := PlannableModel(m); got != want {
			t.Errorf("PlannableModel(%q) = %v", m, got)
		}
	}
}
