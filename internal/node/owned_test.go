package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// These tests read the owner contract without a socket: Register is
// driven through a Registrar of recording functions and TakeAnswer with
// the contract fixtures' answers (specs/contracts/*.json). The
// loop is driven through Tick against ownedGW, an in-process Gateway.

// steps is a Registrar that records what Register did, in order:
// "probe", "resolve", "any org", "owner <nodeID>", "org <name>" per
// legacy registration. resolveErr is what Resolve refuses with (an orgs
// restriction the person's orgs cannot meet).
type steps struct {
	takes      bool
	answer     *gateway.NodeAnswer
	scopes     Scopes
	resolveErr error
	did        []string
}

func (s *steps) registrar() Registrar {
	return Registrar{
		TakesOwner: func(context.Context) bool {
			s.did = append(s.did, "probe")
			return s.takes
		},
		Resolve: func(context.Context) (Scopes, error) {
			s.did = append(s.did, "resolve")
			if s.resolveErr != nil {
				return nil, s.resolveErr
			}
			return s.scopes, nil
		},
		AnyOrg: func(context.Context) (string, error) {
			s.did = append(s.did, "any org")
			return "33333333-3333-4333-8333-333333333333", nil
		},
		RegisterOwner: func(_ context.Context, nodeID, orgID string) (*gateway.NodeAnswer, error) {
			s.did = append(s.did, "owner "+nodeID)
			return s.answer, nil
		},
		RegisterIn: func(_ context.Context, scopes Scopes) error {
			for _, sc := range scopes {
				s.did = append(s.did, "org "+sc.Name())
			}
			return nil
		},
	}
}

// the person's orgs as MyOrgs lists them, per-org ids from NodeIDIn
func personScopes(t *testing.T, cfg *Config) Scopes {
	t.Helper()
	var out Scopes
	for _, o := range []struct{ id, login string }{{"33333333-3333-4333-8333-333333333333", "runsheet"}, {"77777777-7777-4777-8777-777777777777", "wardroomid"}} {
		id, err := cfg.NodeIDIn(o.id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, Scope{Login: o.login, Slug: o.login, ID: o.id, NodeID: id})
	}
	return out
}

const mini = "Chris's Mini"

func ownedCfg() *Config {
	return &Config{Slug: "mini", Name: mini, OwnerID: "44444444-4444-4444-8444-444444444444", NodeID: "22222222-2222-4222-8222-222222222222"}
}

func ownedLoop(t *testing.T, a *gateway.NodeAnswer) (*Loop, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	scopes, err := ScopesFromAnswer(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := OwnerFrom(a)
	return &Loop{Cfg: ownedCfg(), Scopes: scopes, Owned: &Owned{NodeID: a.NodeID, Owner: o}, Log: log.New(&buf, "", 0)}, &buf
}

func logLines(buf *bytes.Buffer) []string {
	return strings.Split(strings.TrimSpace(buf.String()), "\n")
}

// A registration answered by registerNode.json: one owner registration
// under the node's own id, and the loop's Scopes are the answer's three
// orgs, every one on the answer's one NodeID.
func TestOwnedRegisterFixtureThreeScopesOneNodeID(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: answerFixture(t, "registerNode.json"), scopes: personScopes(t, cfg)}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"probe", "any org", "owner 22222222-2222-4222-8222-222222222222"}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q", s.did, want)
	}
	if reg.Owned == nil || reg.Owned.NodeID != "55555555-5555-4555-8555-555555555555" || reg.Owned.Owner.String() != "owned by Chris" || reg.Owned.Owner.Kind != "person" {
		t.Fatalf("owned = %+v", reg.Owned)
	}
	loop := &Loop{Scopes: reg.Scopes, Owned: reg.Owned}
	if len(loop.Scopes) != 3 {
		t.Fatalf("scopes = %+v, want 3", loop.Scopes)
	}
	for _, sc := range loop.Scopes {
		if sc.NodeID != "55555555-5555-4555-8555-555555555555" {
			t.Fatalf("scope %s on node %s, want the answer's one id", sc.Name(), sc.NodeID)
		}
	}
	if got := strings.Join(loop.Scopes.Names(), ", "); got != "runsheet, wardroomid, grapevine" {
		t.Fatalf("names = %q", got)
	}
}

// A node with no node_id yet registers under one from (owner, slug), so
// a second registration converges on the same id.
func TestOwnedNodeIDConverges(t *testing.T) {
	cfg := ownedCfg()
	cfg.NodeID = ""
	a, b := OwnedNodeID(cfg), OwnedNodeID(cfg)
	if a == "" || a != b {
		t.Fatalf("OwnedNodeID = %q then %q, want one stable id", a, b)
	}
	cfg.NodeID = "22222222-2222-4222-8222-222222222222"
	if got := OwnedNodeID(cfg); got != cfg.NodeID {
		t.Fatalf("OwnedNodeID = %q, want node.yaml's %q", got, cfg.NodeID)
	}
}

// heartbeatNode.json's answer adds foghornid: TakeAnswer serves four
// orgs, grapevine by its slug, and logs the change as a reload does.
func TestOwnedTakeAnswerServesTheFourthOrg(t *testing.T) {
	loop, buf := ownedLoop(t, answerFixture(t, "registerNode.json"))
	loop.TakeAnswer(answerFixture(t, "heartbeatNode.json"))
	if len(loop.Scopes) != 4 {
		t.Fatalf("scopes = %+v, want 4", loop.Scopes)
	}
	grapevine := false
	for _, sc := range loop.Scopes {
		grapevine = grapevine || sc.Name() == "grapevine"
		if sc.NodeID != "55555555-5555-4555-8555-555555555555" {
			t.Fatalf("scope %s on node %s", sc.Name(), sc.NodeID)
		}
	}
	if !grapevine {
		t.Fatalf("no scope named grapevine in %q", loop.Scopes.Names())
	}
	var orgs string
	for _, l := range logLines(buf) {
		if strings.HasPrefix(l, "orgs ") {
			orgs = l
		}
	}
	if orgs != "orgs runsheet, wardroomid, grapevine → runsheet, wardroomid, grapevine, foghornid" {
		t.Fatalf("orgs line %q in log:\n%s", orgs, buf)
	}
}

// The mini handed from Chris to anythink: the next answer's owner is
// taken and logged, with no restart.
func TestOwnedTakeAnswerHandOver(t *testing.T) {
	loop, buf := ownedLoop(t, answerFixture(t, "registerNode.json"))
	handed := answerFixture(t, "registerNode.json")
	handed.Owner = gateway.NodeOwner{Kind: "enterprise", ID: "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb", Name: "anythink"}
	loop.TakeAnswer(handed)
	if want := "owner changed: Chris → anythink"; !slices.Contains(logLines(buf), want) {
		t.Fatalf("log lacks %q:\n%s", want, buf)
	}
	if loop.Owned.Owner.Kind != "enterprise" || loop.Owned.Owner.Name != "anythink" {
		t.Fatalf("owned = %+v", loop.Owned)
	}
	// the same owner again says nothing more
	buf.Reset()
	loop.TakeAnswer(handed)
	if buf.Len() != 0 {
		t.Fatalf("an unchanged answer logged:\n%s", buf)
	}
}

// A heartbeat with no answer keeps the last scopes.
func TestOwnedTakeAnswerNilKeepsScopes(t *testing.T) {
	loop, buf := ownedLoop(t, answerFixture(t, "registerNode.json"))
	before := slices.Clone(loop.Scopes)
	loop.TakeAnswer(nil)
	if !slices.Equal(loop.Scopes, before) || buf.Len() != 0 {
		t.Fatalf("scopes %+v → %+v, log %q", before, loop.Scopes, buf)
	}
}

// node.yaml's orgs restriction holds on the answer; one naming an org
// the node is not served is logged, not fatal.
func TestOwnedTakeAnswerRestriction(t *testing.T) {
	loop, buf := ownedLoop(t, answerFixture(t, "registerNode.json"))
	loop.restrict, loop.restrictSet = []string{"grapevine", "initech"}, true
	loop.TakeAnswer(answerFixture(t, "heartbeatNode.json"))
	if got := strings.Join(loop.Scopes.Names(), ", "); got != "grapevine" {
		t.Fatalf("names = %q, want grapevine alone", got)
	}
	if !strings.Contains(buf.String(), "orgs: not a member of initech") {
		t.Fatalf("log lacks the unserved org:\n%s", buf)
	}
}

// With TakesOwner false the registration is today's exactly: the
// person's orgs, one registration per org, no owner registration.
func TestOwnedRegisterLegacyPerOrgWithNoOwner(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: false, answer: answerFixture(t, "registerNode.json"), scopes: personScopes(t, cfg)}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"probe", "resolve", "org runsheet", "org wardroomid"}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q", s.did, want)
	}
	if reg.Owned != nil || !slices.Equal(reg.Scopes, s.scopes) || len(reg.Notes) != 0 {
		t.Fatalf("registered %+v, notes %q", reg, reg.Notes)
	}
}

// With TakesOwner true and a nil answer (the platform half-deployed):
// one owner registration, then the legacy path in full — one
// registration per org — and the fallback said.
func TestOwnedRegisterNilAnswerFallsBackPerOrg(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: nil, scopes: personScopes(t, cfg)}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"probe", "any org", "owner 22222222-2222-4222-8222-222222222222", "resolve", "org runsheet", "org wardroomid"}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q", s.did, want)
	}
	if reg.Owned != nil || !slices.Equal(reg.Scopes, s.scopes) {
		t.Fatalf("registered %+v, want the per-org scopes", reg)
	}
	if len(reg.Notes) != 1 || !strings.Contains(reg.Notes[0], "serving per org") {
		t.Fatalf("logged %q, want the fallback once", reg.Notes)
	}
}

// The probe comes before the person's orgs are read: an orgs
// restriction naming grapevine by its slug — an org the person's
// MyOrgs, which the restriction would be checked against, cannot meet —
// is checked against the answer instead, and holds.
func TestOwnedRegisterProbesBeforeResolving(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: answerFixture(t, "registerNode.json"), resolveErr: fmt.Errorf("not a member of grapevine (the node.yaml orgs list names them)")}
	reg, err := Register(context.Background(), s.registrar(), cfg, []string{"grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"probe", "any org", "owner 22222222-2222-4222-8222-222222222222"}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q (the person's orgs never read)", s.did, want)
	}
	if got := strings.Join(reg.Scopes.Names(), ", "); got != "grapevine" || reg.Owned == nil {
		t.Fatalf("scopes %q owned %+v, want grapevine alone on the owner contract", got, reg.Owned)
	}
	// With the probe false the same restriction is today's refusal.
	s = &steps{takes: false, resolveErr: fmt.Errorf("not a member of grapevine")}
	if _, err := Register(context.Background(), s.registrar(), cfg, []string{"grapevine"}); err == nil {
		t.Fatal("legacy path took a restriction the person's orgs cannot meet")
	}
	if want := []string{"probe", "resolve"}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q", s.did, want)
	}
}

// On the owner contract the reload reads node.yaml alone: its orgs
// restriction is applied to the last answer at once, and no scopes come
// from it.
func TestOwnedReloaderRestrictsTheLastAnswer(t *testing.T) {
	loop, _ := ownedLoop(t, answerFixture(t, "registerNode.json"))
	loop.TakeAnswer(answerFixture(t, "heartbeatNode.json"))
	loop.Reload = OwnedReloader(func() (*Config, error) {
		return &Config{Slug: "mini", Capacity: Fixed(2), Orgs: []string{"foghornid"}}, nil
	})
	got, err := loop.Reload(context.Background())
	if err != nil || got.Scopes != nil || !got.OrgsRead || got.Capacity.Max != 2 {
		t.Fatalf("reloaded %+v, %v", got, err)
	}
	loop.reload(context.Background())
	if names := strings.Join(loop.Scopes.Names(), ", "); names != "foghornid" {
		t.Fatalf("names = %q, want foghornid alone", names)
	}
}

// An owned node still knows the per-org ids it registered under before,
// so it adopts the sessions they started.
func TestOwnedLegacyIDsAreThePerOrgOnes(t *testing.T) {
	cfg := ownedCfg()
	scopes, err := ScopesFromAnswer(answerFixture(t, "registerNode.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy := LegacyIDs(cfg, scopes)
	if len(legacy) != 3 {
		t.Fatalf("legacy ids = %v, want 3", legacy)
	}
	for _, sc := range scopes {
		id, _ := cfg.NodeIDIn(sc.ID)
		if legacy[id] != sc.ID {
			t.Fatalf("legacy[%s] = %q, want %s's org %s", id, legacy[id], sc.Name(), sc.ID)
		}
	}
}

// A non-nil answer naming no org is the owner contract, not a nil one:
// registration is the one owner registration, no per-org path and no
// fallback line, and the node serves none until an answer names one.
func TestOwnedRegisterEmptyAnswerIsNotNil(t *testing.T) {
	cfg := ownedCfg()
	a := answerFixture(t, "registerNode.json")
	a.Scopes = nil
	s := &steps{takes: true, answer: a, scopes: personScopes(t, cfg)}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"probe", "any org", "owner 22222222-2222-4222-8222-222222222222"}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q (no per-org registration)", s.did, want)
	}
	if reg.Owned == nil || reg.Owned.NodeID != a.NodeID || len(reg.Scopes) != 0 {
		t.Fatalf("registered %+v, want owned and serving none", reg)
	}
	for _, line := range reg.Notes {
		if strings.Contains(line, "serving per org") {
			t.Fatalf("an empty answer took the nil answer's fallback: %q", reg.Notes)
		}
	}
}

// A restriction matching none of the answer's orgs is logged, not a
// failed registration: the node is owned and serves none.
func TestOwnedRegisterUnmatchedRestrictionIsNotFatal(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: answerFixture(t, "registerNode.json")}
	reg, err := Register(context.Background(), s.registrar(), cfg, []string{"initech"})
	if err != nil {
		t.Fatalf("Register = %v, want the restriction logged, not fatal", err)
	}
	if reg.Owned == nil || len(reg.Scopes) != 0 {
		t.Fatalf("registered %+v, want owned and serving none", reg)
	}
	if len(reg.Notes) != 1 || !strings.Contains(reg.Notes[0], "not a member of initech") {
		t.Fatalf("logged %q, want the unmatched restriction once", reg.Notes)
	}
}

// A heartbeat answer that drops orgs replaces the scopes: an org the
// platform no longer serves is claimed from no more, and one naming no
// org leaves the node serving none — only a nil answer keeps them.
func TestOwnedTakeAnswerDropsRemovedOrgs(t *testing.T) {
	loop, buf := ownedLoop(t, answerFixture(t, "heartbeatNode.json"))
	a := answerFixture(t, "heartbeatNode.json")
	a.Scopes = a.Scopes[:1]
	loop.TakeAnswer(a)
	if len(loop.Scopes) != 1 || loop.Scopes[0].Name() != (Scope{Login: a.Scopes[0].Login, Slug: a.Scopes[0].Slug}).Name() {
		t.Fatalf("scopes %q, want the one org the answer names", loop.Scopes.Names())
	}
	a.Scopes = nil
	loop.TakeAnswer(a)
	if len(loop.Scopes) != 0 {
		t.Fatalf("scopes %q after an answer naming no org, want none", loop.Scopes.Names())
	}
	if lines := logLines(buf); !strings.HasSuffix(lines[len(lines)-1], "→ none") {
		t.Fatalf("log %q, want the last orgs line ending → none", lines)
	}
}

// A restriction matching none of a heartbeat answer's orgs is logged,
// and the node serves none of them.
func TestOwnedTakeAnswerUnmatchedRestriction(t *testing.T) {
	loop, buf := ownedLoop(t, answerFixture(t, "registerNode.json"))
	loop.restrict, loop.restrictSet = []string{"initech"}, true
	loop.TakeAnswer(answerFixture(t, "heartbeatNode.json"))
	if len(loop.Scopes) != 0 {
		t.Fatalf("scopes %q, want none", loop.Scopes.Names())
	}
	if !strings.Contains(buf.String(), "orgs: not a member of initech") {
		t.Fatalf("log lacks the unmatched restriction:\n%s", buf)
	}
}

// ownedGW is an in-process platform for an owned node's Tick: every
// heartbeat answers answer (nil, an older platform's silence, until
// set), one ready item per org is listed, a claim is recorded with the
// node id it carried, and everything else answers empty.
type ownedGW struct {
	Gateway
	mu     sync.Mutex
	answer *gateway.NodeAnswer
	items  map[string]gateway.WorkItem // org id → its ready item
	beats  []string                    // the node id each heartbeat carried
	claims map[string]string           // org id → the node id its claim carried
}

func (g *ownedGW) HeartbeatNode(_ context.Context, nodeID, _ string, _ int, _ ...gateway.HeartbeatExtras) (*gateway.NodeAnswer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.beats = append(g.beats, nodeID)
	return g.answer, nil
}

func (g *ownedGW) ReadyWorkItems(_ context.Context, orgID string) ([]gateway.WorkItem, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if it, ok := g.items[orgID]; ok && g.claims[orgID] == "" {
		return []gateway.WorkItem{it}, nil
	}
	return nil, nil
}

func (g *ownedGW) ClaimWorkItem(_ context.Context, itemID, nodeID, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for org, it := range g.items {
		if it.ID == itemID {
			g.claims[org] = nodeID
		}
	}
	return nil
}

func (g *ownedGW) GetDesiredVersion(context.Context, string) (string, error) { return "", nil }
func (g *ownedGW) Goals(context.Context, string) ([]gateway.Goal, error)     { return nil, nil }
func (g *ownedGW) GoalWorkItems(context.Context, string) ([]gateway.WorkItem, error) {
	return nil, nil
}
func (g *ownedGW) OrgWorkItems(context.Context, string, string) ([]gateway.WorkItem, error) {
	return nil, nil
}
func (g *ownedGW) RequestedRebases(context.Context, string) ([]gateway.WorkItem, error) {
	return nil, nil
}
func (g *ownedGW) Projects(context.Context, string) ([]gateway.Project, error) { return nil, nil }
func (g *ownedGW) Directions(context.Context, string, string) ([]gateway.Direction, error) {
	return nil, nil
}

func (g *ownedGW) setAnswer(a *gateway.NodeAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer = a
}

func (g *ownedGW) beatsNow() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.beats)
}

func (g *ownedGW) claimed() map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.claims)
}

// An owned node's loop through Tick, no socket: registerNode.json's
// answer serves three orgs on one id; a tick heartbeats once, under that
// id, and claims from each of the three on it — grapevine, with no
// login, named by its slug. When the next heartbeat's answer
// (heartbeatNode.json) adds foghornid the tick after serves it too,
// with no restart.
func TestOwnedLoopClaimsFromEveryAnsweredOrgAndAFourthWithNoRestart(t *testing.T) {
	const nodeID = "55555555-5555-4555-8555-555555555555"
	registered, heartbeat := answerFixture(t, "registerNode.json"), answerFixture(t, "heartbeatNode.json")
	gw := &ownedGW{items: map[string]gateway.WorkItem{}, claims: map[string]string{}}
	names := map[string]string{} // org id → its name
	for i, sa := range heartbeat.Scopes {
		name := Scope{Login: sa.Login, Slug: sa.Slug}.Name()
		names[sa.OrgID] = name
		gw.items[sa.OrgID] = gateway.WorkItem{ID: fmt.Sprintf("e0000000-0000-4000-8000-00000000000%d", i), GoalID: fmt.Sprintf("c0000000-0000-4000-8000-00000000000%d", i),
			OrgID: sa.OrgID, Seq: 1, Title: name + " work", Status: "ready"}
	}
	s := &steps{takes: true, answer: registered}
	reg, err := Register(context.Background(), s.registrar(), ownedCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ran []string
	var buf bytes.Buffer
	loop := &Loop{GW: gw, Cfg: &Config{Slug: "mini", Name: "mini", Capacity: Fixed(8)}, Scopes: reg.Scopes, Owned: reg.Owned,
		Interval: time.Hour, Harness: "claude-code", Log: log.New(&buf, "", 0),
		Dispatch: func(_ context.Context, item gateway.WorkItem) {
			mu.Lock()
			ran = append(ran, names[item.OrgID])
			mu.Unlock()
		}}
	loop.TakeAnswer(reg.Answer)
	gw.setAnswer(registered)

	ctx := context.Background()
	loop.Tick(ctx)
	loop.wg.Wait()
	if beats := gw.beatsNow(); !slices.Equal(beats, []string{nodeID}) {
		t.Fatalf("heartbeats %q, want one under the node's id", beats)
	}
	claims := gw.claimed()
	for _, sa := range registered.Scopes {
		if claims[sa.OrgID] != nodeID {
			t.Fatalf("%s's item claimed by %q, want %s (claims %v)", names[sa.OrgID], claims[sa.OrgID], nodeID, claims)
		}
	}
	if len(claims) != 3 {
		t.Fatalf("claims %v, want the three answered orgs", claims)
	}
	mu.Lock()
	first := slices.Sorted(slices.Values(ran))
	mu.Unlock()
	if want := []string{"grapevine", "runsheet", "wardroomid"}; !slices.Equal(first, want) {
		t.Fatalf("first tick ran %q, want %q", first, want)
	}

	gw.setAnswer(heartbeat)
	loop.Tick(ctx) // the heartbeat's answer names foghornid: served from this tick on
	loop.wg.Wait()
	if beats := gw.beatsNow(); !slices.Equal(beats, []string{nodeID, nodeID}) {
		t.Fatalf("heartbeats %q, want one per tick under the node's id", beats)
	}
	if got := gw.claimed()["cccccccc-3333-4333-8333-cccccccccccc"]; got != nodeID {
		t.Fatalf("foghornid's item claimed by %q, want %s", got, nodeID)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 4 || ran[3] != "foghornid" {
		t.Fatalf("second tick ran %q, want foghornid added", ran)
	}
	for _, want := range []string{
		"orgs runsheet, wardroomid, grapevine → runsheet, wardroomid, grapevine, foghornid",
		"owner changed: Chris → anythink",
	} {
		if !slices.Contains(logLines(&buf), want) {
			t.Fatalf("log lacks %q:\n%s", want, buf.String())
		}
	}
}

// node start hands the registration's answer to the loop before its
// first tick: a reload that narrows node.yaml's orgs to runsheet applies
// to it even though every heartbeat since answered nil.
func TestOwnedStartHandsTheAnswerToTheLoop(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: answerFixture(t, "registerNode.json")}
	reg, err := Register(context.Background(), s.registrar(), cfg, cfg.Orgs)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Answer == nil {
		t.Fatal("Registered carries no answer")
	}
	var buf bytes.Buffer
	gw := &ownedGW{items: map[string]gateway.WorkItem{}, claims: map[string]string{}} // every heartbeat answers nil
	// as nodeStart builds it
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: reg.Scopes, Owned: reg.Owned, Interval: time.Hour, Harness: "claude-code", Log: log.New(&buf, "", 0)}
	loop.Reload = OwnedReloader(func() (*Config, error) {
		return &Config{Slug: "mini", Name: mini, Capacity: Fixed(2), Orgs: []string{"runsheet"}}, nil
	})
	loop.TakeAnswer(reg.Answer)
	if buf.Len() != 0 {
		t.Fatalf("taking the registration's own answer logged:\n%s", buf.String())
	}
	loop.Tick(context.Background())
	loop.wg.Wait()
	if len(gw.beatsNow()) != 1 {
		t.Fatalf("heartbeats %q", gw.beatsNow())
	}
	if names := strings.Join(loop.CurrentScopes().Names(), ", "); names != "runsheet" {
		t.Fatalf("scopes %q, want runsheet alone", names)
	}
	if !slices.Contains(logLines(&buf), "orgs runsheet, wardroomid, grapevine → runsheet") {
		t.Fatalf("log lacks the orgs change:\n%s", buf.String())
	}
	if line := StartLine(cfg, reg); line != "node Chris's Mini (mini) owned by Chris, serving runsheet, wardroomid, grapevine" {
		t.Fatalf("start line %q", line)
	}
}

// Every registration records its contract in node.yaml beside the
// node's id: the owner contract `contract: owner` and the answer's one
// id, the legacy path `contract: per-org`. A failed save is an error.
func TestOwnedContractPersisted(t *testing.T) {
	read := func(t *testing.T) *Config {
		t.Helper()
		c, err := LoadSettings()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Run("owner", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		cfg := ownedCfg()
		s := &steps{takes: true, answer: answerFixture(t, "registerNode.json")}
		reg, err := Register(context.Background(), s.registrar(), cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveRegistration(cfg, reg, nil); err != nil {
			t.Fatal(err)
		}
		got := read(t)
		if got.Contract != ContractOwner || got.NodeID != "55555555-5555-4555-8555-555555555555" {
			t.Fatalf("node.yaml contract %q node_id %q", got.Contract, got.NodeID)
		}
		path, _ := ConfigPath()
		if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "contract: owner\n") {
			t.Fatalf("node.yaml:\n%s", raw)
		}
	})
	t.Run("per-org", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		cfg := ownedCfg()
		cfg.NodeID = ""
		s := &steps{scopes: personScopes(t, cfg)}
		reg, err := Register(context.Background(), s.registrar(), cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveRegistration(cfg, reg, nil); err != nil {
			t.Fatal(err)
		}
		if got := read(t); got.Contract != ContractPerOrg || got.NodeID != "" {
			t.Fatalf("node.yaml contract %q node_id %q", got.Contract, got.NodeID)
		}
		if StartLine(cfg, reg) != "node Chris's Mini (mini) per org, serving runsheet, wardroomid" {
			t.Fatalf("start line %q", StartLine(cfg, reg))
		}
	})
	t.Run("old node.yaml", func(t *testing.T) {
		if (&Config{}).ContractKind() != ContractPerOrg {
			t.Fatal("an empty contract must read per-org")
		}
	})
	t.Run("failed save", func(t *testing.T) {
		cfg := ownedCfg()
		refused := errors.New("read-only file system")
		err := SaveRegistration(cfg, Registered{Owned: &Owned{NodeID: "55555555-5555-4555-8555-555555555555"}}, func() error { return refused })
		if !errors.Is(err, refused) {
			t.Fatalf("SaveRegistration = %v, want the save's error", err)
		}
	})
}

// node.log's first line after the start lines names the owner and the
// orgs; what the registration has to say — an unmatched restriction,
// the nil answer's fallback — follows it, never precedes it.
func TestOwnedRegistrationLinesNameTheOwnerFirst(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: answerFixture(t, "registerNode.json")}
	reg, err := Register(context.Background(), s.registrar(), cfg, []string{"grapevine", "initech"})
	if err != nil {
		t.Fatal(err)
	}
	lines := RegistrationLines(cfg, reg)
	if len(lines) != 2 || !strings.Contains(lines[0], " owned by ") || !strings.HasSuffix(lines[0], "serving grapevine") || !strings.HasPrefix(lines[1], "orgs: ") {
		t.Fatalf("lines %q, want the owner line first, then the restriction", lines)
	}
	s = &steps{takes: true, scopes: personScopes(t, cfg)}
	if reg, err = Register(context.Background(), s.registrar(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	lines = RegistrationLines(cfg, reg)
	if len(lines) != 2 || !strings.Contains(lines[0], " per org, serving ") || lines[1] != FallbackLine {
		t.Fatalf("lines %q, want the per-org line first, then the fallback", lines)
	}
}

// Status and the rollback reporter heartbeat under the one id only on
// the contract the last registration saved: a per-org node, or a
// node.yaml before `contract:`, that kept a node_id is reached per org.
func TestOwnedContractChoosesTheHeartbeat(t *testing.T) {
	for _, tc := range []struct{ contract, want string }{
		{ContractOwner, "55555555-5555-4555-8555-555555555555"},
		{ContractPerOrg, ""},
		{"", ""},
	} {
		cfg := &Config{NodeID: "55555555-5555-4555-8555-555555555555", Contract: tc.contract}
		if got := cfg.OwnedID(); got != tc.want {
			t.Errorf("contract %q: OwnedID = %q, want %q", tc.contract, got, tc.want)
		}
	}
	if (&Config{Contract: ContractOwner}).OwnedID() != "" {
		t.Error("an owner contract with no node_id heartbeats under an empty id")
	}
}

// outsideGW is the platform a command outside the loop sees: the probe,
// the heartbeat (answering answer) and the person's orgs, each call
// recorded.
type outsideGW struct {
	takes  bool
	answer *gateway.NodeAnswer
	orgs   []gateway.Org
	beats  []string
	myOrgs int
}

func (g *outsideGW) TakesOwner(context.Context) bool { return g.takes }
func (g *outsideGW) HeartbeatNode(_ context.Context, nodeID, _ string, _ int, _ ...gateway.HeartbeatExtras) (*gateway.NodeAnswer, error) {
	g.beats = append(g.beats, nodeID)
	return g.answer, nil
}
func (g *outsideGW) MyOrgs(context.Context) ([]gateway.Org, error) {
	g.myOrgs++
	return g.orgs, nil
}

// `mesh node status` and the rollback reporter on a saved owner
// contract: a nil heartbeat answer keeps the contract — one heartbeat,
// under the one id, and never the person's orgs or a heartbeat per org,
// even when the token's person is in orgs the owner is not.
func TestOwnedOutsideHeartbeatNilAnswerKeepsTheOneID(t *testing.T) {
	const id = "55555555-5555-4555-8555-555555555555"
	cfg := &Config{NodeID: id, Contract: ContractOwner, Slug: "runsheet-mini"}
	gw := &outsideGW{takes: true, orgs: []gateway.Org{
		{ID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Login: "runsheet"},
		{ID: "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb", Login: "wardroomid"},
	}}
	out, err := HeartbeatOutside(context.Background(), gw, cfg, "v1", gateway.HeartbeatExtras{}, func(Scope, error) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !out.Owned || out.Answer != nil || len(out.Scopes) != 0 {
		t.Errorf("out = %+v, want owned with no answer and no scopes", out)
	}
	if !slices.Equal(gw.beats, []string{id}) {
		t.Errorf("heartbeats = %v, want one under %s", gw.beats, id)
	}
	if gw.myOrgs != 0 {
		t.Errorf("MyOrgs read %d times on the owner contract", gw.myOrgs)
	}
}

// A per-org node (or a node.yaml before `contract:`) is reached per org
// alone, whatever node_id it kept, and a platform that no longer takes
// `owner` reaches an owned one per org too.
func TestOwnedOutsideHeartbeatPerOrg(t *testing.T) {
	orgs := []gateway.Org{
		{ID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Login: "runsheet"},
		{ID: "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb", Login: "wardroomid"},
	}
	for _, tc := range []struct {
		contract string
		takes    bool
	}{{"", true}, {ContractPerOrg, true}, {ContractOwner, false}} {
		cfg := &Config{NodeID: "55555555-5555-4555-8555-555555555555", Contract: tc.contract, Slug: "runsheet-mini"}
		gw := &outsideGW{takes: tc.takes, orgs: orgs}
		out, err := HeartbeatOutside(context.Background(), gw, cfg, "v1", gateway.HeartbeatExtras{}, func(Scope, error) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if out.Owned || len(out.Scopes) != 2 || len(gw.beats) != 2 || gw.myOrgs != 1 {
			t.Errorf("contract %q takes %v: out %+v, beats %v, myOrgs %d; want per org", tc.contract, tc.takes, out, gw.beats, gw.myOrgs)
		}
		for i, sc := range out.Scopes {
			if gw.beats[i] != sc.NodeID {
				t.Errorf("beat %d under %s, want %s's per-org id %s", i, gw.beats[i], sc.Name(), sc.NodeID)
			}
		}
	}
}
