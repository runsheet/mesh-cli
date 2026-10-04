package node

import (
	"context"
	"strings"
	"testing"
	"time"
)

// holdingLoop is priorityFake's loop at t0 on two subscriptions: mesh -
// max20 paced at 85% of its five-hour window, and claude - shhemail
// exhausted at 99% of its seven-day one.
func (p *priorityFake) holdingLoop(capacity int) (*Loop, *strings.Builder) {
	loop, logs, _ := p.pacedLoop(capacity, reading(0.1, 0.1))
	loop.Limits = &Limits{}
	knowAccounts(loop, []AccountConfig{
		{Label: "mesh - max20", Kind: AccountSubscription},
		{Label: "claude - shhemail", Kind: AccountSubscription},
	}...)
	loop.Limits.Observe("mesh - max20", reading(0.85, 0.2))
	loop.Limits.Observe("claude - shhemail", reading(0.1, 0.99))
	return loop, logs
}

// lastHolding is the holding the fake's last heartbeat carried.
func (p *priorityFake) lastHolding() map[string]any {
	p.t.Helper()
	hb := p.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		p.t.Fatal("no heartbeat")
	}
	h, ok := hb[len(hb)-1].Input["holding"].(map[string]any)
	if !ok {
		p.t.Fatalf("last heartbeat's holding = %#v", hb[len(hb)-1].Input["holding"])
	}
	return h
}

// unclaimedOf is holding.unclaimed by goal id and kind.
func unclaimedOf(t *testing.T, h map[string]any) map[string]map[string]any {
	t.Helper()
	list, ok := h["unclaimed"].([]any)
	if !ok {
		t.Fatalf("holding.unclaimed = %#v, want a list", h["unclaimed"])
	}
	out := map[string]map[string]any{}
	for _, e := range list {
		u := e.(map[string]any)
		out[u["goalId"].(string)+"/"+u["kind"].(string)] = u
	}
	return out
}

// A paced account and an exhausted one, a ready normal item and a
// normal goal in planning: neither is claimed, and the next heartbeat
// carries the last pass's holding — paced, both goals left unclaimed,
// each reason naming both accounts and the exhausted one's reset.
func TestHeartbeatCarriesHoldingOfPacedAndExhaustedAccounts(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	p.addPlanning(orgID, "plan-normal", "normal")
	loop, logs := p.holdingLoop(4)

	loop.Tick(context.Background())
	if hb := p.CallsNamed("heartbeatNode"); hb[len(hb)-1].Input["holding"] != nil {
		t.Fatalf("the first heartbeat, before any pass, carried holding %#v", hb[len(hb)-1].Input["holding"])
	}
	loop.Tick(context.Background())
	loop.wg.Wait()

	if n := len(p.CallsNamed("claimWorkItem")) + len(p.CallsNamed("claimPlan")); n != 0 {
		t.Fatalf("claimed %d time(s) with no headroom for normal work", n)
	}
	if s := p.Item("normal-1")["status"]; s != "ready" {
		t.Fatalf("normal-1 = %v, want ready", s)
	}
	if s := p.Goals["plan-normal"]["status"]; s != "planning" {
		t.Fatalf("plan-normal = %v, want planning", s)
	}
	if !strings.Contains(logs.String(), "accounts: no account has headroom for 1 item(s), 1 plan(s)") {
		t.Fatalf("the waiting line is gone:\n%s", logs)
	}

	h := p.lastHolding()
	if h["mode"] != "paced" {
		t.Fatalf("holding.mode = %v, want paced", h["mode"])
	}
	un := unclaimedOf(t, h)
	if len(un) != 2 {
		t.Fatalf("holding.unclaimed = %#v, want the item and the plan", h["unclaimed"])
	}
	want := "normal priority; only urgent and high while paced (mesh - max20 until " + fiveOver.Format(time.RFC3339) + "); claude - shhemail exhausted until " + weekOver.Format(time.RFC3339)
	for _, k := range []string{"steady/item", "plan-normal/plan"} {
		u, ok := un[k]
		if !ok {
			t.Fatalf("holding.unclaimed has no %s: %#v", k, h["unclaimed"])
		}
		if u["reason"] != want {
			t.Fatalf("%s reason = %q, want %q", k, u["reason"], want)
		}
		if u["count"] != float64(1) {
			t.Fatalf("%s count = %v, want 1 (once per goal per pass)", k, u["count"])
		}
	}
}

// A goal's items passed over are one entry per pass, counting them.
func TestHoldingNamesAGoalOncePerPass(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	p.addItem(orgID, "steady", "normal-2", 2, false)
	loop, _ := p.holdingLoop(4)
	loop.Tick(context.Background())
	loop.Tick(context.Background())
	un := unclaimedOf(t, p.lastHolding())
	if len(un) != 1 || un["steady/item"]["count"] != float64(2) {
		t.Fatalf("holding.unclaimed = %#v, want steady once with count 2", un)
	}
}

// A node with headroom sends holding with unclaimed [].
func TestHeartbeatHoldingWithHeadroomLeavesNothingUnclaimed(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	loop, _, _ := p.pacedLoop(4, reading(0.1, 0.1))
	loop.Tick(context.Background())
	loop.Tick(context.Background())
	loop.wg.Wait()
	h := p.lastHolding()
	if h["mode"] != "normal" {
		t.Fatalf("holding.mode = %v, want normal", h["mode"])
	}
	if list, ok := h["unclaimed"].([]any); !ok || len(list) != 0 {
		t.Fatalf("holding.unclaimed = %#v, want []", h["unclaimed"])
	}
}

// An urgent item on the same paced node is claimed and is not among
// the unclaimed; the normal one beside it is.
func TestHoldingLeavesOutTheUrgentItemAPacedNodeClaims(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "fire", "urgent")
	p.addItem(orgID, "fire", "urgent-1", 1, false)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 2, false)
	loop, _ := p.holdingLoop(4)
	if got := p.claimedBy(loop); strings.Join(got, ",") != "urgent-1" {
		t.Fatalf("paced claimed %v, want urgent-1", got)
	}
	loop.Tick(context.Background())
	loop.wg.Wait()
	un := unclaimedOf(t, p.lastHolding())
	if _, ok := un["fire/item"]; ok {
		t.Fatalf("the claimed urgent goal is among the unclaimed: %#v", un)
	}
	if _, ok := un["steady/item"]; !ok {
		t.Fatalf("the normal goal is not among the unclaimed: %#v", un)
	}
}

// A NoCredentials headroom hold (item 1) on the item's project lands on
// the heartbeat's holding.unclaimed with the hold's own reason and
// until, in RFC 3339 (item 4 of no-headroom-is-a-hold-not-a-claim-loop).
func TestHoldingCarriesANoCredentialHeadroomHoldUntil(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	loop, _, c := p.pacedLoop(4, reading(0.1, 0.1))
	until := c.Now().Add(30 * time.Minute)
	loop.NoCredential = &NoCredentials{Now: loop.Now}
	e := &ErrNoHeadroom{Org: "acme", Project: "mesh", ProjectID: projID, Kind: "build", Adapter: AdapterClaudeCode, Refused: []string{"max20"}, Until: until}
	loop.NoCredential.NoteHeadroom("steady", e)

	loop.Tick(context.Background())
	loop.Tick(context.Background())
	loop.wg.Wait()

	if n := len(p.CallsNamed("claimWorkItem")); n != 0 {
		t.Fatalf("claimed %d time(s) under a headroom hold", n)
	}
	un := unclaimedOf(t, p.lastHolding())
	u, ok := un["steady/item"]
	if !ok {
		t.Fatalf("holding.unclaimed has no steady/item: %#v", un)
	}
	if reason, _ := u["reason"].(string); !strings.Contains(reason, e.Error()) {
		t.Fatalf("reason = %q, want it to contain %q", reason, e.Error())
	}
	if got := u["until"]; got != until.Format(time.RFC3339) {
		t.Fatalf("until = %v, want %v", got, until.Format(time.RFC3339))
	}
}

// A draining node makes no pass and sends the last report unchanged.
func TestDrainingNodeSendsTheLastHolding(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	loop, _ := p.holdingLoop(4)
	loop.Tick(context.Background())
	before := loop.Holding()
	loop.Drain()
	loop.Tick(context.Background())
	if loop.Holding() != before {
		t.Fatal("a draining tick replaced the holding report")
	}
	if len(unclaimedOf(t, p.lastHolding())) != 1 {
		t.Fatalf("the draining heartbeat's holding = %#v", p.lastHolding())
	}
}

// The heartbeat's holding carries the kinds this node takes as `kinds`
// only while the gateway's holding input takes it, and the summary
// always opens with "takes <list>; " when fewer than every kind is
// taken, whether or not the platform reads the structured field
// (ADR-025, item 6). Two separate fakes, one with the field and one
// without, since the gateway's probe answer is cached for an hour and
// the Gateway interface the loop holds does not expose a clock to force
// a re-probe.
func TestHeartbeatHoldingKinds(t *testing.T) {
	run := func(t *testing.T, noKinds bool) (sentKinds []any, hasKinds bool, summary string) {
		p := newPriorityFake(t)
		p.NoHoldingKinds = noKinds
		p.addGoal(orgID, "steady", "normal")
		p.addItem(orgID, "steady", "normal-1", 1, false)
		loop, _, _ := p.pacedLoop(4, reading(0.1, 0.1))
		loop.Cfg.Takes = Takes{{Kind: "evaluate"}}

		loop.Tick(context.Background())
		loop.Tick(context.Background())
		loop.wg.Wait()

		h := p.lastHolding()
		k, ok := h["kinds"]
		list, _ := k.([]any)
		s, _ := h["summary"].(string)
		return list, ok, s
	}

	want := "takes evaluate; "

	kinds, ok, summary := run(t, false)
	if !ok || len(kinds) != 1 || kinds[0] != "evaluate" {
		t.Fatalf("holding.kinds = %#v, ok=%v, want [evaluate]", kinds, ok)
	}
	if !strings.HasPrefix(summary, want) {
		t.Fatalf("summary = %q, want prefix %q", summary, want)
	}

	_, ok, summary = run(t, true)
	if ok {
		t.Fatalf("holding.kinds sent despite NoHoldingKinds")
	}
	if !strings.HasPrefix(summary, want) {
		t.Fatalf("summary = %q, want prefix %q", summary, want)
	}
}

// knowAccounts has the loop know accounts as the claim-time read issued
// them (Known): the only accounts it schedules on, since node.yaml names
// none. An account with no id gets one from its label.
func knowAccounts(l *Loop, accounts ...AccountConfig) {
	if l.Known == nil {
		l.Known = &KnownAccounts{}
	}
	for _, a := range accounts {
		if a.ID == "" {
			a.ID = "cred-" + a.Label
		}
		if err := l.Known.Note(a, l.now()); err != nil {
			panic(err)
		}
	}
}
