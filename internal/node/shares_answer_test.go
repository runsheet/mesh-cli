package node

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// sharesOrgA and sharesOrgB are two orgs an owner answer names, logins
// "a" and "b" so a weights map renders as the brief's "a 2, b 1".
const (
	sharesOrgA = "33333333-3333-4333-8333-333333333333"
	sharesOrgB = "77777777-7777-4777-8777-777777777777"
)

// sharesAnswer is a NodeAnswer naming orgs a and b with that Shares
// each, on one owner id.
func sharesAnswer(a, b int) *gateway.NodeAnswer {
	return &gateway.NodeAnswer{
		NodeID: "22222222-2222-4222-8222-222222222222",
		Owner:  gateway.NodeOwner{Kind: "person", ID: "44444444-4444-4444-8444-444444444444", Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{
			{OrgID: sharesOrgA, Login: "a", Shares: a},
			{OrgID: sharesOrgB, Login: "b", Shares: b},
		},
	}
}

// TestAnswerShares: TakeAnswer reads the owner answer's scopes' Shares
// into a weights map keyed by Scope.Name() when any scope carries a
// non-zero value, over node.yaml's shares: in Weights() — logged once
// on change as `shares (platform) <old> → <new>`, nothing when the
// answer repeats unchanged.
func TestAnswerShares(t *testing.T) {
	loop, buf := ownedLoop(t, sharesAnswer(0, 0))
	if got := loop.Weights(); got != nil {
		t.Fatalf("weights before any shares answer = %v, want nil (no node.yaml shares either)", got)
	}

	loop.TakeAnswer(sharesAnswer(2, 1))
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 2, "b": 1}) {
		t.Fatalf("weights after the answer = %v, want a:2 b:1", got)
	}
	if !strings.Contains(buf.String(), "shares (platform) none → a 2, b 1") {
		t.Fatalf("no `shares (platform) none → a 2, b 1` line:\n%s", buf)
	}

	before := buf.Len()
	loop.TakeAnswer(sharesAnswer(2, 1))
	if buf.Len() != before {
		t.Fatalf("an identical answer logged something: %q", buf.String()[before:])
	}

	loop.TakeAnswer(sharesAnswer(3, 1))
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 3, "b": 1}) {
		t.Fatalf("weights after the changed answer = %v, want a:3 b:1", got)
	}
	if !strings.Contains(buf.String()[before:], "shares (platform) a 2, b 1 → a 3, b 1") {
		t.Fatalf("no change line for the changed answer:\n%s", buf.String()[before:])
	}
}

// TestSharesPrecedence: Weights() takes the platform's shares over
// node.yaml's shares: when the answer carried any, falls back to
// node.yaml's otherwise, and an answer whose scopes are all Shares 0
// leaves node.yaml's weights in force with nothing logged. With both in
// force, `shares: the platform's weights are in force; node.yaml's
// shares: ignored` logs once, on the change into that state, not
// again on a repeat.
func TestSharesPrecedence(t *testing.T) {
	var buf bytes.Buffer
	cfg := ownedCfg()
	cfg.Shares = map[string]int{"a": 5, "b": 1}
	a := sharesAnswer(0, 0)
	scopes, err := ScopesFromAnswer(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Cfg: cfg, Scopes: scopes, Owned: &Owned{NodeID: a.NodeID, Owner: OwnerFrom(a)}, Log: log.New(&buf, "", 0)}

	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 5, "b": 1}) {
		t.Fatalf("weights with no platform shares yet = %v, want node.yaml's a:5 b:1", got)
	}

	loop.TakeAnswer(sharesAnswer(0, 0))
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 5, "b": 1}) {
		t.Fatalf("weights after an all-zero answer = %v, want node.yaml's a:5 b:1 still in force", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("an all-zero answer logged something: %q", buf.String())
	}

	loop.TakeAnswer(sharesAnswer(3, 1))
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 3, "b": 1}) {
		t.Fatalf("weights after the platform's shares = %v, want a:3 b:1, over node.yaml's", got)
	}
	if !strings.Contains(buf.String(), "shares (platform) none → a 3, b 1") {
		t.Fatalf("no `shares (platform)` line:\n%s", &buf)
	}
	if !strings.Contains(buf.String(), "shares: the platform's weights are in force; node.yaml's shares: ignored") {
		t.Fatalf("no `node.yaml's shares: ignored` line:\n%s", &buf)
	}

	before := buf.Len()
	loop.TakeAnswer(sharesAnswer(3, 1))
	if buf.Len() != before {
		t.Fatalf("a repeat with both in force logged something: %q", buf.String()[before:])
	}
}

// TestAnswerSharesClearsOnAllZero: once the platform's shares are in
// force, an answer whose scopes are all Shares 0 clears them — Weights()
// falls back to node.yaml's shares: again — logged the same way as a
// change, `shares (platform) <old> → none`; a further all-zero answer
// logs nothing more, since there is nothing left to clear.
func TestAnswerSharesClearsOnAllZero(t *testing.T) {
	var buf bytes.Buffer
	cfg := ownedCfg()
	cfg.Shares = map[string]int{"a": 5, "b": 1}
	a := sharesAnswer(0, 0)
	scopes, err := ScopesFromAnswer(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Cfg: cfg, Scopes: scopes, Owned: &Owned{NodeID: a.NodeID, Owner: OwnerFrom(a)}, Log: log.New(&buf, "", 0)}

	loop.TakeAnswer(sharesAnswer(3, 1))
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 3, "b": 1}) {
		t.Fatalf("weights after the platform's shares = %v, want a:3 b:1", got)
	}
	buf.Reset()

	loop.TakeAnswer(sharesAnswer(0, 0))
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 5, "b": 1}) {
		t.Fatalf("weights after the all-zero answer = %v, want node.yaml's a:5 b:1 back in force", got)
	}
	if !strings.Contains(buf.String(), "shares (platform) a 3, b 1 → none") {
		t.Fatalf("no `shares (platform) … → none` line clearing the override:\n%s", &buf)
	}

	before := buf.Len()
	loop.TakeAnswer(sharesAnswer(0, 0))
	if buf.Len() != before {
		t.Fatalf("a further all-zero answer logged something: %q", buf.String()[before:])
	}
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 5, "b": 1}) {
		t.Fatalf("weights after the second all-zero answer = %v, want node.yaml's a:5 b:1 still", got)
	}
}

// TestReloadUnchangedWithPlatformSharesInForce: once the platform's
// shares are in force, reloading an unchanged node.yaml (no shares:,
// or the same shares: as before) logs nothing on successive ticks — the
// reload compares node.yaml's own last value, not Weights()'s effective
// one, so the platform's override being different from node.yaml's is
// never mistaken for a node.yaml change.
func TestReloadUnchangedWithPlatformSharesInForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeNodeYAML(t, "slug: box\nname: Box\ncapacity: 1\nshares: {a: 1, b: 1}\n")

	var logs strings.Builder
	loop := &Loop{Cfg: &Config{Capacity: Fixed(1)}, Scopes: Scopes{{Login: "a", ID: "org-a"}, {Login: "b", ID: "org-b"}}, Log: log.New(&logs, "", 0)}
	loop.Reload = OwnedReloader(LoadConfig)

	loop.reload(context.Background())
	logs.Reset()

	loop.TakeAnswer(sharesAnswer(3, 1))
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 3, "b": 1}) {
		t.Fatalf("weights after the platform's shares = %v, want a:3 b:1", got)
	}
	logs.Reset()

	for i := 0; i < 3; i++ {
		loop.reload(context.Background())
		if got := logs.String(); got != "" {
			t.Fatalf("reload %d of an unchanged node.yaml logged %q, want nothing", i+1, got)
		}
		if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 3, "b": 1}) {
			t.Fatalf("weights after reload %d = %v, want the platform's a:3 b:1 still", i+1, got)
		}
	}
}

// TestClaimReadyPlatformShares is the end-to-end case: a fake answering
// shares acme:3 globex:1 on the heartbeat, capacity 4, both orgs with
// ready normal-priority work — the weights alone decide, so acme claims
// three items and globex one, the platform's shares taken at the first
// heartbeat of the tick, before claimReady runs.
func TestClaimReadyPlatformShares(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "fire", "normal")
	p.addGoal(org2, "steady", "normal")
	for i := 1; i <= 4; i++ {
		p.addItem(orgID, "fire", fmt.Sprintf("a-%d", i), i, false)
		p.addItem(org2, "steady", fmt.Sprintf("b-%d", i), i, false)
	}
	nodeID := "22222222-2222-4222-8222-222222222222"
	p.Nodes[nodeID] = map[string]any{"id": nodeID}
	p.Answer = &gateway.NodeAnswer{
		NodeID: nodeID,
		Owner:  gateway.NodeOwner{Kind: "person", ID: "44444444-4444-4444-8444-444444444444", Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{
			{OrgID: orgID, Login: "acme", AccountID: acctID, Shares: 3},
			{OrgID: org2, Login: "globex", AccountID: acct2, Shares: 1},
		},
	}
	var logs strings.Builder
	loop := &Loop{GW: gateway.New(p.URL(), "rs_test"), Cfg: &Config{Slug: "box", Capacity: Fixed(4)},
		Scopes: p.scopes, Owned: &Owned{NodeID: nodeID, Owner: Owner{Kind: "person", ID: "44444444-4444-4444-8444-444444444444", Name: "Chris"}},
		Interval: time.Hour, Harness: "claude-code", Log: log.New(&logs, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {}}

	loop.Tick(context.Background())
	loop.wg.Wait()

	byOrg := map[string]int{}
	for _, c := range p.CallsNamed("claimWorkItem") {
		byOrg[fmt.Sprint(p.Item(fmt.Sprint(c.Input["aggregateId"]))["orgId"])]++
	}
	if byOrg[orgID] != 3 || byOrg[org2] != 1 {
		t.Fatalf("claims by org %v, want acme 3 and globex 1\n%s", byOrg, &logs)
	}
	if !strings.Contains(logs.String(), "shares (platform) none → acme 3, globex 1") {
		t.Fatalf("no platform shares log line:\n%s", &logs)
	}
}
