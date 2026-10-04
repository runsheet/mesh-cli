package node

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Weights() is node.yaml's Shares, a copy so a caller cannot mutate the
// loop's own; a reloaded value (sharesSet) takes it over Cfg's, and
// with neither it is nil — equal shares (ADR-023).
func TestWeightsFromConfig(t *testing.T) {
	loop := &Loop{Cfg: &Config{Shares: map[string]int{"a": 3, "b": 1}}}
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 3, "b": 1}) {
		t.Fatalf("weights from Cfg = %v, want a:3 b:1", got)
	}
	got := loop.Weights()
	got["a"] = 99
	if again := loop.Weights(); again["a"] != 3 {
		t.Fatal("Weights handed out a live map, not a copy")
	}

	loop.shares, loop.sharesSet = map[string]int{"a": 5}, true
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 5}) {
		t.Fatalf("weights after reload = %v, want a:5 alone, over Cfg's", got)
	}

	empty := &Loop{}
	if got := empty.Weights(); got != nil {
		t.Fatalf("weights with no Cfg and no reload = %v, want nil", got)
	}
}

func TestFloors(t *testing.T) {
	two := Scopes{{Login: "a", ID: "org-a"}, {Slug: "b", ID: "org-b"}}
	cases := []struct {
		name     string
		capacity int
		scopes   Scopes
		weights  map[string]int
		a, b     int
	}{
		{"equal shares", 4, two, nil, 2, 2},
		{"weights 3:1", 4, two, map[string]int{"a": 3, "b": 1}, 3, 1},
		{"capacity 1", 1, two, nil, 1, 0},
		{"capacity 5: the remainder to the first listed", 5, two, nil, 3, 2},
		{"a zero or missing weight is 1", 4, two, map[string]int{"a": 0}, 2, 2},
		{"weights by slug when no login", 4, two, map[string]int{"b": 3}, 1, 3},
		{"capacity 0", 0, two, nil, 0, 0},
	}
	for _, c := range cases {
		got := Floors(c.capacity, c.scopes, c.weights)
		if got["org-a"] != c.a || got["org-b"] != c.b || len(got) != 2 {
			t.Errorf("%s: floors %v, want a %d b %d", c.name, got, c.a, c.b)
		}
	}
	// three orgs, capacity 4: 2, 1, 1 — they sum to the capacity
	three := append(Scopes{}, two...)
	three = append(three, Scope{Login: "c", ID: "org-c"})
	got := Floors(4, three, nil)
	if got["org-a"] != 2 || got["org-b"] != 1 || got["org-c"] != 1 {
		t.Errorf("three orgs: floors %v", got)
	}
	// largest remainder: weights 1:1:3 of capacity 3 are 0.6, 0.6 and
	// 1.8 — 0, 0, 1 rounded down, then c's .8 and a's .6, listed first
	got = Floors(3, three, map[string]int{"c": 3})
	if got["org-a"] != 1 || got["org-b"] != 0 || got["org-c"] != 2 {
		t.Errorf("weights 1:1:3 of 3: floors %v", got)
	}
}

func TestShareOrder(t *testing.T) {
	a, b := Scope{Login: "a", ID: "org-a"}, Scope{Login: "b", ID: "org-b"}
	item := func(sc Scope, id string, seq, rank int) readyItem {
		return readyItem{item: gateway.WorkItem{ID: id, Seq: seq, OrgID: sc.ID}, scope: sc, rank: rank}
	}
	urgent, normal := gateway.PriorityRank("urgent"), gateway.PriorityRank("normal")
	ready := []readyItem{
		item(a, "a1", 1, urgent), item(a, "a2", 2, urgent), item(a, "a3", 3, urgent), item(a, "a4", 4, urgent),
		item(b, "b1", 1, normal), item(b, "b2", 2, normal),
	}
	ids := func(rs []readyItem) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.item.ID)
		}
		return strings.Join(out, ",")
	}
	floors := map[string]int{"org-a": 2, "org-b": 2}
	// the floors first, the org furthest below first and a in listed
	// order on a tie, then priority
	got := shareOrder(ready, floors, nil)
	if ids(got) != "a1,b1,a2,b2,a3,a4" {
		t.Fatalf("order %s", ids(got))
	}
	for _, r := range got[:4] {
		if r.floor != 2 {
			t.Fatalf("%s taken for a floor carries floor %d", r.item.ID, r.floor)
		}
	}
	if got[1].held != 0 || got[3].held != 1 || got[4].floor != 0 {
		t.Fatalf("held %d, %d; spare floor %d", got[1].held, got[3].held, got[4].floor)
	}
	// b already holds two: priority alone
	if got := ids(shareOrder(ready, floors, map[string]int{"org-b": 2})); got != "a1,a2,a3,a4,b1,b2" {
		t.Fatalf("b at its floor: order %s", got)
	}
	// a holds one: b is further below and goes first
	if got := ids(shareOrder(ready, floors, map[string]int{"org-a": 1})); got != "b1,a1,b2,a2,a3,a4" {
		t.Fatalf("a holding one: order %s", got)
	}
	// b has nothing ready: its floor is not a ceiling on a
	if got := ids(shareOrder(ready[:4], floors, nil)); got != "a1,a2,a3,a4" {
		t.Fatalf("b with nothing ready: order %s", got)
	}
	if ready[0].item.ID != "a1" || ready[4].item.ID != "b1" {
		t.Fatal("shareOrder changed its input")
	}
}

// sharesFake is priorityFake with acme (A) holding four ready items of
// an urgent goal and globex (B) two of a normal one.
func sharesFake(t *testing.T, bReady bool) *priorityFake {
	p := newPriorityFake(t)
	p.addGoal(orgID, "fire", "urgent")
	p.addGoal(org2, "steady", "normal")
	for i := 1; i <= 4; i++ {
		p.addItem(orgID, "fire", fmt.Sprintf("a-%d", i), i, false)
	}
	if bReady {
		p.addItem(org2, "steady", "b-1", 1, false)
		p.addItem(org2, "steady", "b-2", 2, false)
	}
	return p
}

// sharesLoop is a loop over both orgs at that capacity, logging into the
// returned buffer.
func (p *priorityFake) sharesLoop(capacity int) (*Loop, *strings.Builder) {
	var logs strings.Builder
	return &Loop{GW: gateway.New(p.URL(), "rs_test"), Cfg: &Config{Slug: "box", Capacity: Fixed(capacity)}, Scopes: p.scopes,
		Interval: time.Hour, Harness: "claude-code", Log: log.New(&logs, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {}}, &logs
}

// claimsByOrg runs one tick of loop and counts the claims the fake was
// sent per org.
func (p *priorityFake) claimsByOrg(loop *Loop) map[string]int {
	p.t.Helper()
	before := len(p.CallsNamed("claimWorkItem"))
	loop.Tick(context.Background())
	loop.wg.Wait()
	byOrg := map[string]int{}
	for _, c := range p.CallsNamed("claimWorkItem")[before:] {
		byOrg[fmt.Sprint(p.Item(fmt.Sprint(c.Input["aggregateId"]))["orgId"])]++
	}
	return byOrg
}

// Clause 1: two orgs, capacity 4, A's items urgent and B's normal: B
// below its floor of 2 is claimed for twice, whatever the priority, and
// A twice.
func TestClaimReadySharesFloorsBeforePriority(t *testing.T) {
	p := sharesFake(t, true)
	loop, logs := p.sharesLoop(4)
	if got := p.claimsByOrg(loop); got[orgID] != 2 || got[org2] != 2 {
		t.Fatalf("claims by org %v, want A 2 and B 2\n%s", got, logs)
	}
	if !strings.Contains(logs.String(), `globex: claimed item 1 "b-1" (below its floor of 2, holding 0)`) ||
		!strings.Contains(logs.String(), `globex: claimed item 2 "b-2" (below its floor of 2, holding 1)`) {
		t.Fatalf("no floor line:\n%s", logs)
	}
}

// Clause 2: A urgent, B nothing ready: B's floor is not a ceiling, and
// the spare slots go to priority order — A is claimed four times.
func TestClaimReadySharesSpareGoesToPriority(t *testing.T) {
	p := sharesFake(t, false)
	loop, logs := p.sharesLoop(4)
	if got := p.claimsByOrg(loop); got[orgID] != 4 || got[org2] != 0 {
		t.Fatalf("claims by org %v, want A 4\n%s", got, logs)
	}
}

// Clause 3: B already runs a plan and a research session at capacity 4
// over two orgs: they are B's held, B is at its floor, and A gets every
// free slot. Plans and research do not count against capacity, so all
// four are free.
func TestClaimReadySharesCountPlansAndResearch(t *testing.T) {
	p := sharesFake(t, true)
	loop, logs := p.sharesLoop(4)
	loop.planning = map[string]bool{"b-goal": true}
	loop.researching = map[string]bool{"b-direction": true}
	loop.sessionOrg = map[string]string{"b-goal": org2, "b-direction": org2}
	if got := loop.heldByOrg(); got[org2] != 2 || got[orgID] != 0 {
		t.Fatalf("held by org %v", got)
	}
	if got := p.claimsByOrg(loop); got[orgID] != 4 || got[org2] != 0 {
		t.Fatalf("claims by org %v, want A 4 and B none\n%s", got, logs)
	}
}

// An evaluation (or a rebase) is no session of the org's share: B
// evaluating two items still gets its floor of 2.
func TestClaimReadySharesEvaluationsDoNotCount(t *testing.T) {
	p := sharesFake(t, true)
	loop, logs := p.sharesLoop(4)
	loop.evaluating = map[string]bool{"b-eval": true}
	loop.rebasing = map[string]bool{"b-rebase": true}
	loop.sessionOrg = map[string]string{"b-eval": org2, "b-rebase": org2}
	if got := loop.heldByOrg(); len(got) != 0 {
		t.Fatalf("held by org %v, want none", got)
	}
	if got := p.claimsByOrg(loop); got[orgID] != 2 || got[org2] != 2 {
		t.Fatalf("claims by org %v, want A 2 and B 2\n%s", got, logs)
	}
}

// node.yaml's `shares:` sets the floors' weights: two orgs, capacity 4,
// shares acme:3 globex:1, each with four ready items of a normal goal —
// no priority difference, so the weights alone decide: acme claims
// three, globex one. The shares are loaded from a real node.yaml
// through the loop's own reload path (Tick → reload), not set on Cfg
// directly, so the wiring from disk to Weights() to claimOrder is
// proved end to end.
func TestClaimReadySharesWeightsFromNodeYAML(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "fire", "normal")
	p.addGoal(org2, "steady", "normal")
	for i := 1; i <= 4; i++ {
		p.addItem(orgID, "fire", fmt.Sprintf("a-%d", i), i, false)
		p.addItem(org2, "steady", fmt.Sprintf("b-%d", i), i, false)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	writeNodeYAML(t, "slug: box\nname: Box\ncapacity: 4\nshares: {acme: 3, globex: 1}\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}

	var logs strings.Builder
	loop := &Loop{GW: gateway.New(p.URL(), "rs_test"), Cfg: cfg, Scopes: p.scopes,
		Interval: time.Hour, Harness: "claude-code", Log: log.New(&logs, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {}}
	loop.Reload = Reloader(loop.GW, cfg, p.scopes, LoadConfig, func(context.Context, Scope) error { return nil })

	if got := p.claimsByOrg(loop); got[orgID] != 3 || got[org2] != 1 {
		t.Fatalf("claims by org %v, want acme 3 and globex 1\n%s", got, &logs)
	}
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"acme": 3, "globex": 1}) {
		t.Fatalf("weights after the reload %v, want acme:3 globex:1", got)
	}
}

// A build counts toward its org from the claim to its end.
func TestClaimReadySharesCountBuilds(t *testing.T) {
	p := sharesFake(t, true)
	release := make(chan struct{})
	loop, logs := p.sharesLoop(4)
	loop.Dispatch = func(context.Context, gateway.WorkItem) { <-release }
	loop.Tick(context.Background())
	if got := loop.heldByOrg(); got[orgID] != 2 || got[org2] != 2 {
		close(release)
		t.Fatalf("held by org while building %v\n%s", got, logs)
	}
	close(release)
	loop.wg.Wait()
	if got := loop.heldByOrg(); len(got) != 0 || len(loop.sessionOrg) != 0 {
		t.Fatalf("held by org after the builds %v, sessionOrg %v", got, loop.sessionOrg)
	}
}

// A tick tells Shares each org's floor, held and ready, and node.shares
// written by RunState.WriteShares reads back the same rows (ADR-023,
// Reporting); Stopped removes it.
func TestWriteShares(t *testing.T) {
	p := sharesFake(t, true)
	release := make(chan struct{})
	loop, logs := p.sharesLoop(4)
	loop.Dispatch = func(context.Context, gateway.WorkItem) { <-release }
	state := RunState{Dir: t.TempDir()}
	if _, ok := state.Shares(); ok {
		t.Fatal("shares before any tick")
	}
	loop.Shares = func(rows []HoldingOrg) {
		if err := state.WriteShares(rows); err != nil {
			t.Fatal(err)
		}
	}
	loop.Tick(context.Background())
	close(release)
	loop.wg.Wait()
	rows, ok := state.Shares()
	if !ok || len(rows) != 2 {
		t.Fatalf("shares = %+v, %v\n%s", rows, ok, logs)
	}
	want := []HoldingOrg{
		{Org: p.scopes[0].Name(), OrgID: orgID, Floor: 2, Held: 2, Ready: 4},
		{Org: p.scopes[1].Name(), OrgID: org2, Floor: 2, Held: 2, Ready: 2},
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("shares[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
	if h := loop.Holding(); h == nil || len(h.Orgs) != 2 || h.Orgs[1] != want[1] {
		t.Fatalf("holding orgs = %+v", h)
	}
	state.Stopped(0)
	if _, ok := state.Shares(); ok {
		t.Fatal("node.shares left after Stopped")
	}
}
