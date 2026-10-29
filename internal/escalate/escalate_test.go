package escalate

import (
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A ladder starts on the default, steps up the kind's escalate list on
// the triggers its model.on names, never past max, and keeps each step.
func TestLadder(t *testing.T) {
	cases := []struct {
		name   string
		policy meshapi.ModelPolicy
		ons    []string
		want   []string // the model after each trigger; "!" + reason when refused
	}{
		{"climbs to the ceiling", meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet", "opus"}, On: []string{OnRed, OnStall}, Max: "opus"},
			[]string{OnRed, OnStall, OnRed}, []string{"sonnet", "opus", "!ceiling"}},
		{"max caps the ladder", meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet", "opus"}, On: []string{OnRed}, Max: "sonnet"},
			[]string{OnRed, OnRed}, []string{"sonnet", "!ceiling"}},
		{"no max climbs the whole ladder", meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet", "opus"}, On: []string{OnAsked}},
			[]string{OnAsked, OnAsked, OnAsked}, []string{"sonnet", "opus", "!no model is left"}},
		{"a trigger the kind does not name", meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet"}, On: []string{OnRed}},
			[]string{OnConflict, OnRed}, []string{"!does not escalate on conflict", "sonnet"}},
		{"a default on the ladder starts past it", meshapi.ModelPolicy{Default: "sonnet", Escalate: []string{"sonnet", "opus"}, On: []string{OnRed}},
			[]string{OnRed}, []string{"opus"}},
		{"no ladder", meshapi.ModelPolicy{On: []string{OnRed}}, []string{OnRed}, []string{"!no model is left above the CLI's default"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := New(c.policy)
			taken := 0
			for i, on := range c.ons {
				peek, perr := l.Peek(on)
				step, err := l.Step(on, "why")
				if want, refused := strings.CutPrefix(c.want[i], "!"); refused {
					if err == nil || !strings.Contains(err.Error(), want) || perr == nil {
						t.Fatalf("%s: want a refusal %q, got %+v %v", on, want, step, err)
					}
					continue
				}
				if err != nil || step.To != c.want[i] || peek != step.To || l.Model() != step.To || step.On != on || step.At.IsZero() {
					t.Fatalf("%s: %+v %v (peek %q)", on, step, err, peek)
				}
				taken++
			}
			if len(l.Steps()) != taken {
				t.Fatalf("steps %+v", l.Steps())
			}
		})
	}
}

// A step the session refused is marked failed and the lease stays on the
// model it came from.
func TestFailedStep(t *testing.T) {
	l := New(meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet"}, On: []string{OnRed}})
	l.Step(OnRed, "red")
	l.Failed("sonnet", "refused")
	if l.Model() != "haiku" || l.Steps()[0].Failed != "refused" {
		t.Fatalf("%q %+v", l.Model(), l.Steps())
	}
}
