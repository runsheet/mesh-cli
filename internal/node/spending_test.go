package node

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// fiveHourReading is a subscription's reading with only its five-hour window:
// used, resetting at t0 + resets.
func fiveHourReading(used float64, resets time.Duration, at time.Time) *harness.RateLimit {
	return &harness.RateLimit{Status: "allowed", FiveHour: harness.RateLimitWindow{Utilization: used, ResetsAt: t0.Add(resets)}, ObservedAt: at}
}

// oneSlot is a five-hour window with room for one slot at the default
// burn for the next half hour: 0.6 of room over 150 minutes.
func oneSlot(at time.Time) *harness.RateLimit { return fiveHourReading(0.3, 150*time.Minute, at) }

// overTarget is a five-hour window past its 0.90 target but short of
// the stop: the subscription affords no slot, and is paced.
func overTarget(at time.Time) *harness.RateLimit { return fiveHourReading(0.92, 150*time.Minute, at) }

// accountsNode is a ranged node on two subscriptions and one api key,
// its builds blocking and each noting the account it was handed.
type accountsNode struct {
	*scaled
	mu sync.Mutex
	on map[string]string
}

// The key's $300 day at 14:00 UTC is $30 an hour, three slots at the
// default $10 a slot-hour: two normal and the last tenth's one for
// urgent and high work.
func newAccountsNode(t *testing.T, c Capacity) *accountsNode {
	s := newScaled(t, c, true)
	n := &accountsNode{scaled: s, on: map[string]string{}}
	knowAccounts(s.loop, []AccountConfig{
		{Label: "max-a", Kind: AccountSubscription},
		{Label: "max-b", Kind: AccountSubscription},
		{Label: "key", Kind: AccountAPI, DailyCapUSD: 300},
	}...)
	s.loop.Ledger = &Ledger{Location: time.UTC, Now: s.clock.Now}
	s.loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		a, _ := ChosenAccount(ctx)
		n.mu.Lock()
		n.on[item.ID] = a.Label
		n.mu.Unlock()
		<-s.release
		if ctx.Err() != nil {
			s.cancelled.Add(1)
		}
	}
	return n
}

func (n *accountsNode) subscriptions(r func(time.Time) *harness.RateLimit, at time.Duration) {
	for _, l := range []string{"max-a", "max-b"} {
		n.loop.limitsStore().Observe(l, r(t0.Add(at)))
	}
}

func (n *accountsNode) accountOf(id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.on[id]
}

// Two subscriptions and a key: the node grows one step per 5 minutes to
// what they afford together (1 + 1 + 3), and each account's slots are
// what the heartbeat will carry. A normal item takes a subscription; a
// normal item waiting once the subscriptions' slots are full takes the
// key's normal slots, never its reserve; an urgent item takes the
// reserve. A shrink — the subscriptions past their target and the key
// at its cap — stops no running session, and a key at its cap takes
// nothing more, not even urgent work.
func TestAccountsNodeGrowsToTheSumAndSpendsInOrder(t *testing.T) {
	n := newAccountsNode(t, Capacity{Min: 1, Max: 8})
	n.subscriptions(oneSlot, 0)
	stepsOf(t, n.capacityEvery(0, 25*time.Minute), func(d time.Duration) int {
		return min(1+int(d/(5*time.Minute)), 5)
	})
	if got, _ := n.loop.Scaling(); bare(got) != (Scaling{Capacity: 5, Min: 1, Max: 8, Reason: ReasonAccounts}) {
		t.Fatalf("scaling = %+v", got)
	}
	slots, _ := n.loop.AccountSlots()
	want := []AccountSlots{
		{Name: "max-a", Kind: AccountSubscription, Slots: Slots{Normal: 1}, Reason: ReasonFiveHourPace},
		{Name: "max-b", Kind: AccountSubscription, Slots: Slots{Normal: 1}, Reason: ReasonFiveHourPace},
		{Name: "key", Kind: AccountAPI, Slots: Slots{Normal: 2, UrgentOnly: 1}, Reason: ReasonDailyCap},
	}
	if len(slots) != len(want) {
		t.Fatalf("account slots %+v", slots)
	}
	for i := range want {
		if slots[i] != want[i] {
			t.Fatalf("account slots[%d] = %+v, want %+v", i, slots[i], want[i])
		}
	}
	if !strings.Contains(n.logs.String(), "capacity 1–8: scaling by what its 3 account(s) afford from 1") {
		t.Fatalf("no scaling line:\n%s", n.logs)
	}

	// five normal items wait; one scaling minute has passed with none running
	n.addGoal(orgID, "steady", "normal")
	for i, id := range []string{"n1", "n2", "n3", "n4", "n5"} {
		n.addItem(orgID, "steady", id, i+1, false)
	}
	got := n.at(26 * time.Minute)
	if strings.Join(got, ",") != "n1,n2,n3,n4" {
		t.Fatalf("claimed %v, want n1..n4 (the fifth slot is the key's reserve)", got)
	}
	waitFor(t, func() bool { return n.accountOf("n4") != "" })
	for id, want := range map[string]string{"n1": "max-a", "n2": "max-b", "n3": "key", "n4": "key"} {
		if on := n.accountOf(id); on != want {
			t.Fatalf("%s ran on %q, want %s", id, on, want)
		}
	}
	if n.Item("n5")["status"] != "ready" {
		t.Fatalf("n5 = %v, want ready: the reserve is not for normal work", n.Item("n5")["status"])
	}

	// an urgent item takes the key's reserve slot
	n.addGoal(orgID, "fire", "urgent")
	n.addItem(orgID, "fire", "u1", 1, false)
	if got := n.at(26*time.Minute + 30*time.Second); strings.Join(got, ",") != "u1" {
		t.Fatalf("claimed %v, want u1", got)
	}
	waitFor(t, func() bool { return n.accountOf("u1") != "" })
	if on := n.accountOf("u1"); on != "key" {
		t.Fatalf("u1 ran on %q, want key", on)
	}

	// the subscriptions pass their target and the key reaches its cap:
	// the target falls to min, the capacity steps down, nothing is cancelled
	n.subscriptions(overTarget, 27*time.Minute)
	if err := n.loop.Ledger.Record("key", 300); err != nil {
		t.Fatal(err)
	}
	n.addItem(orgID, "fire", "u2", 2, false)
	for d := 27 * time.Minute; d <= 50*time.Minute; d += time.Minute {
		if got := n.at(d); len(got) != 0 {
			t.Fatalf("at +%s claimed %v at capacity %d", d, got, n.loop.Capacity())
		}
	}
	if n.loop.Capacity() != 1 || n.loop.Running() != 5 {
		t.Fatalf("capacity %d, running %d; want 1 and 5", n.loop.Capacity(), n.loop.Running())
	}
	if n.cancelled.Load() != 0 {
		t.Fatalf("%d running session(s) cancelled by the shrink", n.cancelled.Load())
	}
	for range 5 {
		n.finishOne()
	}
	// capacity 1, nothing running, u2 urgent: the subscriptions have no
	// slot and the key is at its cap, so u2 waits
	if got := n.at(51 * time.Minute); len(got) != 0 {
		t.Fatalf("with every account spent, claimed %v", got)
	}
	if !strings.Contains(n.logs.String(), "accounts: no account has headroom for 1 item(s)") {
		t.Fatalf("no waiting line:\n%s", n.logs)
	}
}

// The subscriptions at 0 slots and an item waiting: a normal item takes
// the key. With a slot left on a paced subscription, a normal item
// waits — the key is only for urgent and high work then.
func TestAccountsKeyTakesNormalWorkOnlyWhenSubscriptionsHaveNoSlot(t *testing.T) {
	t.Run("subscriptions at 0 slots", func(t *testing.T) {
		n := newAccountsNode(t, Capacity{Min: 1, Max: 4})
		n.subscriptions(overTarget, 0)
		n.at(0)
		n.addGoal(orgID, "steady", "normal")
		n.addItem(orgID, "steady", "n1", 1, false)
		if got := n.at(5 * time.Minute); strings.Join(got, ",") != "n1" {
			t.Fatalf("claimed %v at capacity %d, want n1", got, n.loop.Capacity())
		}
		waitFor(t, func() bool { return n.accountOf("n1") != "" })
		if on := n.accountOf("n1"); on != "key" {
			t.Fatalf("n1 ran on %q, want key", on)
		}
	})
	t.Run("a paced subscription with a slot", func(t *testing.T) {
		n := newAccountsNode(t, Capacity{Min: 1, Max: 4})
		// 0.05 of room over 10 minutes: one slot, and paced (past 0.80)
		n.subscriptions(func(at time.Time) *harness.RateLimit { return fiveHourReading(0.85, 10*time.Minute, at) }, 0)
		n.at(0)
		n.addGoal(orgID, "steady", "normal")
		n.addGoal(orgID, "hot", "high")
		n.addItem(orgID, "steady", "n1", 1, false)
		n.addItem(orgID, "hot", "h1", 1, false)
		if got := n.at(5 * time.Minute); strings.Join(got, ",") != "h1" {
			t.Fatalf("claimed %v at capacity %d, want h1 only", got, n.loop.Capacity())
		}
		waitFor(t, func() bool { return n.accountOf("h1") != "" })
		if on := n.accountOf("h1"); on != "key" {
			t.Fatalf("h1 ran on %q, want key", on)
		}
		if n.Item("n1")["status"] != "ready" {
			t.Fatalf("n1 = %v, want ready", n.Item("n1")["status"])
		}
	})
}

// A key at its daily cap — or its monthly — takes nothing, urgent work
// included, even when it is the only account.
func TestAccountsKeyAtItsCapTakesNothing(t *testing.T) {
	for name, spend := range map[string]func(*Ledger){
		"daily":   func(l *Ledger) { _ = l.Record("key", 300) },
		"monthly": func(l *Ledger) { l.mu.Lock(); l.entry("key").MonthUSD = 1000; l.mu.Unlock() },
	} {
		t.Run(name, func(t *testing.T) {
			n := newAccountsNode(t, Capacity{Min: 1, Max: 4})
			n.loop.Known = nil
			knowAccounts(n.loop, AccountConfig{Label: "key", Kind: AccountAPI, DailyCapUSD: 300})
			spend(n.loop.Ledger)
			n.addGoal(orgID, "fire", "urgent")
			n.addItem(orgID, "fire", "u1", 1, false)
			for d := time.Duration(0); d <= 10*time.Minute; d += time.Minute {
				if got := n.at(d); len(got) != 0 {
					t.Fatalf("a key at its cap took %v", got)
				}
			}
		})
	}
}

// Without accounts the spending order is what it was: nothing filtered.
func TestSpendableWithoutAccounts(t *testing.T) {
	l := &Loop{Cfg: &Config{}}
	own := []AccountConfig{{Label: DefaultAccount, Kind: AccountSubscription}}
	if got := l.spendable(own, false, true); len(got) != 1 {
		t.Fatalf("spendable = %+v", got)
	}
}

// lastScaling is the last heartbeat's scaling, and whether it had one.
func lastScaling(t *testing.T, s *scaled) (map[string]any, bool) {
	t.Helper()
	hb := s.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		t.Fatal("no heartbeat")
	}
	v, ok := hb[len(hb)-1].Input["scaling"]
	m, _ := v.(map[string]any)
	return m, ok
}

// A ranged node on accounts heartbeats its capacity, range and reason
// and one entry per account with its slots and reason — names and
// kinds, never a token file or a key's variable. A gateway whose schema
// lacks scaling gets none, and the heartbeat still lands.
func TestAccountsNodeHeartbeatsEachAccountsSlots(t *testing.T) {
	n := newAccountsNode(t, Capacity{Min: 1, Max: 8})
	n.subscriptions(oneSlot, 0)
	n.capacityEvery(0, 25*time.Minute)
	s, ok := lastScaling(t, n.scaled)
	if !ok || s["capacity"] != 5.0 || s["min"] != 1.0 || s["max"] != 8.0 || s["reason"] != "accounts" {
		t.Fatalf("scaling = %#v, %v", s, ok)
	}
	accounts, _ := s["accounts"].([]any)
	want := []map[string]any{
		{"name": "max-a", "kind": "subscription", "slots": 1.0, "urgentOnlySlots": 0.0, "reason": "five-hour pace"},
		{"name": "max-b", "kind": "subscription", "slots": 1.0, "urgentOnlySlots": 0.0, "reason": "five-hour pace"},
		{"name": "key", "kind": "api", "slots": 2.0, "urgentOnlySlots": 1.0, "reason": "daily cap"},
	}
	if len(accounts) != len(want) {
		t.Fatalf("scaling.accounts = %#v", s["accounts"])
	}
	for i, w := range want {
		a, _ := accounts[i].(map[string]any)
		if len(a) != len(w) {
			t.Fatalf("accounts[%d] = %#v, want %#v", i, a, w)
		}
		for k, v := range w {
			if a[k] != v {
				t.Fatalf("accounts[%d].%s = %#v, want %#v", i, k, a[k], v)
			}
		}
	}
	hb := n.CallsNamed("heartbeatNode")
	raw, _ := json.Marshal(hb[len(hb)-1].Input)
	for _, secret := range []string{`"/a"`, `"/b"`, "KEY", "token_file", "api_key_env", "tokenFile", "apiKeyEnv"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("heartbeat carries %s: %s", secret, raw)
		}
	}

	old := newAccountsNode(t, Capacity{Min: 1, Max: 8})
	old.UnknownFields["scaling"] = true
	old.subscriptions(oneSlot, 0)
	old.at(0)
	old.at(time.Minute)
	if len(old.CallsNamed("heartbeatNode")) != 4 { // two ticks, two orgs
		t.Fatalf("heartbeats landed: %d, want 4", len(old.CallsNamed("heartbeatNode")))
	}
	if s, ok := lastScaling(t, old.scaled); ok {
		t.Fatalf("scaling sent to a schema without it: %#v", s)
	}
}

// A fixed node heartbeats its capacity as its min and max, reason
// `fixed`, on several accounts and on one alike — no scaler counts its
// accounts' slots (ADR-018: before it, a fixed node on one account sent
// none, and a capacity changed in node.yaml never reached the platform).
func TestFixedNodeHeartbeatsItsCapacityOnAnyNumberOfAccounts(t *testing.T) {
	two := newScaled(t, Fixed(3), false)
	knowAccounts(two.loop, []AccountConfig{
		{Label: "max-a", Kind: AccountSubscription},
		{Label: "max-b", Kind: AccountSubscription},
	}...)
	two.at(0)
	s, ok := lastScaling(t, two)
	// accounts rides as [] (the platform refuses a null list, 2026-09-28)
	if accts, _ := s["accounts"].([]any); !ok || len(s) != 5 || s["capacity"] != 3.0 || s["min"] != 3.0 || s["max"] != 3.0 || s["reason"] != "fixed" || accts == nil || len(accts) != 0 {
		t.Fatalf("fixed node on two accounts: scaling = %#v, %v", s, ok)
	}

	one := newScaled(t, Fixed(3), false)
	knowAccounts(one.loop, []AccountConfig{{Label: "max-a", Kind: AccountSubscription}}...)
	one.at(0)
	if s, ok := lastScaling(t, one); !ok || len(s) != 5 || s["capacity"] != 3.0 || s["min"] != 3.0 || s["max"] != 3.0 || s["reason"] != "fixed" {
		t.Fatalf("fixed node on one account: scaling = %#v, %v", s, ok)
	}
}

// The whole heartbeat of a fixed node on the operator's own login, one
// account: version, running and scaling {capacity, min, max, reason,
// accounts} with its capacity — nothing else rides it, and a gateway
// whose schema lacks scaling gets the heartbeat without it.
func TestFixedSingleAccountHeartbeatBody(t *testing.T) {
	n := newScaled(t, Fixed(3), false)
	n.loop.Version = "v0.2.203"
	n.at(0)
	hb := n.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		t.Fatal("no heartbeat")
	}
	in := hb[len(hb)-1].Input
	delete(in, "nodeId")
	delete(in, "aggregateId")
	got, _ := json.Marshal(in)
	const want = `{"namespace":"runsheet","running":0,"scaling":{"accounts":[],"capacity":3,"max":3,"min":3,"reason":"fixed"},"version":"v0.2.203"}`
	if string(got) != want {
		t.Fatalf("heartbeat body =\n%s\nwant\n%s", got, want)
	}

	old := newScaled(t, Fixed(3), false)
	old.UnknownFields["scaling"] = true
	old.at(0)
	if s, ok := lastScaling(t, old); ok {
		t.Fatalf("scaling sent to a schema without it: %#v", s)
	}
}
