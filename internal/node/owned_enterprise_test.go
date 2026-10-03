package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// These tests read the enterprise's registration (ADR-015's amendment
// 2026-10-03) through a Registrar of recording functions: the
// enterprise's slug resolved once and cached, its id sent as ownerId,
// and the legacy per-org rows retired on the first owner-path answer
// alone.

const (
	anythinkID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	runsheetID = "33333333-3333-4333-8333-333333333333"
	wardroomID = "77777777-7777-4777-8777-777777777777"
	grapeID    = "88888888-8888-4888-8888-888888888888"
	ownedID    = "55555555-5555-4555-8555-555555555555"
)

// enterpriseSteps is steps plus the enterprise's resolution, the
// person's orgs and the retirements, each recorded.
type enterpriseSteps struct {
	steps
	field     string
	resolved  []string
	resolveE  error
	person    []gateway.Org
	retired   []string
	retireErr map[string]error
}

func (s *enterpriseSteps) registrar() Registrar {
	r := s.steps.registrar()
	r.OwnerField = func(context.Context) string { return s.field }
	r.ResolveEnterprise = func(_ context.Context, slug string) (string, error) {
		s.resolved = append(s.resolved, slug)
		if s.resolveE != nil {
			return "", s.resolveE
		}
		return FindEnterprise(slug, []gateway.Enterprise{{ID: anythinkID, Slug: "anythink", TenantSlug: "anythink-co"}})
	}
	r.LegacyOrgs = func(context.Context) ([]gateway.Org, error) { return s.person, nil }
	r.Retire = func(_ context.Context, id string) error {
		s.retired = append(s.retired, id)
		return s.retireErr[id]
	}
	return r
}

// enterpriseAnswer is the platform's answer for the Mini owned by
// anythink: runsheet and wardroomid.
func enterpriseAnswer() *gateway.NodeAnswer {
	return &gateway.NodeAnswer{
		NodeID: ownedID,
		Owner:  gateway.NodeOwner{Kind: "enterprise", ID: anythinkID, Name: "anythink"},
		Scopes: []gateway.ScopeAnswer{{OrgID: runsheetID, Login: "runsheet"}, {OrgID: wardroomID, Login: "wardroomid"}},
	}
}

func enterpriseCfg() *Config {
	c := ownedCfg()
	c.Name, c.Slug = "mini", "runsheet-mini"
	c.Owner, c.Enterprise = "enterprise", "anythink"
	return c
}

func personOrgs() []gateway.Org {
	return []gateway.Org{{ID: runsheetID, Login: "runsheet"}, {ID: grapeID, TenantSlug: "grapevine"}}
}

// An `enterprise:` slug is resolved through ResolveEnterprise once per
// process, sent as ownerId, cached in node.yaml as enterprise_id by
// SaveRegistration, and a later start reads the cache without resolving.
func TestEnterpriseSlugResolvedOnceAndCached(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := enterpriseCfg()
	s := &enterpriseSteps{steps: steps{takes: true, answer: enterpriseAnswer()}}
	for range 2 {
		reg, err := Register(context.Background(), s.registrar(), cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveRegistration(cfg, reg, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(s.resolved, []string{"anythink"}) {
		t.Fatalf("resolved %q, want once for anythink", s.resolved)
	}
	if !slices.Equal(s.ownerIDs, []string{anythinkID, anythinkID}) {
		t.Fatalf("ownerIds %q, want the enterprise's id each time", s.ownerIDs)
	}
	if cfg.OwnerID != "44444444-4444-4444-8444-444444444444" {
		t.Fatalf("owner_id = %q, want the person's kept", cfg.OwnerID)
	}
	path, _ := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "enterprise_id: "+anythinkID) {
		t.Fatalf("node.yaml lacks enterprise_id:\n%s", data)
	}
	// a later start, on a token that cannot list enterprises, reads it
	saved, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	later := &enterpriseSteps{steps: steps{takes: true, answer: enterpriseAnswer()}, resolveE: errors.New("cannot list")}
	if _, err := Register(context.Background(), later.registrar(), saved, nil); err != nil {
		t.Fatal(err)
	}
	if len(later.resolved) != 0 || !slices.Equal(later.ownerIDs, []string{anythinkID}) {
		t.Fatalf("later start resolved %q and sent %q, want the cache", later.resolved, later.ownerIDs)
	}
	if filepath.Base(path) != "node.yaml" {
		t.Fatalf("config path %s", path)
	}
}

// An `enterprise:` that is a uuid is the id: sent unresolved.
func TestEnterpriseUUIDPassedThrough(t *testing.T) {
	cfg := enterpriseCfg()
	cfg.Enterprise = anythinkID
	s := &enterpriseSteps{steps: steps{takes: true, answer: enterpriseAnswer()}}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.resolved) != 0 || !slices.Equal(s.ownerIDs, []string{anythinkID}) || reg.EnterpriseID != "" {
		t.Fatalf("resolved %q, sent %q, cached %q; want the uuid sent as is", s.resolved, s.ownerIDs, reg.EnterpriseID)
	}
	if id, err := FindEnterprise(anythinkID, nil); err != nil || id != anythinkID {
		t.Fatalf("FindEnterprise(uuid) = %q, %v", id, err)
	}
}

// A slug the token's person does not administer fails the registration,
// naming the slug and that the token must be an enterprise admin's;
// nothing is registered.
func TestEnterpriseUnknownSlugIsFatal(t *testing.T) {
	cfg := enterpriseCfg()
	cfg.Enterprise = "acme"
	s := &enterpriseSteps{steps: steps{takes: true, answer: enterpriseAnswer()}}
	_, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err == nil || !strings.Contains(err.Error(), `enterprise "acme"`) || !strings.Contains(err.Error(), "enterprise admin") || !strings.Contains(err.Error(), "(anythink)") {
		t.Fatalf("err = %v, want one naming acme, the token's enterprises and enterprise admin", err)
	}
	if len(s.ownerIDs) != 0 || cfg.EnterpriseID != "" {
		t.Fatalf("registered %q, cached %q after a refused slug", s.ownerIDs, cfg.EnterpriseID)
	}
	// tenantSlug matches too, case-insensitive
	if id, err := FindEnterprise("Anythink-Co", []gateway.Enterprise{{ID: anythinkID, Slug: "anythink", TenantSlug: "anythink-co"}}); err != nil || id != anythinkID {
		t.Fatalf("FindEnterprise(tenantSlug) = %q, %v", id, err)
	}
}

// A person's node sends node.yaml's owner_id as ownerId and resolves
// nothing.
func TestPersonOwnerIDSent(t *testing.T) {
	cfg := ownedCfg()
	s := &enterpriseSteps{steps: steps{takes: true, answer: answerFixture(t, "registerNode.json")}}
	if _, err := Register(context.Background(), s.registrar(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	if len(s.resolved) != 0 || !slices.Equal(s.ownerIDs, []string{cfg.OwnerID}) {
		t.Fatalf("resolved %q, sent %q, want owner_id", s.resolved, s.ownerIDs)
	}
}

// The first owner-path answer after a per-org contract retires exactly
// NodeIDIn of the answer's orgs and the person's orgs, each once, never
// the owned id, one note each — a refusal noted, not fatal.
func TestFirstOwnerAnswerRetiresLegacyRows(t *testing.T) {
	cfg := enterpriseCfg()
	cfg.Contract = ContractPerOrg
	a := enterpriseAnswer()
	// the owned id is one of the derived ids: never retired
	ownedHere, _ := cfg.NodeIDIn(wardroomID)
	a.NodeID = ownedHere
	idIn := func(org string) string {
		id, err := cfg.NodeIDIn(org)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	s := &enterpriseSteps{steps: steps{takes: true, answer: a}, person: personOrgs(),
		retireErr: map[string]error{idIn(grapeID): errors.New("no such node")}}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{idIn(runsheetID), idIn(grapeID)}; !slices.Equal(s.retired, want) {
		t.Fatalf("retired %q, want %q", s.retired, want)
	}
	for _, want := range []string{
		"retired legacy node " + idIn(runsheetID) + " (org runsheet)",
		"legacy node " + idIn(grapeID) + " (org grapevine): no such node",
	} {
		if !slices.Contains(reg.Notes, want) {
			t.Fatalf("notes %q lack %q", reg.Notes, want)
		}
	}
	lines := RegistrationLines(cfg, reg)
	if !strings.HasPrefix(lines[0], "node mini (runsheet-mini) owned by") {
		t.Fatalf("first line %q, want StartLine", lines[0])
	}
}

// Every org's legacy row is retired when the owned id is none of them.
func TestFirstOwnerAnswerRetiresEveryLegacyRow(t *testing.T) {
	cfg := enterpriseCfg()
	s := &enterpriseSteps{steps: steps{takes: true, answer: enterpriseAnswer()}, person: personOrgs()}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var want, notes []string
	for _, o := range []struct{ id, name string }{{runsheetID, "runsheet"}, {wardroomID, "wardroomid"}, {grapeID, "grapevine"}} {
		id, _ := cfg.NodeIDIn(o.id)
		want = append(want, id)
		notes = append(notes, "retired legacy node "+id+" (org "+o.name+")")
	}
	if !slices.Equal(s.retired, want) {
		t.Fatalf("retired %q, want %q", s.retired, want)
	}
	var got []string
	for _, n := range reg.Notes {
		if strings.Contains(n, "legacy node") {
			got = append(got, n)
		}
	}
	if !slices.Equal(got, notes) {
		t.Fatalf("notes %q, want %q", got, notes)
	}
}

// A registration whose contract was already owner, the per-org path and
// a nil answer retire nothing.
func TestLegacyRowsNotRetiredOtherwise(t *testing.T) {
	for _, c := range []struct {
		name     string
		takes    bool
		answer   *gateway.NodeAnswer
		contract string
	}{
		{"already owner", true, enterpriseAnswer(), ContractOwner},
		{"per-org path", false, nil, ContractPerOrg},
		{"nil answer", true, nil, ContractPerOrg},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := enterpriseCfg()
			cfg.Contract = c.contract
			s := &enterpriseSteps{steps: steps{takes: c.takes, answer: c.answer, scopes: personScopes(t, cfg)}, person: personOrgs()}
			reg, err := Register(context.Background(), s.registrar(), cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.retired) != 0 {
				t.Fatalf("retired %q, want none", s.retired)
			}
			for _, n := range reg.Notes {
				if strings.Contains(n, "legacy node") {
					t.Fatalf("note %q, want no retirement", n)
				}
			}
		})
	}
}

// StartLine on an enterprise's answer names it with its kind, a person
// stays bare, and `owner changed` names the kind too.
func TestEnterpriseStartLineAndOwnerChanged(t *testing.T) {
	cfg := enterpriseCfg()
	s := &enterpriseSteps{steps: steps{takes: true, answer: enterpriseAnswer()}}
	reg, err := Register(context.Background(), s.registrar(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := StartLine(cfg, reg), "node mini (runsheet-mini) owned by anythink (enterprise), serving runsheet, wardroomid"; got != want {
		t.Fatalf("StartLine = %q, want %q", got, want)
	}
	if got := (Owner{Kind: "person", Name: "Chris"}).Who(); got != "Chris" {
		t.Fatalf("person Who = %q", got)
	}
	loop, buf := ownedLoop(t, answerFixture(t, "registerNode.json"))
	loop.TakeAnswer(enterpriseAnswer())
	if want := "owner changed: Chris → anythink (enterprise)"; !slices.Contains(logLines(buf), want) {
		t.Fatalf("log lacks %q:\n%s", want, buf)
	}
}

// The registration lines name the owner field the platform took, on
// either contract: `owner contract: field ownerKind`, `field owner`.
func TestRegistrationLinesNameOwnerField(t *testing.T) {
	for _, c := range []struct {
		field string
		takes bool
		want  string
	}{
		{"ownerKind", true, "owner contract: field ownerKind"},
		{"owner", true, "owner contract: field owner"},
		{"", false, "owner contract: no owner field (registerNode takes neither ownerKind nor owner); registering per org"},
	} {
		cfg := enterpriseCfg()
		cfg.Contract = ContractOwner
		s := &enterpriseSteps{steps: steps{takes: c.takes, answer: enterpriseAnswer(), scopes: personScopes(t, cfg)}, field: c.field}
		reg, err := Register(context.Background(), s.registrar(), cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		lines := RegistrationLines(cfg, reg)
		if len(lines) < 2 || lines[1] != c.want {
			t.Fatalf("field %q: lines %q, want %q after StartLine", c.field, lines, c.want)
		}
	}
}
