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
