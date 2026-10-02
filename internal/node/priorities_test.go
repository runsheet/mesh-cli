package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

// An account that names no priorities serves every one, as before; one
// that names some serves those alone, and work of no priority
// (research) runs on any account.
func TestChooseAccountServesEmptyMeansAll(t *testing.T) {
	all := AccountConfig{Label: "all"}
	for _, p := range []string{"urgent", "high", "normal", "low", ""} {
		if !all.Serves(p) {
			t.Errorf("an account with no priorities does not serve %q", p)
		}
	}
	reserved := AccountConfig{Label: "reserved", Priorities: []string{"urgent", "high"}}
	for p, want := range map[string]bool{"urgent": true, "high": true, "normal": false, "low": false, "": true} {
		if got := reserved.Serves(p); got != want {
			t.Errorf("reserved.Serves(%q) = %v, want %v", p, got, want)
		}
	}
}

// A subscription reserved for urgent and high work sits idle for normal
// and low work, whatever its headroom, and takes urgent and high by
// headroom like any other.
func TestChooseAccountSkipsAnAccountThatDoesNotServeThePriority(t *testing.T) {
	a := AccountConfig{Label: "a", Kind: AccountSubscription, Priorities: []string{"urgent", "high"}}
	b := AccountConfig{Label: "b", Kind: AccountSubscription}
	type readings = map[string]harness.RateLimit
	for _, c := range []struct {
		name     string
		accounts []AccountConfig
		readings readings
		priority string
		want     string // "" is ok=false
	}{
		{"normal work skips the reserved one", []AccountConfig{a, b}, nil, "normal", "b"},
		{"low work skips the reserved one", []AccountConfig{a, b}, nil, "low", "b"},
		{"normal work skips it with b busier", []AccountConfig{a, b}, readings{"b": *reading(0.7, 0.7)}, "normal", "b"},
		{"urgent work takes the first on a tie", []AccountConfig{a, b}, nil, "urgent", "a"},
		{"urgent work takes the most headroom", []AccountConfig{a, b}, readings{"a": *reading(0.5, 0.5)}, "urgent", "b"},
		{"the reserved one alone: no normal", []AccountConfig{a}, nil, "normal", ""},
		{"the reserved one alone: no low", []AccountConfig{a}, nil, "low", ""},
		{"the reserved one alone: urgent", []AccountConfig{a}, nil, "urgent", "a"},
		{"the reserved one alone: high", []AccountConfig{a}, nil, "high", "a"},
		{"research runs on it", []AccountConfig{a}, nil, "", "a"},
	} {
		got, ok := ChooseAccount(t0, c.accounts, c.readings, LimitsConfig{}, c.priority, true)
		if c.want == "" {
			if ok {
				t.Errorf("%s: chose %s, want none", c.name, got.Label)
			}
			continue
		}
		if !ok || got.Label != c.want {
			t.Errorf("%s: chose %q (ok=%v), want %s", c.name, got.Label, ok, c.want)
		}
	}
}

// Every subscription serving the work's priority stopped — none, for a
// subscription reserved for urgent and high — lets an api key step in
// for normal work that waits; one that serves it with headroom keeps
// it off the api key; an api key reserved for urgent is skipped too.
func TestChooseAccountExhaustedIsOverServingSubscriptions(t *testing.T) {
	a := AccountConfig{Label: "a", Kind: AccountSubscription, Priorities: []string{"urgent", "high"}}
	b := AccountConfig{Label: "b", Kind: AccountSubscription}
	key := AccountConfig{Label: "key", Kind: AccountAPI}
	urgentKey := AccountConfig{Label: "urgent-key", Kind: AccountAPI, Priorities: []string{"urgent"}}
	type readings = map[string]harness.RateLimit
	for _, c := range []struct {
		name      string
		accounts  []AccountConfig
		readings  readings
		priority  string
		workWaits bool
		want      string
	}{
		{"no subscription serves normal: the api", []AccountConfig{a, key}, nil, "normal", true, "key"},
		{"no subscription serves normal, nothing waits: none", []AccountConfig{a, key}, nil, "normal", false, ""},
		{"a serving subscription with headroom: not the api", []AccountConfig{a, b, key}, nil, "normal", true, "b"},
		{"the serving subscription stopped: the api", []AccountConfig{a, b, key}, readings{"b": *reading(0.97, 0.1)}, "normal", true, "key"},
		{"an api key reserved for urgent: not for normal", []AccountConfig{a, urgentKey}, nil, "normal", true, ""},
		{"an api key reserved for urgent: for urgent over a paced one", []AccountConfig{a, urgentKey}, readings{"a": *reading(0.85, 0.1)}, "urgent", true, "urgent-key"},
	} {
		got, ok := ChooseAccount(t0, c.accounts, c.readings, LimitsConfig{}, c.priority, c.workWaits)
		if c.want == "" {
			if ok {
				t.Errorf("%s: chose %s, want none", c.name, got.Label)
			}
			continue
		}
		if !ok || got.Label != c.want {
			t.Errorf("%s: chose %q (ok=%v), want %s", c.name, got.Label, ok, c.want)
		}
	}
}

// The Codex evaluator's chooser honours an account's priorities: a
// Codex account reserved for urgent and high evaluates a high item and
// not a normal one, pinned or not.
func TestChooseAccountCodexChooserServesPriorities(t *testing.T) {
	codex := AccountConfig{Label: "chatgpt-pro", Kind: AccountSubscription, Adapter: AdapterCodex, Priorities: []string{"urgent", "high"}}
	e := &Evaluator{Limits: &Limits{}}
	for _, pinned := range []bool{false, true} {
		if got, ok := e.codexChooser("normal", pinned)([]AccountConfig{codex}); ok {
			t.Errorf("pinned=%v: a normal evaluation chose %s", pinned, got.Label)
		}
		if got, ok := e.codexChooser("high", pinned)([]AccountConfig{codex}); !ok || got.Label != codex.Label {
			t.Errorf("pinned=%v: a high evaluation chose %q (ok=%v)", pinned, got.Label, ok)
		}
	}
}

// An account's own pace point paces it in place of the node-wide one;
// one with none is paced by the node's.
func TestPaceAccountsOwnPacePoint(t *testing.T) {
	cfg := LimitsConfig{PaceFiveHour: 0.8}
	own := AccountConfig{Label: "own", PaceFiveHour: 0.5}
	none := AccountConfig{Label: "none"}
	if p := pace(t0, *reading(0.6, 0.1), cfg.For(own)); p.mode != pacePaced {
		t.Errorf("PaceFiveHour 0.5 at 0.6: %v, want paced", p.mode)
	}
	if p := pace(t0, *reading(0.6, 0.1), cfg.For(none)); p.mode != paceNormal {
		t.Errorf("no pace point at 0.6 under 0.8: %v, want normal", p.mode)
	}
	week := AccountConfig{Label: "week", PaceSevenDay: 0.7}
	p := pace(t0, *reading(0.1, 0.75), cfg.For(week))
	if p.mode != pacePaced || !p.resumeAt.Equal(weekOver) {
		t.Errorf("PaceSevenDay 0.7 at 0.75: %v until %v, want paced until %v", p.mode, p.resumeAt, weekOver)
	}
	if p := pace(t0, *reading(0.1, 0.75), cfg.For(none)); p.mode != paceNormal {
		t.Errorf("no seven-day pace point at 0.75: %v, want normal", p.mode)
	}
	// ChooseAccount judges by it: normal work skips own, urgent takes it
	readings := map[string]harness.RateLimit{"own": *reading(0.6, 0.1), "none": *reading(0.7, 0.1)}
	own.Kind, none.Kind = AccountSubscription, AccountSubscription
	if got, ok := ChooseAccount(t0, []AccountConfig{own, none}, readings, cfg, "normal", true); !ok || got.Label != "none" {
		t.Errorf("normal work chose %q (ok=%v), want none", got.Label, ok)
	}
	if got, ok := ChooseAccount(t0, []AccountConfig{own}, readings, cfg, "urgent", true); !ok || got.Label != "own" {
		t.Errorf("urgent work chose %q (ok=%v), want own", got.Label, ok)
	}
	// and the holding report's status is the same judgement
	rows := holdingAccounts(t0, []AccountConfig{own, none}, readings, cfg, 4)
	if rows[0].Status != "paced" || rows[1].Status != "ok" {
		t.Errorf("holding statuses %s, %s, want paced, ok", rows[0].Status, rows[1].Status)
	}
}

// An ok account reserved for urgent and high takes urgent and high
// alone, and a normal goal held is said to be held by it.
func TestHoldingTakesWhatTheAccountServes(t *testing.T) {
	a := AccountConfig{Label: "mesh - max20", Kind: AccountSubscription, Priorities: []string{"urgent", "high"}}
	readings := map[string]harness.RateLimit{"mesh - max20": *reading(0.1, 0.1)}
	rows := holdingAccounts(t0, []AccountConfig{a}, readings, LimitsConfig{}, 4)
	if rows[0].Status != "ok" || !slices.Equal(rows[0].Takes.Priorities, []string{"urgent", "high"}) || rows[0].Takes.MaxItems != 4 || !rows[0].Takes.Research {
		t.Fatalf("row = %+v, want ok taking [urgent high], 4 items, research", rows[0])
	}
	reason := heldReason("normal", "", rows)
	if want := "normal priority; mesh - max20 serves only urgent and high"; reason != want {
		t.Fatalf("heldReason = %q, want %q", reason, want)
	}
	if !strings.Contains(reason, "serves only urgent and high") {
		t.Fatal("no serves line")
	}
	// paced, it takes the intersection: still urgent and high
	readings["mesh - max20"] = *reading(0.85, 0.1)
	rows = holdingAccounts(t0, []AccountConfig{a}, readings, LimitsConfig{}, 4)
	if !slices.Equal(rows[0].Takes.Priorities, []string{"urgent", "high"}) {
		t.Fatalf("paced takes %v", rows[0].Takes.Priorities)
	}
	// one reserved for urgent alone, paced, takes urgent alone
	u := AccountConfig{Label: "u", Kind: AccountSubscription, Priorities: []string{"urgent"}}
	readings["u"] = *reading(0.85, 0.1)
	rows = holdingAccounts(t0, []AccountConfig{u}, readings, LimitsConfig{}, 4)
	if !slices.Equal(rows[0].Takes.Priorities, []string{"urgent"}) {
		t.Fatalf("paced urgent-only takes %v", rows[0].Takes.Priorities)
	}
}

// With the only account with headroom reserved for urgent and high and
// a normal item ready, the node claims nothing, fails and releases
// nothing, and says why: no account serves normal work. The holding
// report's reason says the same.
func TestWaitingNoAccountServesNormalWork(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	loop, logs, _ := p.pacedLoop(2, nil)
	knowAccounts(loop, AccountConfig{Label: "max20", Kind: AccountSubscription, Priorities: []string{"urgent", "high"}})
	loop.Tick(context.Background())
	loop.Tick(context.Background())
	loop.wg.Wait()
	if n := len(p.CallsNamed("claimWorkItem")); n != 0 {
		t.Fatalf("claimed %d time(s) with no account serving normal work", n)
	}
	if n := len(p.CallsNamed("failWorkItem")) + len(p.CallsNamed("releaseWorkItem")); n != 0 {
		t.Fatalf("an unclaimed item was failed or released %d time(s)", n)
	}
	if s := p.Item("normal-1")["status"]; s != "ready" {
		t.Fatalf("normal-1 = %v, want ready", s)
	}
	const line = "accounts: no account serves normal work (max20 serves urgent and high); 1 item(s) left unclaimed until one does"
	if n := strings.Count(logs.String(), line); n != 2 {
		t.Fatalf("want the line once per tick (2), got %d:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "no account has headroom") {
		t.Fatalf("the headroom line for a priority wait:\n%s", logs)
	}
	h := loop.Holding()
	if h == nil || len(h.Unclaimed) != 1 || !strings.Contains(h.Unclaimed[0].Reason, "max20 serves only urgent and high") {
		t.Fatalf("holding = %+v, want the reason to name what max20 serves", h)
	}
}
