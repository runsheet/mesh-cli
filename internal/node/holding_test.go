package node

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// A paced subscription and an exhausted one: the report names both
// accounts' standing and the node's own aggregate summary — the more
// lenient of the two, since nodePace takes the most lenient account.
func TestHoldingReportPacedAndExhausted(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	fiveOver := time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC)
	weekOver := time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC)
	accounts := []AccountConfig{
		{Label: "max20", Kind: AccountSubscription},
		{Label: "shhemail", Kind: AccountSubscription},
	}
	readings := map[string]harness.RateLimit{
		"max20": {
			Status:     "allowed",
			ObservedAt: now,
			FiveHour:   harness.RateLimitWindow{Utilization: 0.85, ResetsAt: fiveOver},
			SevenDay:   harness.RateLimitWindow{Utilization: 0.2, ResetsAt: weekOver.Add(24 * time.Hour)},
		},
		"shhemail": {
			Status:     "allowed_warning",
			ObservedAt: now,
			FiveHour:   harness.RateLimitWindow{Utilization: 0.1, ResetsAt: fiveOver},
			SevenDay:   harness.RateLimitWindow{Utilization: 0.99, ResetsAt: weekOver},
		},
	}
	cfg := LimitsConfig{}
	p := nodePace(now, accounts, readings, cfg)
	if p.mode != pacePaced {
		t.Fatalf("nodePace = %v, want paced", p.mode)
	}
	rep := HoldingReport(now, accounts, readings, cfg, 6, p, nil, nil)
	if rep.Mode != "paced" {
		t.Fatalf("mode = %q, want paced", rep.Mode)
	}
	if len(rep.Accounts) != 2 {
		t.Fatalf("accounts = %#v", rep.Accounts)
	}
	a, b := rep.Accounts[0], rep.Accounts[1]
	if a.Account != "max20" || a.Status != "paced" || !a.ResetsAt.Equal(fiveOver) {
		t.Fatalf("max20 = %+v", a)
	}
	if len(a.Takes.Priorities) != 2 || a.Takes.Priorities[0] != "urgent" || a.Takes.Priorities[1] != "high" || a.Takes.MaxItems != 3 || a.Takes.Research {
		t.Fatalf("max20.takes = %+v", a.Takes)
	}
	if b.Account != "shhemail" || b.Status != "exhausted" || !b.ResetsAt.Equal(weekOver) {
		t.Fatalf("shhemail = %+v", b)
	}
	if len(b.Takes.Priorities) != 0 || b.Takes.MaxItems != 0 || b.Takes.Research {
		t.Fatalf("shhemail.takes = %+v", b.Takes)
	}
	want := "paced: urgent and high only, 3 item(s), no research"
	if rep.Summary != want {
		t.Fatalf("summary = %q, want %q", rep.Summary, want)
	}
}

// Every account exhausted: the summary says no headroom, not a mode of
// "paced" or "stopped" alone, and names the earliest of the resets.
func TestHoldingReportEveryAccountExhausted(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	earlier := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC)
	accounts := []AccountConfig{
		{Label: "max20", Kind: AccountSubscription},
		{Label: "shhemail", Kind: AccountSubscription},
	}
	readings := map[string]harness.RateLimit{
		"max20":    {Status: "rejected", ObservedAt: now, ResetsAt: earlier},
		"shhemail": {Status: "allowed", ObservedAt: now, SevenDay: harness.RateLimitWindow{Utilization: 0.99, ResetsAt: later}},
	}
	cfg := LimitsConfig{}
	p := nodePace(now, accounts, readings, cfg)
	if p.mode != paceStopped {
		t.Fatalf("nodePace = %v, want stopped", p.mode)
	}
	rep := HoldingReport(now, accounts, readings, cfg, 6, p, nil, nil)
	want := "no headroom: nothing claimed until " + earlier.Format(time.RFC3339)
	if !strings.HasPrefix(rep.Summary, "no headroom: nothing claimed until ") {
		t.Fatalf("summary = %q, want prefix %q", rep.Summary, "no headroom: nothing claimed until ")
	}
	if rep.Summary != want {
		t.Fatalf("summary = %q, want %q (the earliest reset)", rep.Summary, want)
	}
	for _, a := range rep.Accounts {
		if a.Status != "exhausted" {
			t.Fatalf("account %s = %q, want exhausted", a.Account, a.Status)
		}
	}
}

// An account with no reading yet is unknown, not exhausted:
// ChooseAccount treats it as full headroom, so the report says the node
// still takes everything on it.
func TestHoldingReportUnknownAccountTakesEverything(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	accounts := []AccountConfig{{Label: "fresh", Kind: AccountSubscription}}
	cfg := LimitsConfig{}
	p := nodePace(now, accounts, nil, cfg)
	rep := HoldingReport(now, accounts, nil, cfg, 4, p, nil, nil)
	if len(rep.Accounts) != 1 {
		t.Fatalf("accounts = %#v", rep.Accounts)
	}
	a := rep.Accounts[0]
	if a.Status != "unknown" {
		t.Fatalf("status = %q, want unknown", a.Status)
	}
	if !a.ResetsAt.IsZero() {
		t.Fatalf("resetsAt = %v, want zero", a.ResetsAt)
	}
	want := []string{"urgent", "high", "normal", "low"}
	if len(a.Takes.Priorities) != len(want) {
		t.Fatalf("priorities = %#v, want %#v", a.Takes.Priorities, want)
	}
	for i, w := range want {
		if a.Takes.Priorities[i] != w {
			t.Fatalf("priorities = %#v, want %#v", a.Takes.Priorities, want)
		}
	}
	if a.Takes.MaxItems != 4 || !a.Takes.Research {
		t.Fatalf("takes = %+v, want maxItems 4 and research true (a subscription)", a.Takes)
	}
	if rep.Summary != "normal: up to 4 item(s)" {
		t.Fatalf("summary = %q", rep.Summary)
	}
}

// A normal node with ready work left unclaimed for another reason (not
// pacing — item 2 wires that in) still names it on the summary.
func TestHoldingReportNamesUnclaimedWork(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	accounts := []AccountConfig{{Label: "max20", Kind: AccountSubscription}}
	readings := map[string]harness.RateLimit{"max20": {Status: "allowed", ObservedAt: now}}
	cfg := LimitsConfig{}
	p := nodePace(now, accounts, readings, cfg)
	unclaimed := []Unclaimed{
		{GoalID: "goal-1", Kind: "item", Count: 2, Reason: "no account has headroom"},
		{GoalID: "goal-2", Kind: "plan", Count: 1, Reason: "no account has headroom"},
	}
	rep := HoldingReport(now, accounts, readings, cfg, 4, p, unclaimed, nil)
	want := "normal: up to 4 item(s); 2 item(s) and 1 plan(s) left unclaimed"
	if rep.Summary != want {
		t.Fatalf("summary = %q, want %q", rep.Summary, want)
	}
	if len(rep.Unclaimed) != 2 {
		t.Fatalf("unclaimed = %#v", rep.Unclaimed)
	}
}

// An unclaimed entry's Until rides the report (item 4 of
// no-headroom-is-a-hold-not-a-claim-loop): the report's own until is
// the earliest of a hold's until and the accounts' resetsAt, and a zero
// Until is left out of the wire shape (omitzero).
func TestHoldingReportCarriesUnclaimedUntil(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	accountReset := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	holdUntil := time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC) // earlier than the account's
	accounts := []AccountConfig{{Label: "max20", Kind: AccountSubscription}}
	readings := map[string]harness.RateLimit{
		"max20": {Status: "rejected", ObservedAt: now, ResetsAt: accountReset},
	}
	cfg := LimitsConfig{}
	p := nodePace(now, accounts, readings, cfg)
	unclaimed := []Unclaimed{
		{GoalID: "goal-1", Kind: "item", Count: 1, Reason: "no issued credential has headroom", Until: holdUntil},
		{GoalID: "goal-2", Kind: "plan", Count: 1, Reason: "the node is at its min"},
	}
	rep := HoldingReport(now, accounts, readings, cfg, 4, p, unclaimed, nil)
	if !rep.Until.Equal(holdUntil) {
		t.Fatalf("report.Until = %v, want %v (the earliest of the hold and the account's reset)", rep.Until, holdUntil)
	}
	var withUntil, withoutUntil gateway.HoldingUnclaimed
	for _, u := range rep.Unclaimed {
		switch u.GoalID {
		case "goal-1":
			withUntil = u
		case "goal-2":
			withoutUntil = u
		}
	}
	if !withUntil.Until.Equal(holdUntil) {
		t.Fatalf("goal-1.Until = %v, want %v", withUntil.Until, holdUntil)
	}
	if !withoutUntil.Until.IsZero() {
		t.Fatalf("goal-2.Until = %v, want zero", withoutUntil.Until)
	}
	b, err := json.Marshal(withoutUntil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"until"`) {
		t.Fatalf("a zero Until was not omitted: %s", b)
	}
	b2, err := json.Marshal(withUntil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), `"until"`) {
		t.Fatalf("a set Until was omitted: %s", b2)
	}
}

// An api account's kind renders as "api_key" (the holding shape's own
// name), and it never takes research even with full headroom.
func TestHoldingReportAPIAccountKindAndResearch(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	accounts := []AccountConfig{{Label: "key", Kind: AccountAPI}}
	cfg := LimitsConfig{}
	p := nodePace(now, accounts, nil, cfg)
	rep := HoldingReport(now, accounts, nil, cfg, 4, p, nil, nil)
	a := rep.Accounts[0]
	if a.Kind != "api_key" {
		t.Fatalf("kind = %q, want api_key", a.Kind)
	}
	if a.Takes.Research {
		t.Fatalf("takes.research = true, want false for an api account")
	}
}

// The holding report carries one row per served org, as holdingOrgs
// built them: floor, held and ready, and -1 ready for an org whose
// ready read failed (ADR-023, Reporting).
func TestHoldingReportOrgs(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	scopes := Scopes{{Login: "runsheet", ID: "org-r"}, {Slug: "wardroomid", ID: "org-w"}, {Login: "quiet", ID: "org-q"}}
	floors := Floors(5, scopes, nil)
	rows := holdingOrgs(scopes, floors, map[string]int{"org-r": 1, "org-w": 2}, map[string]int{"org-r": 3, "org-w": 0})
	rep := HoldingReport(now, nil, nil, LimitsConfig{}, 5, nodePace(now, nil, nil, LimitsConfig{}), nil, rows)
	want := []gateway.HoldingOrg{
		{Org: "runsheet", OrgID: "org-r", Floor: 2, Held: 1, Ready: 3},
		{Org: "wardroomid", OrgID: "org-w", Floor: 2, Held: 2, Ready: 0},
		{Org: "quiet", OrgID: "org-q", Floor: 1, Held: 0, Ready: -1},
	}
	if len(rep.Orgs) != len(want) {
		t.Fatalf("orgs = %+v", rep.Orgs)
	}
	for i := range want {
		if rep.Orgs[i] != want[i] {
			t.Fatalf("orgs[%d] = %+v, want %+v", i, rep.Orgs[i], want[i])
		}
	}
	b, _ := json.Marshal(rep)
	if !strings.Contains(string(b), `"orgs":[{"org":"runsheet","orgId":"org-r","floor":2,"held":1,"ready":3}`) {
		t.Fatalf("wire shape: %s", b)
	}
	// no org served is still a list
	b, _ = json.Marshal(HoldingReport(now, nil, nil, LimitsConfig{}, 5, nodePace(now, nil, nil, LimitsConfig{}), nil, nil))
	if !strings.Contains(string(b), `"orgs":[]`) {
		t.Fatalf("no orgs: %s", b)
	}
}
