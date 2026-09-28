package harness

import (
	"strings"
	"testing"
)

// A full sample: one initiative-scoped pitch, one goal-scoped pitch,
// and two answers to earlier open questions. Every field round-trips.
func TestParsePitchesFullSample(t *testing.T) {
	text := "I read the pitch_feedback and the code.\n\n```json\n" +
		`{"pitches": [` +
		`{"slug": "fewer-conflicts", "title": "Fewer conflicts", "problem": "Rebases fail often.", ` +
		`"evidence": [{"kind": "metric", "ref": "scorecard:conflictReworksPer100Merges", "note": "up 40% this month"}, ` +
		`{"kind": "research_note", "ref": "specs/notes/rebases.md", "note": "three incidents traced to one pattern"}], ` +
		`"bet": "fewer-reworks", ` +
		`"scope": {"kind": "initiative", "goals": [` +
		`{"slug": "rebase-runbook", "title": "A rebase runbook", "statement": "Rebases follow a runbook.", "acceptance": "the skill exists and is used"}, ` +
		`{"slug": "rebase-metrics", "title": "Rebase metrics", "statement": "Rebases are measured.", "acceptance": "the scorecard shows the metric", "budget_cents": 50000}` +
		`]}, ` +
		`"estimate": {"cost_cents": 900000, "days": 6, "basis": "goal_history median for similar goals"}, ` +
		`"risks": ["the runbook is ignored"], ` +
		`"alternatives": ["do nothing and hope"], ` +
		`"signal": {"metric": "conflictReworksPer100Merges", "how_measured": "scorecard window", "direction": "down", "window_days": 30}}, ` +
		`{"slug": "faster-onboarding", "title": "Faster onboarding", "problem": "New nodes take too long to set up.", ` +
		`"evidence": [{"kind": "user_signal", "ref": "https://example.com/feedback/1", "note": "an operator said so"}], ` +
		`"bet": "", ` +
		`"scope": {"kind": "goal", "goals": [{"slug": "onboarding-doc", "title": "An onboarding doc", "statement": "A new node sets up in ten minutes.", "acceptance": "README section exists"}]}, ` +
		`"estimate": {"cost_cents": 200000, "days": 2, "basis": "similar docs items"}, ` +
		`"risks": [], "alternatives": [], ` +
		`"signal": {"metric": "merged", "how_measured": "count", "direction": "up", "window_days": 14}}` +
		`], "answers": [{"pitch": "fewer-conflicts", "text": "Yes, scoped to the mini only."}, {"pitch": "faster-onboarding", "text": "No, the doc stands alone."}]}` +
		"\n```\n"
	out, err := ParsePitches(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Pitches) != 2 {
		t.Fatalf("pitches: %+v", out.Pitches)
	}
	init, goal := out.Pitches[0], out.Pitches[1]
	if init.Slug != "fewer-conflicts" || init.Title != "Fewer conflicts" || init.Problem != "Rebases fail often." {
		t.Fatalf("initiative pitch: %+v", init)
	}
	if len(init.Evidence) != 2 || init.Evidence[0].Kind != "metric" || init.Evidence[0].Ref != "scorecard:conflictReworksPer100Merges" {
		t.Fatalf("evidence: %+v", init.Evidence)
	}
	if init.Bet != "fewer-reworks" {
		t.Fatalf("bet: %q", init.Bet)
	}
	if init.Scope.Kind != "initiative" || len(init.Scope.Goals) != 2 || init.Scope.Goals[1].BudgetCents != 50000 {
		t.Fatalf("scope: %+v", init.Scope)
	}
	if init.Estimate.CostCents != 900000 || init.Estimate.Days != 6 || init.Estimate.Basis == "" {
		t.Fatalf("estimate: %+v", init.Estimate)
	}
	if len(init.Risks) != 1 || len(init.Alternatives) != 1 {
		t.Fatalf("risks/alternatives: %+v %+v", init.Risks, init.Alternatives)
	}
	if init.Signal.Metric != "conflictReworksPer100Merges" || init.Signal.Direction != "down" || init.Signal.WindowDays != 30 {
		t.Fatalf("signal: %+v", init.Signal)
	}
	if goal.Scope.Kind != "goal" || len(goal.Scope.Goals) != 1 || goal.Scope.Goals[0].Slug != "onboarding-doc" {
		t.Fatalf("goal scope: %+v", goal.Scope)
	}
	if goal.Bet != "" {
		t.Fatalf("goal pitch's bet: %q", goal.Bet)
	}
	if len(out.Answers) != 2 || out.Answers[0].Pitch != "fewer-conflicts" || out.Answers[1].Text != "No, the doc stands alone." {
		t.Fatalf("answers: %+v", out.Answers)
	}
}

// The pitches survive a pitch whose own prose mentions a fence, the
// same trap ParseProposals and ParsePlan guard against.
func TestParsePitchesNestedFence(t *testing.T) {
	text := "I read the code.\n\n```json\n" +
		`{"pitches": [{"slug": "a", "title": "A", "problem": "shown with ` + "```sh\\nmesh login\\n```" + `", ` +
		`"evidence": [{"kind": "metric", "ref": "scorecard:merged", "note": "n"}], "bet": "", ` +
		`"scope": {"kind": "goal", "goals": [{"slug": "g", "title": "G", "statement": "s", "acceptance": "a"}]}, ` +
		`"estimate": {"cost_cents": 1, "days": 1, "basis": "b"}, "risks": [], "alternatives": [], ` +
		`"signal": {"metric": "merged", "how_measured": "m", "direction": "up", "window_days": 14}}], "answers": []}` +
		"\n```\n"
	out, err := ParsePitches(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Pitches) != 1 || out.Pitches[0].Slug != "a" {
		t.Fatalf("pitches: %+v", out.Pitches)
	}
}

// An empty list is a valid answer: nothing worth pitching is not a
// parse failure.
func TestParsePitchesEmptyIsAnAnswer(t *testing.T) {
	out, err := ParsePitches("Nothing is worth pitching tonight.\n\n```json\n{\"pitches\": []}\n```")
	if err != nil {
		t.Fatalf("an empty list is an answer: %v", err)
	}
	if len(out.Pitches) != 0 || len(out.Answers) != 0 {
		t.Fatalf("out: %+v", out)
	}
}

func pitchJSON(fields string) string {
	return "```json\n{\"pitches\": [{" + fields + "}]}\n```"
}

func TestParsePitchesRefusals(t *testing.T) {
	validScope := `"scope": {"kind": "goal", "goals": [{"slug": "g", "title": "G", "statement": "s", "acceptance": "a"}]}`
	base := `"slug": "s", "title": "T", "problem": "p", "evidence": [{"kind": "metric", "ref": "r", "note": "n"}], "bet": "", ` +
		validScope + `, "estimate": {"cost_cents": 1, "days": 1, "basis": "b"}, "risks": [], "alternatives": [], ` +
		`"signal": {"metric": "merged", "how_measured": "m", "direction": "up", "window_days": 14}`
	withField := func(remove, replacement string) string {
		return strings.Replace(base, remove, replacement, 1)
	}
	for what, text := range map[string]string{
		"no json at all": "I had a think and decided not to say.",
		"no problem":     pitchJSON(withField(`"problem": "p", `, "")),
		"goal scope with two goals": pitchJSON(withField(
			validScope,
			`"scope": {"kind": "goal", "goals": [{"slug": "g1", "title": "G1", "statement": "s", "acceptance": "a"}, {"slug": "g2", "title": "G2", "statement": "s", "acceptance": "a"}]}`,
		)),
		"scope with no goal": pitchJSON(withField(
			validScope,
			`"scope": {"kind": "initiative", "goals": []}`,
		)),
		"evidence without ref": pitchJSON(withField(
			`"evidence": [{"kind": "metric", "ref": "r", "note": "n"}], `,
			`"evidence": [{"kind": "metric", "ref": "", "note": "n"}], `,
		)),
	} {
		if _, err := ParsePitches(text); err == nil {
			t.Fatalf("%s should be refused", what)
		}
	}
}

// The same slug twice in one session is one pitch: the first wins.
func TestParsePitchesDuplicateSlugCollapsed(t *testing.T) {
	one := `{"slug": "a", "title": "First", "problem": "p", "evidence": [{"kind": "metric", "ref": "r", "note": "n"}], "bet": "", ` +
		`"scope": {"kind": "goal", "goals": [{"slug": "g", "title": "G", "statement": "s", "acceptance": "a"}]}, ` +
		`"estimate": {"cost_cents": 1, "days": 1, "basis": "b"}, "risks": [], "alternatives": [], ` +
		`"signal": {"metric": "merged", "how_measured": "m", "direction": "up", "window_days": 14}}`
	two := strings.Replace(one, `"title": "First"`, `"title": "Second"`, 1)
	text := "```json\n{\"pitches\": [" + one + ", " + two + "]}\n```"
	out, err := ParsePitches(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Pitches) != 1 || out.Pitches[0].Title != "First" {
		t.Fatalf("pitches: %+v", out.Pitches)
	}
}
