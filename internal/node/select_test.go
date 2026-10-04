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

// ChooseAccount, the pure choice, by headroom and the spending order.
func TestChooseAccount(t *testing.T) {
	maxA := AccountConfig{Label: "max-a", Kind: AccountSubscription}
	maxB := AccountConfig{Label: "max-b", Kind: AccountSubscription}
	maxC := AccountConfig{Label: "max-c", Kind: AccountSubscription}
	key := AccountConfig{Label: "key", Kind: AccountAPI}
	rejected := *reading(0.3, 0.3)
	rejected.Status, rejected.ResetsAt = "rejected", t0.Add(30*time.Minute)
	type readings = map[string]harness.RateLimit
	for _, c := range []struct {
		name      string
		accounts  []AccountConfig
		readings  readings
		priority  string
		workWaits bool
		want      string // "" is ok=false
	}{
		{"the seven-day window counts", []AccountConfig{maxA, maxB}, readings{"max-a": *reading(0.1, 0.7), "max-b": *reading(0.3, 0.4)}, "normal", true, "max-b"},
		{"never seen is full headroom", []AccountConfig{maxA, maxB}, readings{"max-a": *reading(0.1, 0.1)}, "normal", true, "max-b"},
		{"a tie goes to the first listed", []AccountConfig{maxA, maxB}, nil, "normal", true, "max-a"},
		{"past stop is skipped", []AccountConfig{maxA, maxB}, readings{"max-a": *reading(0.1, 0.99), "max-b": *reading(0.5, 0.5)}, "normal", true, "max-b"},
		{"three accounts at 10/50/80 with half the week gone pick the 10% one", []AccountConfig{maxA, maxB, maxC}, readings{"max-a": *readingHalfWeek(0.10), "max-b": *readingHalfWeek(0.50), "max-c": *readingHalfWeek(0.80)}, "normal", true, "max-a"},
		{"then the 50% one", []AccountConfig{maxB, maxC}, readings{"max-b": *readingHalfWeek(0.50), "max-c": *readingHalfWeek(0.80)}, "normal", true, "max-b"},
		{"an account ahead of its line is skipped while another is behind", []AccountConfig{maxA, maxB}, readings{"max-a": *readingHalfWeek(0.70), "max-b": *readingHalfWeek(0.30)}, "normal", true, "max-b"},
		{"only ahead-of-line accounts: the least ahead", []AccountConfig{maxA, maxB}, readings{"max-a": *readingHalfWeek(0.70), "max-b": *readingHalfWeek(0.90)}, "normal", true, "max-a"},
		{"rejected is skipped", []AccountConfig{maxA, maxB}, readings{"max-a": rejected, "max-b": *reading(0.5, 0.5)}, "urgent", true, "max-b"},
		{"paced takes no normal work", []AccountConfig{maxA}, readings{"max-a": *reading(0.85, 0.1)}, "normal", true, ""},
		{"paced takes urgent work", []AccountConfig{maxA}, readings{"max-a": *reading(0.85, 0.1)}, "urgent", true, "max-a"},
		{"normal work never gets the api while a subscription has headroom", []AccountConfig{key, maxA}, readings{"max-a": *reading(0.7, 0.7)}, "normal", true, "max-a"},
		{"low work never gets the api while a subscription is only paced", []AccountConfig{maxA, key}, readings{"max-a": *reading(0.85, 0.1)}, "low", true, ""},
		{"high work takes a subscription with headroom first", []AccountConfig{key, maxA}, readings{"max-a": *reading(0.5, 0.1)}, "high", true, "max-a"},
		{"high work takes the api over paced subscriptions", []AccountConfig{maxA, maxB, key}, readings{"max-a": *reading(0.85, 0.1), "max-b": *reading(0.9, 0.1)}, "high", true, "key"},
		{"urgent work takes the api when subscriptions are stopped", []AccountConfig{maxA, key}, readings{"max-a": *reading(0.99, 0.1)}, "urgent", false, "key"},
		{"all subscriptions exhausted and work waits: the api", []AccountConfig{maxA, maxB, key}, readings{"max-a": *reading(0.99, 0.1), "max-b": rejected}, "normal", true, "key"},
		{"all subscriptions exhausted, nothing waits: no api", []AccountConfig{maxA, maxB, key}, readings{"max-a": *reading(0.99, 0.1), "max-b": rejected}, "low", false, ""},
		{"only an api account, work waits", []AccountConfig{key}, nil, "normal", true, "key"},
		{"the api stopped too", []AccountConfig{maxA, key}, readings{"max-a": *reading(0.99, 0.1), "key": rejected}, "urgent", true, ""},
		{"nothing fits", []AccountConfig{maxA, maxB}, readings{"max-a": *reading(0.99, 0.1), "max-b": rejected}, "urgent", true, ""},
		{"no accounts", nil, nil, "urgent", true, ""},
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

// With no account to run on, the node does not claim: the item stays
// ready — not failed, not released — and the log says so once per tick.
// When an account frees up, the next tick claims the item and the
// session runs on that account's env.
func TestLoopClaimsNothingWithoutHeadroomThenClaimsOnTheFreedAccount(t *testing.T) {
	var accounts []AccountConfig
	for _, l := range []string{"max-a", "max-b"} {
		accounts = append(accounts, AccountConfig{Label: l, Kind: AccountSubscription})
	}
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "urgent")
	p.addItem(orgID, "g", "one", 1, false)
	p.addItem(orgID, "g", "two", 2, false)
	p.addPlanning(orgID, "planned", "normal")
	loop, logs, _ := p.pacedLoop(2, nil)
	knowAccounts(loop, accounts...)
	loop.Limits.Observe("max-a", reading(0.99, 0.1))
	rejected := reading(0.3, 0.3)
	rejected.Status = "rejected"
	loop.Limits.Observe("max-b", rejected)

	var mu sync.Mutex
	tokens := map[string]string{}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		a, _ := ChosenAccount(ctx)
		mu.Lock()
		tokens[item.ID] = a.Label
		mu.Unlock()
	}

	loop.Tick(context.Background())
	loop.Tick(context.Background())
	loop.wg.Wait()
	if n := len(p.CallsNamed("claimWorkItem")) + len(p.CallsNamed("claimPlan")); n != 0 {
		t.Fatalf("with no headroom the node claimed %d time(s)", n)
	}
	if n := len(p.CallsNamed("failWorkItem")) + len(p.CallsNamed("releaseWorkItem")); n != 0 {
		t.Fatalf("an unclaimed item was failed or released %d time(s)", n)
	}
	for _, id := range []string{"one", "two"} {
		if s := p.Item(id)["status"]; s != "ready" {
			t.Fatalf("%s = %v, want ready", id, s)
		}
	}
	const line = "accounts: no account has headroom for 2 item(s), 1 plan(s); judged max-a, max-b; left unclaimed until one has"
	if n := strings.Count(logs.String(), line); n != 2 {
		t.Fatalf("want the line once per tick (2), got %d:\n%s", n, logs)
	}

	// max-b's window resets and a session reads it fresh
	freed := reading(0.2, 0.3)
	freed.ObservedAt = t0.Add(time.Minute)
	loop.Limits.Observe("max-b", freed)
	claimed := p.claimedBy(loop)
	if strings.Join(claimed, ",") != "one,two" {
		t.Fatalf("claimed %v once max-b freed up\n%s", claimed, logs)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range claimed {
		if tokens[id] != "max-b" {
			t.Fatalf("item %s ran on %q, want max-b", id, tokens[id])
		}
	}
	if n := len(p.CallsNamed("claimPlan")); n != 1 {
		t.Fatalf("the plan claimed %d time(s) once max-b freed up", n)
	}
	if strings.Count(logs.String(), "accounts: no account has headroom") != 2 {
		t.Fatalf("a tick with headroom logs no wait:\n%s", logs)
	}
}

// An evaluation or a rebase of normal work takes a paced subscription
// before the api account — finishing work never moves it onto money —
// and falls to the api account only once every subscription is stopped.
func TestAccountToFinishKeepsTheSpendingOrder(t *testing.T) {
	maxA := AccountConfig{Label: "max-a", Kind: AccountSubscription}
	key := AccountConfig{Label: "key", Kind: AccountAPI}
	for _, c := range []struct {
		name string
		rl   *harness.RateLimit
		want string
		ok   bool
	}{
		{"fresh", reading(0.1, 0.1), "max-a", true},
		{"paced", reading(0.85, 0.3), "max-a", true},
		{"stopped", reading(0.3, 0.99), "key", true},
	} {
		loop := &Loop{Cfg: &Config{}, Limits: &Limits{}, Now: func() time.Time { return t0 }}
		knowAccounts(loop, maxA, key)
		loop.Limits.Observe("max-a", c.rl)
		ctx, ok := loop.accountToFinish(context.Background(), "normal", "evaluation")
		a, _ := ChosenAccount(ctx)
		if ok != c.ok || a.Label != c.want {
			t.Errorf("%s: chose %q (%v), want %q (%v)", c.name, a.Label, ok, c.want, c.ok)
		}
	}
	// nothing but a stopped subscription: nothing
	loop := &Loop{Cfg: &Config{}, Limits: &Limits{}, Now: func() time.Time { return t0 }}
	knowAccounts(loop, maxA)
	loop.Limits.Observe("max-a", reading(0.3, 0.99))
	if _, ok := loop.accountToFinish(context.Background(), "normal", "evaluation"); ok {
		t.Error("a stopped subscription took an evaluation")
	}
}
