package node

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// These tests read the owner contract without a socket: Register is
// driven through a Registrar of recording functions and TakeAnswer with
// the contract fixtures' answers (specs/contracts/*.json). The
// gateway-backed ones are TestLoopOwned… in loop_test.go.

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
	logged     []string
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
		Logf: func(format string, args ...any) { s.logged = append(s.logged, fmt.Sprintf(format, args...)) },
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
	if want := "owner owned by Chris → owned by anythink"; !slices.Contains(logLines(buf), want) {
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
	if reg.Owned != nil || !slices.Equal(reg.Scopes, s.scopes) || len(s.logged) != 0 {
		t.Fatalf("registered %+v, logged %q", reg, s.logged)
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
	if len(s.logged) != 1 || !strings.Contains(s.logged[0], "serving per org") {
		t.Fatalf("logged %q, want the fallback once", s.logged)
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
