package node

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// These tests read Register without a socket: a Registrar of recording
// functions, answered from the contract fixtures (specs/contracts/).

// steps is a Registrar that records what Register did, in order:
// "probe", "resolve", "owner <nodeID>", and "org <name>" per legacy
// registration.
type steps struct {
	takes  bool
	answer *gateway.NodeAnswer
	scopes Scopes
	did    []string
	logged []string
}

func (s *steps) registrar() Registrar {
	return Registrar{
		TakesOwner: func(context.Context) bool {
			s.did = append(s.did, "probe")
			return s.takes
		},
		Resolve: func(context.Context) (Scopes, error) {
			s.did = append(s.did, "resolve")
			return s.scopes, nil
		},
		RegisterOwner: func(_ context.Context, nodeID string) (*gateway.NodeAnswer, error) {
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

const ownedID = "22222222-2222-4222-8222-222222222222"

func ownedCfg() *Config {
	return &Config{Slug: "mini", Name: "Chris's Mini", OwnerID: "44444444-4444-4444-8444-444444444444", NodeID: ownedID}
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

func countPrefix(did []string, prefix string) int {
	n := 0
	for _, d := range did {
		if strings.HasPrefix(d, prefix) {
			n++
		}
	}
	return n
}

// A platform without `owner` is asked first and gets today's path
// exactly: resolve, a registration per org, no owner registration.
func TestOwnedRegisterProbesBeforeResolving(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: false, scopes: personScopes(t, cfg)}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"probe", "resolve", "org runsheet", "org wardroomid"}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q", s.did, want)
	}
	if reg.Contract != ContractPerOrg || reg.Owned != nil || len(reg.Scopes) != 2 {
		t.Fatalf("registered = %+v", reg)
	}
}

// A platform that takes `owner` but answers status alone falls to the
// per-org path: one registration per org after the owner one, and the
// owner registration's id is served nowhere.
func TestOwnedRegisterNilAnswerIsLegacy(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: nil, scopes: personScopes(t, cfg)}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"probe", "owner " + ownedID, "resolve", "org runsheet", "org wardroomid"}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q", s.did, want)
	}
	if n := countPrefix(s.did, "org "); n != 2 {
		t.Fatalf("%d per-org registrations, want one per org (2)", n)
	}
	if reg.Contract != ContractPerOrg || reg.Owned != nil {
		t.Fatalf("registered = %+v, want per-org", reg)
	}
	for _, sc := range reg.Scopes {
		if sc.NodeID == ownedID {
			t.Fatalf("scope %s serves under the owner id %s", sc.Name(), ownedID)
		}
	}
}

// registerNode.json's answer: one owner registration, the answer's one
// node id on every scope, three orgs, grapevine named by its slug.
func TestOwnedRegisterOnceWithOwner(t *testing.T) {
	cfg := ownedCfg()
	a := answerFixture(t, "registerNode.json")
	s := &steps{takes: true, answer: a, scopes: personScopes(t, cfg)}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"probe", "owner " + ownedID}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q", s.did, want)
	}
	if reg.Contract != ContractOwner || reg.Owned == nil || reg.Owned.NodeID != a.NodeID || reg.Owned.Answer != a {
		t.Fatalf("registered = %+v", reg)
	}
	if reg.Owned.Owner.String() != "owned by Chris" || reg.Owned.Owner.Kind != "person" {
		t.Fatalf("owner = %+v", reg.Owned.Owner)
	}
	if len(reg.Scopes) < 3 {
		t.Fatalf("scopes = %+v, want three or more", reg.Scopes)
	}
	for _, sc := range reg.Scopes {
		if sc.NodeID != a.NodeID {
			t.Fatalf("scope %s on %s, want the one id %s", sc.Name(), sc.NodeID, a.NodeID)
		}
	}
	if !slices.Contains(reg.Scopes.Names(), "grapevine") {
		t.Fatalf("names = %q, want grapevine by its slug", reg.Scopes.Names())
	}
}

// A non-nil answer naming no org is the owner contract serving nothing,
// never the per-org path.
func TestOwnedRegisterEmptyAnswerServesNothing(t *testing.T) {
	cfg := ownedCfg()
	a := &gateway.NodeAnswer{NodeID: "55555555-5555-4555-8555-555555555555", Owner: gateway.NodeOwner{Kind: "enterprise", ID: "e", Name: "anythink"}}
	s := &steps{takes: true, answer: a, scopes: personScopes(t, cfg)}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Contract != ContractOwner || reg.Owned == nil || len(reg.Scopes) != 0 {
		t.Fatalf("registered = %+v, want owner serving nothing", reg)
	}
	if n := countPrefix(s.did, "org "); n != 0 || slices.Contains(s.did, "resolve") {
		t.Fatalf("did %q: the per-org path ran", s.did)
	}
}

// A restriction naming no org the answer serves is no error: the node
// serves nothing and says so.
func TestOwnedRegisterUnmatchedRestrictionIsLogged(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: answerFixture(t, "registerNode.json")}
	reg, err := Register(context.Background(), s.registrar(), cfg, []string{"foghornid"})
	if err != nil {
		t.Fatalf("register: %v, want no error", err)
	}
	if reg.Contract != ContractOwner || len(reg.Scopes) != 0 {
		t.Fatalf("registered = %+v, want owner serving nothing", reg)
	}
	want := "orgs: none of foghornid is served by this node's owner; serving nothing until it changes"
	if !slices.Contains(s.logged, want) {
		t.Fatalf("logged %q, want %q", s.logged, want)
	}
}

// A restriction to the first org keeps that org alone, matched without
// case; a match never lifts it for the orgs after.
func TestOwnedRegisterRestrictionStaysActive(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: answerFixture(t, "registerNode.json")}
	reg, err := Register(context.Background(), s.registrar(), cfg, []string{"RunSheet"})
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Scopes.Names(); !slices.Equal(got, []string{"runsheet"}) {
		t.Fatalf("scopes = %q, want just runsheet", got)
	}
}

func TestConfigContractValidated(t *testing.T) {
	for _, tc := range []struct {
		contract string
		ok       bool
		owner    bool
	}{{"owner", true, true}, {"per-org", true, false}, {"", true, false}, {"enterprise", false, false}} {
		t.Setenv("HOME", t.TempDir())
		writeNodeYAML(t, "slug: mini\nname: mini\ncontract: "+tc.contract+"\n")
		c, err := LoadConfig()
		if (err == nil) != tc.ok {
			t.Fatalf("contract %q: err = %v, want ok %v", tc.contract, err, tc.ok)
		}
		if err != nil {
			if !strings.Contains(err.Error(), "contract") {
				t.Fatalf("contract %q: err = %v, want it to name contract", tc.contract, err)
			}
			continue
		}
		if c.OnOwnerContract() != tc.owner {
			t.Fatalf("contract %q: OnOwnerContract = %v", tc.contract, c.OnOwnerContract())
		}
	}
}

func TestConfigContractPersisted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := ownedCfg()
	cfg.NodeID = ""
	reg := Registered{Contract: ContractOwner, Owned: &Owned{NodeID: "55555555-5555-4555-8555-555555555555"}}
	if err := WriteNodeID(cfg, reg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Contract != ContractOwner || got.NodeID != "55555555-5555-4555-8555-555555555555" || !got.OnOwnerContract() {
		t.Fatalf("loaded contract %q node_id %q", got.Contract, got.NodeID)
	}
	got.Contract = ContractPerOrg
	if err := SaveConfig(got); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if again.Contract != ContractPerOrg || again.NodeID != got.NodeID {
		t.Fatalf("reloaded contract %q node_id %q", again.Contract, again.NodeID)
	}
}
