package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

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
	// ownerIDs is each ownerId RegisterOwner was sent, in order
	ownerIDs []string
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
		ConfiguredOrg: func(context.Context) (string, error) {
			s.did = append(s.did, "any org")
			return "33333333-3333-4333-8333-333333333333", nil
		},
		RegisterOwner: func(_ context.Context, nodeID, orgID, ownerID string) (*gateway.NodeAnswer, error) {
			s.did = append(s.did, "owner "+nodeID)
			s.ownerIDs = append(s.ownerIDs, ownerID)
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

func logLines(buf *bytes.Buffer) []string {
	return strings.Split(strings.TrimSpace(buf.String()), "\n")
}

// A node with no node_id yet registers under one from (owner, slug), so
// a second registration converges on the same id.
func TestOwnedNodeIDConverges(t *testing.T) {
	cfg := ownedCfg()
	cfg.NodeID = ""
	a, b := OwnedNodeID(cfg, cfg.OwnerKind(), cfg.OwnerID), OwnedNodeID(cfg, cfg.OwnerKind(), cfg.OwnerID)
	if a == "" || a != b {
		t.Fatalf("OwnedNodeID = %q then %q, want one stable id", a, b)
	}
	cfg.NodeID = "22222222-2222-4222-8222-222222222222"
	if got := OwnedNodeID(cfg, cfg.OwnerKind(), cfg.OwnerID); got != cfg.NodeID {
		t.Fatalf("OwnedNodeID = %q, want node.yaml's %q", got, cfg.NodeID)
	}
}

// registerNodeOwnerNodeIDFixture reads specs/contracts/registerNode.json's
// ownerNodeId object: the platform's own formula's worked example,
// never invented here.
func registerNodeOwnerNodeIDFixture(t *testing.T) (kind, owner, slug, id string) {
	t.Helper()
	raw, err := os.ReadFile("../../specs/contracts/registerNode.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		OwnerNodeID struct {
			Kind  string `json:"kind"`
			Owner string `json:"owner"`
			Slug  string `json:"slug"`
			ID    string `json:"id"`
		} `json:"ownerNodeId"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.OwnerNodeID.ID == "" {
		t.Fatal("registerNode.json: no ownerNodeId")
	}
	return fx.OwnerNodeID.Kind, fx.OwnerNodeID.Owner, fx.OwnerNodeID.Slug, fx.OwnerNodeID.ID
}

// OwnedNodeID over a Config built from the fixture's inputs, with no
// node_id set, equals the fixture's worked example (ADR-015,
// 2026-10-03): the node derives the owner row's id the same way the
// platform does.
func TestOwnedNodeIDMatchesFixtureExample(t *testing.T) {
	kind, owner, slug, want := registerNodeOwnerNodeIDFixture(t)
	cfg := &Config{Slug: slug, OwnerID: owner}
	if got := OwnedNodeID(cfg, kind, owner); got != want {
		t.Fatalf("OwnedNodeID(kind %q, owner %q, slug %q) = %q, want the fixture's %q", kind, owner, slug, got, want)
	}
}

// An unparsable owner id still falls back to a fresh uuid, never an
// empty or malformed one.
func TestOwnedNodeIDFallsBackOnAnUnparsableOwner(t *testing.T) {
	cfg := &Config{Slug: "mini"}
	a, b := OwnedNodeID(cfg, "person", "not-a-uuid"), OwnedNodeID(cfg, "person", "not-a-uuid")
	if a == "" || b == "" || a == b {
		t.Fatalf("OwnedNodeID with an unparsable owner = %q then %q, want two distinct fresh uuids", a, b)
	}
}

// An enterprise node derives its owned id from the enterprise's own
// id, not the admin person's owner_id — the ownerId the registration
// actually sends (ownerIDFor), never cfg.OwnerID directly.
func TestOwnedNodeIDDerivesFromTheEnterpriseIDNotThePerson(t *testing.T) {
	const (
		personID     = "a1111111-1111-4111-8111-111111111111"
		enterpriseID = "a3333333-1111-4111-8111-111111111111"
	)
	cfg := &Config{Slug: "runsheet-mini", Owner: "enterprise", Enterprise: "anythink", OwnerID: personID}
	if cfg.OwnerKind() != "enterprise" {
		t.Fatalf("OwnerKind() = %q, want enterprise", cfg.OwnerKind())
	}
	want := gateway.NodeOwnerID("enterprise", uuid.MustParse(enterpriseID), cfg.Slug).String()
	got := OwnedNodeID(cfg, cfg.OwnerKind(), enterpriseID)
	if got != want {
		t.Fatalf("OwnedNodeID = %q, want %q (derived from the enterprise id)", got, want)
	}
	if fromPerson := OwnedNodeID(cfg, cfg.OwnerKind(), personID); got == fromPerson {
		t.Fatalf("OwnedNodeID derived from the enterprise id matches deriving from the person's owner_id %q — it must not", personID)
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
// the probe chose the contract, so the node keeps it — one owner
// registration, no per-org one, the one id kept — and serves the orgs
// it served per org, read through Resolve after the owner registration,
// until a heartbeat answers; the fallback said.
func TestOwnedRegisterNilAnswerKeepsTheOwnerContract(t *testing.T) {
	cfg := ownedCfg()
	s := &steps{takes: true, answer: nil, scopes: personScopes(t, cfg)}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"probe", "any org", "owner 22222222-2222-4222-8222-222222222222", "resolve"}; !slices.Equal(s.did, want) {
		t.Fatalf("did %q, want %q (no per-org registration)", s.did, want)
	}
	if reg.Owned == nil || reg.Owned.NodeID != "22222222-2222-4222-8222-222222222222" || !slices.Equal(reg.Scopes, s.scopes) || reg.Answer != nil {
		t.Fatalf("registered %+v, want the owner contract on the one id serving the per-org scopes", reg)
	}
	if reg.Contract() != ContractOwner {
		t.Fatalf("contract %q, want %q", reg.Contract(), ContractOwner)
	}
	if len(reg.Notes) != 1 || reg.Notes[0] != FallbackLine {
		t.Fatalf("logged %q, want the fallback once", reg.Notes)
	}
	saved := false
	if err := SaveRegistration(cfg, reg, func() error { saved = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if cfg.Contract != ContractOwner || cfg.OwnedID() != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("saved contract %q id %q, want the owner contract on the one id (saved %v)", cfg.Contract, cfg.OwnedID(), saved)
	}
}

// The 2026-10-03 canary: node.yaml's orgs name grapevine, the person's
// first org is foghornid, the platform takes ownerKind and answers nil.
// The node keeps the owner contract under its one id and serves
// grapevine on its per-org id — what Resolve gave — never none; a
// Resolve that fails is noted and serves none, on the owner contract
// still.
func TestOwnedRegisterNilAnswerKeepsThePerOrgScopes(t *testing.T) {
	cfg := ownedCfg()
	cfg.Orgs = []string{"grapevine"}
	gv, err := cfg.NodeIDIn("dddddddd-4444-4444-8444-dddddddddddd")
	if err != nil {
		t.Fatal(err)
	}
	resolved := Scopes{{Slug: "grapevine", ID: "dddddddd-4444-4444-8444-dddddddddddd", NodeID: gv}}
	s := &steps{takes: true, answer: nil, scopes: resolved}
	reg, err := Register(context.Background(), s.registrar(), cfg, cfg.Orgs)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Contract() != ContractOwner || reg.Owned.NodeID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("contract %q id %q, want the owner contract on the owned id", reg.Contract(), reg.Owned.NodeID)
	}
	if !slices.Equal(reg.Scopes, resolved) || !slices.Equal(reg.Scopes.Names(), []string{"grapevine"}) {
		t.Fatalf("scopes %+v, want what Resolve returned (grapevine)", reg.Scopes)
	}
	if !slices.Contains(reg.Notes, FallbackLine) || !strings.Contains(FallbackLine, "keeping the owner contract and the orgs node.yaml names until a heartbeat answers") {
		t.Fatalf("notes %q, want the reworded fallback", reg.Notes)
	}
	if line := StartLine(cfg, reg); !strings.HasSuffix(line, "serving grapevine") {
		t.Fatalf("start line %q, want serving grapevine", line)
	}

	s = &steps{takes: true, answer: nil, resolveErr: errors.New("my orgs: timeout")}
	reg, err = Register(context.Background(), s.registrar(), cfg, cfg.Orgs)
	if err != nil {
		t.Fatalf("Register = %v, want a Resolve error noted, not fatal", err)
	}
	if reg.Contract() != ContractOwner || len(reg.Scopes) != 0 {
		t.Fatalf("contract %q scopes %+v, want the owner contract serving none", reg.Contract(), reg.Scopes)
	}
	if want := "orgs: my orgs: timeout; serving none until a heartbeat answers"; !slices.Contains(reg.Notes, want) {
		t.Fatalf("notes %q, want %q", reg.Notes, want)
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
func (g *ownedGW) Credentials(context.Context, string) ([]gateway.CredentialRow, error) {
	return nil, nil
}
func (g *ownedGW) Goals(context.Context, string) ([]gateway.Goal, error) { return nil, nil }
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
		if got.Contract != ContractOwner || got.NodeID != "b99be3f5-2f54-54a7-9af5-49d9bfc96744" {
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
	if len(lines) != 2 || !strings.Contains(lines[0], " owned by ") || !strings.HasSuffix(lines[0], "serving runsheet, wardroomid") || lines[1] != FallbackLine {
		t.Fatalf("lines %q, want the owner line first, then the fallback", lines)
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
