package gateway_test

import (
	"context"
	"errors"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const (
	orgID   = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	projID  = "cccccccc-3333-4333-8333-cccccccccccc"
	otherID = "cccccccc-4444-4444-8444-cccccccccccc"
	nodeID  = "22222222-2222-4222-8222-222222222222"
)

func directionRow(id, projectID, slug string) map[string]any {
	return map[string]any{
		"id": id, "projectId": projectID, "orgId": orgID, "slug": slug, "name": slug,
		"direction": "Go on then.", "enabled": true, "fromMinute": 1320, "toMinute": 420,
		"timezone": "Australia/Brisbane", "gapMinutes": 45, "maxLiveGoals": 2, "status": "active",
	}
}

// The node lists a project's directions — org-scoped as the gateway
// requires, narrowed on the project — and every field it needs to
// decide whether to offer one comes back on the row.
func TestDirections(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Directions["d1"] = directionRow("d1", projID, "overnight")
	f.Directions["d2"] = directionRow("d2", projID, "mornings")
	f.Directions["d3"] = directionRow("d3", otherID, "elsewhere")
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	all, err := gw.Directions(ctx, orgID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("the org's directions: %+v", all)
	}
	mine, err := gw.Directions(ctx, orgID, projID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Fatalf("the project's directions: %+v", mine)
	}
	var d gateway.Direction
	for _, row := range mine {
		if row.Slug == "overnight" {
			d = row
		}
		if row.ProjectID != projID {
			t.Fatalf("narrowed on the project: %+v", row)
		}
	}
	if d.ID != "d1" || d.OrgID != orgID || d.Direction != "Go on then." || !d.Enabled {
		t.Fatalf("the words and the switch: %+v", d)
	}
	if d.FromMinute != 1320 || d.ToMinute != 420 || d.Timezone != "Australia/Brisbane" {
		t.Fatalf("the window, in the direction's own zone: %+v", d)
	}
	if d.GapMinutes != 45 || d.MaxLiveGoals != 2 || d.Status != "active" {
		t.Fatalf("the pace, the share and the status: %+v", d)
	}
	if !d.Active() {
		t.Fatal("an enabled, unarchived direction is one to offer")
	}
	d.Status = "archived"
	if d.Active() {
		t.Fatal("an archived direction is not offered at all")
	}
}

// The claim and the hand-in are against the direction's own aggregate,
// and the platform's "not now" is a refusal — the node's ordinary
// answer, not an error to shout about.
func TestDirectionResearchClaimAndRecord(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Directions["d1"] = directionRow("d1", projID, "overnight")
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	if err := gw.ClaimDirectionResearch(ctx, "d1", nodeID); err != nil {
		t.Fatal(err)
	}
	claim := f.CallsNamed("claimDirectionResearch")
	if len(claim) != 1 || claim[0].Input["aggregateId"] != "d1" || claim[0].Input["nodeId"] != nodeID {
		t.Fatalf("claimed against the direction: %+v", claim)
	}
	if at, _ := claim[0].Input["at"].(string); at == "" {
		t.Fatal("a claim carries the time it is made: the window is judged on it")
	}

	proposals := []gateway.GoalProposal{
		{Slug: "one-sign-in", Title: "One sign-in", Statement: "Everyone signs in once.",
			Acceptance: "a person signs in once", Why: "the direction asks for it", BudgetCents: 3000},
	}
	if err := gw.RecordDirectionResearch(ctx, "d1", nodeID, gateway.Research{Proposals: proposals}, 125, "1 proposed", nil); err != nil {
		t.Fatal(err)
	}
	rec := f.CallsNamed("recordDirectionResearch")
	if len(rec) != 1 || rec[0].Input["aggregateId"] != "d1" || rec[0].Input["costCents"] != float64(125) {
		t.Fatalf("handed in against the direction, with the cost: %+v", rec)
	}
	list, _ := rec[0].Input["proposals"].([]any)
	if len(list) != 1 {
		t.Fatalf("proposals: %+v", list)
	}
	first, _ := list[0].(map[string]any)
	if first["slug"] != "one-sign-in" || first["budgetCents"] != float64(3000) {
		t.Fatalf("proposal: %+v", first)
	}
	if _, ok := rec[0].Input["pitches"]; ok {
		t.Fatalf("a proposals-only hand-in carries no pitches key: %+v", rec[0].Input)
	}
	if _, ok := rec[0].Input["answers"]; ok {
		t.Fatalf("a proposals-only hand-in carries no answers key: %+v", rec[0].Input)
	}
	if held, _ := f.Directions["d1"]["researchNodeId"].(string); held != "" {
		t.Fatalf("the hand-in lets the claim go: %q", held)
	}

	// outside the window, inside the gap, or held elsewhere: all the
	// same shape, and all IsRefusal
	f.Refuse["claimDirectionResearch"] = "the direction is outside its window"
	err := gw.ClaimDirectionResearch(ctx, "d1", nodeID)
	if err == nil || !gateway.IsRefusal(err) {
		t.Fatalf("a refused claim is a refusal, not a failure: %v", err)
	}
}

// pitchesInput is one propose-mode hand-in: two pitches — one with an
// estimate and a signal, one with neither and no risks named — and one
// answer to an earlier pitch's open question.
func pitchesInput() gateway.Research {
	return gateway.Research{
		Pitches: []gateway.PitchDraft{
			{
				Slug: "trim-onboarding", Title: "Trim onboarding to one screen",
				Problem: "New orgs drop off before their first goal.",
				Evidence: []gateway.PitchEvidence{
					{Kind: gateway.PitchEvidenceMetric, Ref: "activation-dashboard", Note: "40% never reach a first goal"},
				},
				Bet: "fewer-steps",
				Scope: gateway.PitchScope{Kind: gateway.PitchScopeGoal, Goals: []gateway.PitchGoal{
					{Slug: "one-screen-onboarding", Title: "One-screen onboarding",
						Statement:  "A new org reaches its first goal from one screen.",
						Acceptance: "a new org creates a goal without leaving the first screen", BudgetCents: 5000},
				}},
				Estimate:     &gateway.PitchEstimate{CostCents: 4000, Days: 3, Basis: "similar onboarding change last quarter"},
				Risks:        []string{"could hide advanced options people rely on"},
				Alternatives: []string{"a guided tour instead of fewer steps"},
				Signal: &gateway.PitchSignal{Metric: "activation rate", HowMeasured: "orgs with a goal within a day / new orgs",
					Direction: gateway.PitchSignalUp, WindowDays: 14},
			},
			{
				Slug: "scout-two", Title: "A second scout pitch",
				Problem: "No evidence yet, a placeholder pitch.",
				Evidence: []gateway.PitchEvidence{
					{Kind: gateway.PitchEvidenceResearchNote, Ref: "scout-notes", Note: "worth watching"},
				},
				Bet: "watch-and-see",
				Scope: gateway.PitchScope{Kind: gateway.PitchScopeInitiative, Goals: []gateway.PitchGoal{
					{Slug: "watch-metric", Title: "Watch the metric", Statement: "We track it for a month.", Acceptance: "a dashboard exists"},
				}},
				// no estimate, no signal, no risks named
			},
		},
		Answers: []gateway.PitchAnswerDraft{
			{PitchSlug: "trim-onboarding", Text: "Yes, this also covers the invite flow."},
		},
	}
}

// A propose-mode hand-in's pitches and answers reach the fake under
// their camelCase names, with every field — a required list a pitch
// named none of (risks, on the second pitch) sent as `[]`, not left
// out, and estimate/signal left out when the pitch names neither.
func TestRecordDirectionResearchWithPitches(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Directions["d1"] = directionRow("d1", projID, "scout")
	f.Directions["d1"]["researchNodeId"] = nodeID
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	if err := gw.RecordDirectionResearch(ctx, "d1", nodeID, pitchesInput(), 200, "2 pitched", nil); err != nil {
		t.Fatal(err)
	}
	rec := f.CallsNamed("recordDirectionResearch")
	if len(rec) != 1 {
		t.Fatalf("recordDirectionResearch calls: %+v", rec)
	}
	pitches, _ := rec[0].Input["pitches"].([]any)
	if len(pitches) != 2 {
		t.Fatalf("pitches: %+v", rec[0].Input["pitches"])
	}
	first, _ := pitches[0].(map[string]any)
	if first["slug"] != "trim-onboarding" || first["title"] != "Trim onboarding to one screen" ||
		first["problem"] == "" || first["bet"] != "fewer-steps" {
		t.Fatalf("pitch: %+v", first)
	}
	evidence, _ := first["evidence"].([]any)
	if len(evidence) != 1 {
		t.Fatalf("evidence: %+v", first["evidence"])
	}
	ev, _ := evidence[0].(map[string]any)
	if ev["kind"] != "metric" || ev["ref"] != "activation-dashboard" || ev["note"] == "" {
		t.Fatalf("evidence row: %+v", ev)
	}
	scope, _ := first["scope"].(map[string]any)
	if scope["kind"] != "goal" {
		t.Fatalf("scope: %+v", scope)
	}
	goals, _ := scope["goals"].([]any)
	if len(goals) != 1 {
		t.Fatalf("scope goals: %+v", scope["goals"])
	}
	goal, _ := goals[0].(map[string]any)
	if goal["slug"] != "one-screen-onboarding" || goal["budgetCents"] != float64(5000) {
		t.Fatalf("scope goal: %+v", goal)
	}
	estimate, _ := first["estimate"].(map[string]any)
	if estimate["costCents"] != float64(4000) || estimate["days"] != float64(3) {
		t.Fatalf("estimate: %+v", first["estimate"])
	}
	signal, _ := first["signal"].(map[string]any)
	if signal["metric"] == "" || signal["direction"] != "up" || signal["windowDays"] != float64(14) {
		t.Fatalf("signal: %+v", first["signal"])
	}
	risks, _ := first["risks"].([]any)
	if len(risks) != 1 {
		t.Fatalf("risks: %+v", first["risks"])
	}

	second, _ := pitches[1].(map[string]any)
	if _, ok := second["estimate"]; ok {
		t.Fatalf("no estimate is left out: %+v", second)
	}
	if _, ok := second["signal"]; ok {
		t.Fatalf("no signal is left out: %+v", second)
	}
	secondRisks, ok := second["risks"].([]any)
	if !ok || len(secondRisks) != 0 {
		t.Fatalf("a pitch naming no risks still sends risks as []: %+v", second["risks"])
	}

	answers, _ := rec[0].Input["answers"].([]any)
	if len(answers) != 1 {
		t.Fatalf("answers: %+v", rec[0].Input["answers"])
	}
	answer, _ := answers[0].(map[string]any)
	if answer["pitchSlug"] != "trim-onboarding" || answer["text"] == "" {
		t.Fatalf("answer: %+v", answer)
	}
}

// Against a platform that refuses pitches (older than runsheet trd
// §6c), the command is re-sent as proposals — the given ones plus one
// per goal of each pitch's scope — and lands; ErrNoPitches says the
// pitches themselves did not, and the usage report rides both attempts.
func TestRecordDirectionResearchFallsBackWithoutPitches(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["pitches"] = true
	f.Directions["d1"] = directionRow("d1", projID, "scout")
	f.Directions["d1"]["researchNodeId"] = nodeID
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	research := pitchesInput()
	research.Proposals = []gateway.GoalProposal{
		{Slug: "kept-proposal", Title: "A proposal made alongside the pitches", Statement: "s", Acceptance: "a"},
	}
	u := sampleUsageReport()
	err := gw.RecordDirectionResearch(ctx, "d1", nodeID, research, 200, "2 pitched", u)
	if !errors.Is(err, gateway.ErrNoPitches) {
		t.Fatalf("err = %v, want ErrNoPitches", err)
	}
	rec := f.CallsNamed("recordDirectionResearch")
	if len(rec) != 1 {
		t.Fatalf("only the landed call is recorded: %+v", rec)
	}
	if _, ok := rec[0].Input["pitches"]; ok {
		t.Fatalf("the fallback carries no pitches: %+v", rec[0].Input)
	}
	if _, ok := rec[0].Input["answers"]; ok {
		t.Fatalf("the fallback carries no answers: %+v", rec[0].Input)
	}
	proposals, _ := rec[0].Input["proposals"].([]any)
	// the given proposal, plus one per goal of each of the two pitches
	if len(proposals) != 3 {
		t.Fatalf("proposals: %+v", proposals)
	}
	slugs := map[string]bool{}
	for _, p := range proposals {
		row, _ := p.(map[string]any)
		slugs[row["slug"].(string)] = true
	}
	for _, want := range []string{"kept-proposal", "one-screen-onboarding", "watch-metric"} {
		if !slugs[want] {
			t.Fatalf("proposals missing %q: %+v", want, proposals)
		}
	}
	if _, ok := rec[0].Input["usage"]; !ok {
		t.Fatalf("the landed fallback still carries usage: %+v", rec[0].Input)
	}
	if len(f.SessionUsage) != 1 || f.SessionUsage[0].Name != "recordDirectionResearch" {
		t.Fatalf("SessionUsage = %+v", f.SessionUsage)
	}
}

// A direction's mode rides the row (runsheet ADR 022): propose is one
// whose research only proposes; set, or none, is guarded by the room.
func TestDirectionsReadTheMode(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Directions["d1"] = directionRow("d1", projID, "scout")
	f.Directions["d1"]["mode"] = "propose"
	f.Directions["d2"] = directionRow("d2", projID, "overnight")
	f.Directions["d2"]["mode"] = "set"
	gw := gateway.New(f.URL(), "rs_test")

	got, err := gw.Directions(context.Background(), orgID, projID)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]gateway.Direction{}
	for _, d := range got {
		modes[d.Slug] = d
	}
	if d := modes["scout"]; d.Mode != "propose" || !d.Proposes() {
		t.Fatalf("a propose direction: %+v", d)
	}
	if d := modes["overnight"]; d.Mode != "set" || d.Proposes() {
		t.Fatalf("a set direction: %+v", d)
	}
	if (gateway.Direction{}).Proposes() {
		t.Fatal("a direction with no mode is guarded, not proposing")
	}
}

// A platform older than ADR 022 refuses mode: the read falls back to
// the required fields, still returns the rows — with no mode, so
// guarded — and stops asking for it.
func TestDirectionsWithoutModeOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["mode"] = true
	f.Directions["d1"] = directionRow("d1", projID, "scout")
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		got, err := gw.Directions(ctx, orgID, projID)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if len(got) != 1 || got[0].ID != "d1" || got[0].Mode != "" || got[0].Proposes() {
			t.Fatalf("read %d: %+v", i, got)
		}
	}
	// the first read tried mode once; every read after asks without
	if n := len(f.ReadsNamed("directionIndexs")); n != 3 {
		t.Fatalf("directionIndexs read %d times, want 3", n)
	}
}
