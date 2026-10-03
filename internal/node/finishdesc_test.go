package node

import "testing"

// FinishDescription's `## Who` section names the builder's actor, model
// and adapter, one `Paired with` line per pairer, and leaves the
// evaluator to the evaluation's own actor (every-actor-has-a-name item
// 3).
func TestFinishDescription(t *testing.T) {
	got := FinishDescription(FinishWho{
		Actor: "agent:builder@mini", Model: "opus", Adapter: "claude-code",
		Pairers: []Pairer{
			{Name: "Chris Kolenko", Email: "chris@example.com"},
			{Name: "Ann", Email: "ann@example.com"},
		},
	})
	want := "## Who\n\n" +
		"Built by agent:builder@mini on opus (claude-code)\n" +
		"Paired with Chris Kolenko <chris@example.com>\n" +
		"Paired with Ann <ann@example.com>\n" +
		"\n" +
		"Evaluated by: recorded by the evaluation's actor\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// With no pairer the section says so, rather than naming nobody.
func TestFinishDescriptionWithNoPairer(t *testing.T) {
	got := FinishDescription(FinishWho{Actor: "agent:builder@mini", Model: "sonnet", Adapter: "claude-code"})
	want := "## Who\n\n" +
		"Built by agent:builder@mini on sonnet (claude-code)\n" +
		"Paired with nobody\n" +
		"\n" +
		"Evaluated by: recorded by the evaluation's actor\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
