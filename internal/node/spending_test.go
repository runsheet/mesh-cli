package node

import (
	"context"
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
	s.loop.Cfg.Accounts = []AccountConfig{
		{Label: "max-a", Kind: AccountSubscription, TokenFile: "/a"},
		{Label: "max-b", Kind: AccountSubscription, TokenFile: "/b"},
		{Label: "key", Kind: AccountAPI, APIKeyEnv: "KEY", DailyCapUSD: 300},
	}
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
	if got, _ := n.loop.Scaling(); got != (Scaling{Capacity: 5, Min: 1, Max: 8, Reason: ReasonAccounts}) {
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
			n.loop.Cfg.Accounts = n.loop.Cfg.Accounts[2:]
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
