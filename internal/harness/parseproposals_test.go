package harness

import (
	"strings"
	"testing"
)

// The director's proposals survive a proposal whose own prose mentions
// a fence — the same trap that cost three plans on 2026-09-22, in the
// other parser.
func TestParseProposalsNestedFence(t *testing.T) {
	text := "I read the specs.\n\n```json\n{\"goals\": [{\"slug\": \"one-sign-in\", \"title\": \"One sign-in\", " +
		"\"statement\": \"Everyone signs in once.\", \"acceptance\": \"the README shows ```sh\\nmesh login\\n``` and it works\", " +
		"\"why\": \"the front door\"}]}\n```\n"
	goals, err := ParseProposals(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(goals) != 1 || goals[0].Slug != "one-sign-in" {
		t.Fatalf("goals: %+v", goals)
	}
}

// A session that decides nothing is worth doing says so, and that is an
// answer rather than a failure.
func TestParseProposalsEmptyIsAnAnswer(t *testing.T) {
	goals, err := ParseProposals("Nothing is worth doing tonight; the backlog is clear.\n\n```json\n{\"goals\": []}\n```")
	if err != nil {
		t.Fatalf("an empty list is an answer: %v", err)
	}
	if len(goals) != 0 {
		t.Fatalf("goals: %+v", goals)
	}
}

func TestParseProposalsRefusals(t *testing.T) {
	for what, text := range map[string]string{
		"no json at all":   "I had a think and decided not to say.",
		"no slug":          "```json\n{\"goals\": [{\"title\": \"T\", \"statement\": \"s\", \"acceptance\": \"a\"}]}\n```",
		"no acceptance":    "```json\n{\"goals\": [{\"slug\": \"s\", \"title\": \"T\", \"statement\": \"s\"}]}\n```",
		"no statement":     "```json\n{\"goals\": [{\"slug\": \"s\", \"title\": \"T\", \"acceptance\": \"a\"}]}\n```",
		"prose after json": "I could not decide.",
	} {
		if _, err := ParseProposals(text); err == nil {
			t.Fatalf("%s should be refused", what)
		}
	}
}

// A bare list is read as well as the wrapped object, and the same slug
// twice in one session is one goal.
func TestParseProposalsBareAndDeduplicated(t *testing.T) {
	goals, err := ParseProposals("```json\n[" +
		"{\"slug\": \"a\", \"title\": \"A\", \"statement\": \"s\", \"acceptance\": \"a\"}," +
		"{\"slug\": \"a\", \"title\": \"A again\", \"statement\": \"s\", \"acceptance\": \"a\"}," +
		"{\"slug\": \"b\", \"title\": \"B\", \"statement\": \"s\", \"acceptance\": \"a\"}]\n```")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(goals) != 2 || goals[0].Slug != "a" || goals[1].Slug != "b" {
		t.Fatalf("goals: %+v", goals)
	}
}

// The prompt's contract: the direction, the room, the order and the
// JSON block. These lines are load-bearing — a session reads them and
// the platform reads what comes back.
func TestResearchPromptContract(t *testing.T) {
	p := ResearchPrompt(ResearchInput{
		Org: "acme", Project: "mesh", Direction: "Mesh is the place goals turn into merged pull requests.",
		Repos: map[string]string{"acme/widgets": "repos/widgets"}, ContextDir: ".mesh/context",
		ContextFiles: []string{"INDEX.md", "prd.md"}, Room: 2, DefaultBudgetCents: 4000,
		Goals: []ExistingGoal{{Slug: "ship-it", Title: "Ship it", Status: "running"}},
	})
	for _, want := range []string{
		"You are the director of acme, project mesh",
		"Mesh is the place goals turn into merged pull requests.",
		"`ship-it` — Ship it — running",
		"`acme/widgets` in `repos/widgets/`",
		".mesh/context", "INDEX.md, prd.md",
		"at most 2 goals", "room for 2 right now",
		"$40.00",
		"you research and propose, you do not build",
		"```json\n{\"goals\": [{\"slug\"",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	// with no direction there is nothing to go on, and the session is
	// told to say so rather than invent one
	none := ResearchPrompt(ResearchInput{Org: "acme", Project: "mesh", Room: 1})
	if !strings.Contains(none, "Propose nothing and say so.") {
		t.Fatalf("no direction:\n%s", none)
	}
	// and a full project is told there is no room
	full := ResearchPrompt(ResearchInput{Org: "acme", Project: "mesh", Direction: "d", Room: 0})
	if !strings.Contains(full, "no room for another goal right now") {
		t.Fatalf("no room:\n%s", full)
	}
	one := ResearchPrompt(ResearchInput{Org: "acme", Project: "mesh", Direction: "d", Room: 1})
	if !strings.Contains(one, "at most one goal") {
		t.Fatalf("room for one:\n%s", one)
	}
}

// A direction that proposes rather than sets (runsheet ADR 022) is told
// the room as information: a full project still hears the figure, and
// is not told to propose nothing. Its output is pitches (runsheet ADR
// 026, trd §6c Pitches), not bare goal proposals: every field, the
// mesh_projects read of pitch_feedback and the pitches/answers
// contract. A set direction keeps the cap and the goals contract.
func TestResearchPromptProposeMode(t *testing.T) {
	p := ResearchPrompt(ResearchInput{
		Org: "acme", Project: "mesh", Direction: "d", Room: 0, Proposes: true,
		Bets: []string{"faster-onboarding", "fewer-reworks"},
	})
	for _, want := range []string{
		"This direction proposes rather than sets",
		"the room is information, not a cap",
		"The project has room for 0 right now.",
		"a proposal that would wait for room should say so in its why",
		"mesh_projects",
		"pitch_feedback",
		"open_questions",
		"declined",
		"`bets`",
		"goal_history",
		"`faster-onboarding`",
		"evidence",
		"bet",
		"scope",
		"estimate",
		"risks",
		"alternatives",
		"signal",
		"how_measured",
		`{"pitches": [{"slug"`,
		`"answers": [{"pitch"`,
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Propose nothing") {
		t.Fatalf("a propose direction is not capped by the room:\n%s", p)
	}
	if strings.Contains(p, `{"goals"`) {
		t.Fatalf("a propose direction's contract is pitches, not goals:\n%s", p)
	}
	set := ResearchPrompt(ResearchInput{Org: "acme", Project: "mesh", Direction: "d", Room: 0})
	if !strings.Contains(set, "Propose nothing, and say in your notes what you would have proposed.") || strings.Contains(set, "proposes rather than sets") {
		t.Fatalf("a set direction at its ceiling:\n%s", set)
	}
	if !strings.Contains(set, `{"goals": [{"slug"`) {
		t.Fatalf("a set direction keeps the goals contract:\n%s", set)
	}
	if strings.Contains(set, `"pitches"`) {
		t.Fatalf("a set direction does not pitch:\n%s", set)
	}
}
