package node

import (
	"context"
	"testing"
)

// On a gateway whose NodeAccountLimitsInput takes `priorities`, the
// heartbeat's limits carry the known account's priorities, and omit the
// key for one serving all (adr-005's amendment 2026-10,
// `limits[].priorities`).
func TestHeartbeatLimitsCarryPriorities(t *testing.T) {
	p := newPriorityFake(t)
	loop, _, _ := p.pacedLoop(4, reading(0.1, 0.1))
	loop.Limits.Observe("mesh - max20", reading(0.1, 0.1))
	knowAccounts(loop,
		AccountConfig{Label: "mesh - max20", Kind: AccountSubscription, Priorities: []string{"urgent", "high"}},
		AccountConfig{Label: DefaultAccount, Kind: AccountSubscription},
	)

	loop.Tick(context.Background())
	loop.wg.Wait()

	hb := p.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		t.Fatal("no heartbeat recorded")
	}
	limits, _ := hb[len(hb)-1].Input["limits"].([]any)
	byAccount := map[string]map[string]any{}
	for _, l := range limits {
		row := l.(map[string]any)
		byAccount[row["account"].(string)] = row
	}
	reserved, ok := byAccount["mesh - max20"]
	if !ok {
		t.Fatalf("heartbeat limits has no mesh - max20: %#v", limits)
	}
	pr, _ := reserved["priorities"].([]any)
	if len(pr) != 2 || pr[0] != "urgent" || pr[1] != "high" {
		t.Fatalf("mesh - max20 priorities = %#v, want [urgent high]", reserved["priorities"])
	}
	all, ok := byAccount[DefaultAccount]
	if !ok {
		t.Fatalf("heartbeat limits has no %s: %#v", DefaultAccount, limits)
	}
	if _, ok := all["priorities"]; ok {
		t.Fatalf("an all-serving account's entry carries priorities: %#v", all)
	}
}

// A gateway whose NodeAccountLimitsInput lacks `priorities` gets none:
// no limits entry carries the key, and the heartbeat is still recorded —
// an unknown field would otherwise refuse the whole heartbeat.
func TestHeartbeatLimitsOmitPrioritiesOnAnOlderPlatform(t *testing.T) {
	p := newPriorityFake(t)
	p.UnknownFields["priorities"] = true
	loop, _, _ := p.pacedLoop(4, reading(0.1, 0.1))
	knowAccounts(loop, AccountConfig{Label: DefaultAccount, Kind: AccountSubscription, Priorities: []string{"urgent", "high"}})

	loop.Tick(context.Background())
	loop.wg.Wait()

	hb := p.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		t.Fatal("no heartbeat recorded")
	}
	limits, _ := hb[len(hb)-1].Input["limits"].([]any)
	if len(limits) == 0 {
		t.Fatalf("no limits on the heartbeat: %#v", hb[len(hb)-1].Input)
	}
	for _, l := range limits {
		row := l.(map[string]any)
		if _, ok := row["priorities"]; ok {
			t.Fatalf("limits entry carries priorities on a platform without it: %#v", row)
		}
	}
}
