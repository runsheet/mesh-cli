package node

import "testing"

// TestModelsFor: a kind's own model wins, else the default, else the
// CLI's own (empty).
func TestModelsFor(t *testing.T) {
	var none Models
	for _, k := range []string{"build", "evaluate", "plan", "channel", "unknown"} {
		if got := none.For(k); got != "" {
			t.Fatalf("%s with nothing set: %q", k, got)
		}
	}
	m := Models{Default: "opus", Plan: "sonnet", Evaluate: "haiku"}
	for _, c := range []struct{ kind, want string }{
		{"build", "opus"}, {"channel", "opus"}, {"unknown", "opus"},
		{"plan", "sonnet"}, {"evaluate", "haiku"},
	} {
		if got := m.For(c.kind); got != c.want {
			t.Fatalf("%s: got %q want %q", c.kind, got, c.want)
		}
	}
	if got := (Models{Plan: "sonnet"}).For("build"); got != "" {
		t.Fatalf("a kind with none and no default is the CLI's: %q", got)
	}
}

// TestModelRank: haiku < sonnet < opus, a full id by its family, and ""
// or anything unknown below all three.
func TestModelRank(t *testing.T) {
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
