package node

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Owned is a node registered once with its owner (ADR-015, Registration
// and the loop): its one id, the same in every org it serves, and who
// owns it as the platform last answered.
type Owned struct {
	NodeID string
	Owner  Owner
}

// Registrar is what a registration does, one function per step, so the
// decision between the owner contract and the legacy per-org path
// (Register) is read without a gateway.
type Registrar struct {
	// TakesOwner is the probe: whether the platform's RegisterNodeInput
	// has an owner field, `ownerKind` or `owner` (gateway.Client.TakesOwner).
	TakesOwner func(ctx context.Context) bool
	// Resolve lists the orgs of the token's person, restricted by
	// node.yaml's orgs (ResolveScopes). The per-org path reads it, and
	// the owner path when its answer is nil.
	Resolve func(ctx context.Context) (Scopes, error)
	// ConfiguredOrg is an org id for the owner registration's orgId: an
	// org the node is configured to serve, never an arbitrary one of the
	// token's person's. When node.yaml's orgs: names any, it is the id of
	// the first of the token's person's orgs matching one of them
	// (ConfiguredOrgID); when orgs: is empty, the first of the person's
	// orgs, unrestricted, as before; when orgs: names some but none
	// matches, "" and no error — Register notes it
	// (noConfiguredOrgNote), never sending an unconfigured org. The
	// platform ignores orgId on the owner contract: the answer is what
	// names the orgs served.
	ConfiguredOrg func(ctx context.Context) (string, error)
	// RegisterOwner is the one registration on the owner contract: the
	// node's own id, an org id the platform ignores, and the owner's id
	// to send as ownerId — the enterprise's (ResolveEnterprise) on
	// `owner: enterprise`, node.yaml's owner_id for a person.
	RegisterOwner func(ctx context.Context, nodeID, orgID, ownerID string) (*gateway.NodeAnswer, error)
	// RegisterIn registers one Node per org, as before owners.
	RegisterIn func(ctx context.Context, scopes Scopes) error
	// OwnerField is the owner field the platform's RegisterNodeInput
	// takes — "ownerKind", "owner" or "" (gateway.Client.OwnerField) —
	// said once per registration as OwnerFieldLine. Nil says nothing.
	OwnerField func(ctx context.Context) string
	// ResolveEnterprise is node.yaml's `enterprise:` slug as an id
	// (gateway.Client.MyEnterprises, by slug or tenantSlug). Asked only
	// when the slug is not already an id and no enterprise_id is cached.
	ResolveEnterprise func(ctx context.Context, slugOrID string) (string, error)
	// ResolveOrg is node.yaml's `org:` slug as an id on `owner: org`,
	// from the machine token's orgs read (gateway.Client.MyOrgs, by
	// slug, tenantSlug, login or id; FindOrg) — never signIn. Asked
	// only when the slug is not already an id and no org_id is cached.
	ResolveOrg func(ctx context.Context, slugOrID string) (string, error)
	// LegacyOrgs is the token's person's orgs, unrestricted (MyOrgs): with
	// the answer's orgs, where the node may have a legacy per-org row.
	LegacyOrgs func(ctx context.Context) ([]gateway.Org, error)
	// Retire retires one of the node's own legacy per-org rows
	// (gateway.Client.RetireNode). Nil retires nothing.
	Retire func(ctx context.Context, nodeID string) error
	// CredentialID is the machine id of the node's own Latchkey
	// credential, empty on a console token: sent on every registerNode
	// (RegisterOwner and RegisterIn put it on RegisterNodeInput) so the
	// owner row names this machine and heartbeats and claims pass the
	// gateway's machineIsNode rule (runsheet#2290, adr-039).
	CredentialID string
	// Machine is whether the node's token is a Latchkey machine
	// credential (token file kind "machine"): a `not yours` refusal on
	// one is explained by NotYoursLine, even with no CredentialID.
	Machine bool
	// CredentialLine says once per registration which credential the
	// node registers on and whether credentialId went (CredentialLine's
	// function of the same name). Nil says nothing.
	CredentialLine func(ctx context.Context) string
}

// NotYoursLine is what an operator needs beside a registration the
// gateway refused `not yours` on a machine credential (runsheet#2290's
// registersNode machine branch): the credentialId the node sent — the
// machine id in ~/.mesh/token — and where to compare it with the owner
// row's credential_id; or, when none was sent, why the token yielded
// none. "" for a nil error, one that does not say `not yours`, or a
// console token.
func NotYoursLine(err error, credentialID string, machine bool, slug string) string {
	if err == nil || !machine || !strings.Contains(err.Error(), "not yours") {
		return ""
	}
	row := "the node row's credential_id on the console (Nodes → " + slug + ")"
	if credentialID == "" {
		return "sent no credentialId: the token file has no machineId and the access token's sub is not machine:<id>; " +
			"run mesh node login on this machine, then compare its machine id with " + row
	}
	return "sent credentialId " + credentialID + " (the machine id in ~/.mesh/token); compare it with " + row +
		"; a row naming another credential needs mesh node login on that machine or the row's credential cleared"
}

// explainNotYours appends NotYoursLine to a registration's error, so the
// line reaches node.log with it; any other error is returned as it is.
func explainNotYours(err error, r Registrar, cfg *Config) error {
	line := NotYoursLine(err, r.CredentialID, r.Machine, cfg.Slug)
	if line == "" {
		return err
	}
	return fmt.Errorf("%w — %s", err, line)
}

// Registered is Register's result: the scopes to serve and, on the
// owner contract, the node's one id and its owner and the platform's
// answer itself (both nil on the legacy per-org path). The answer is
// handed to the loop before its first tick (Loop.TakeAnswer), so a
// reload that changes node.yaml's orgs restricts it even while every
// heartbeat since answers nil.
type Registered struct {
	Scopes Scopes
	Owned  *Owned
	Answer *gateway.NodeAnswer
	// Notes are what the registration has to say — the nil answer's
	// FallbackLine, an orgs restriction the answer did not meet — for
	// the caller to log after StartLine, so node.log's first line after
	// the start lines is always the owner and the orgs (ADR-015).
	Notes []string
	// EnterpriseID is the enterprise id this registration resolved from
	// node.yaml's `enterprise:` slug, "" when none was resolved (an id
	// as written, a cached enterprise_id, a person's node); SaveRegistration
	// caches it in node.yaml as enterprise_id.
	EnterpriseID string
	// OrgID is the org id this registration resolved from node.yaml's
	// `org:` slug on `owner: org`, "" when none was resolved (an id as
	// written, a cached org_id); SaveRegistration caches it as org_id.
	OrgID string
	// IDSource is how the owner id was arrived at — IDDerived when
	// OwnedNodeID computed it, IDAdopted when a refusal named it
	// (AdoptedID) — and node.yaml's node_id_source as it was when the
	// registration went under the saved node_id. SaveRegistration writes
	// it.
	IDSource string
	// adopted is set when this registration adopted the id a refusal
	// named: cfg.NodeID already holds it, so SaveRegistration saves.
	adopted bool
}

// How the owner id was arrived at (Registered.IDSource, node.yaml's
// node_id_source).
const (
	IDDerived = "derived"
	IDAdopted = "adopted"
)

// registersAt is the platform's refusal of an owner registration under
// an id other than the one it derives (platform/aggregates/node.go):
// `node <slug> registers at <uuid>, the id its form names, not <uuid>`.
var registersAt = regexp.MustCompile(`registers at ([0-9A-Fa-f-]{36})`)

// AdoptedID is the id a refused registration names — the uuid after
// `registers at ` in err's text — and whether there is one: false for a
// nil error, a refusal that names none, or one whose id does not parse.
func AdoptedID(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	m := registersAt.FindStringSubmatch(err.Error())
	if m == nil {
		return "", false
	}
	id, perr := uuid.Parse(m[1])
	if perr != nil {
		return "", false
	}
	return id.String(), true
}

// AdoptionNote is the note a registration that adopted the platform's
// id logs after StartLine.
func AdoptionNote(adopted, sent string) string {
	return fmt.Sprintf("owner id adopted from the platform: %s (derived %s)", adopted, sent)
}

// Contract is the contract the registration made: ContractOwner when
// the platform answered the owner registration, else ContractPerOrg.
func (r Registered) Contract() string {
	if r.Owned != nil {
		return ContractOwner
	}
	return ContractPerOrg
}

// StartLine is node.log's first line after the start lines (mesh-specs
// ADR 014, TRD §3): `node mini (runsheet-mini) owned by Chris, serving
// runsheet, wardroomid, grapevine` on the owner contract, `node mini
// (runsheet-mini) per org, serving …` on the legacy path; every org by
// Scope.Name, so one without a GitHub login is its slug.
func StartLine(cfg *Config, r Registered) string {
	if r.Owned != nil {
		return fmt.Sprintf("node %s (%s) owned by %s, serving %s", cfg.Name, cfg.Slug, r.Owned.Owner.Who(), orgsOrNone(r.Scopes))
	}
	return fmt.Sprintf("node %s (%s) per org, serving %s", cfg.Name, cfg.Slug, orgsOrNone(r.Scopes))
}

// OwnerFieldLine names the owner field the platform's RegisterNodeInput
// took on this registration: `owner contract: field ownerKind`, `owner
// contract: field owner`, or that it takes neither and the node
// registers per org.
func OwnerFieldLine(field string) string {
	if field == "" {
		return "owner contract: no owner field (registerNode takes neither ownerKind nor owner); registering per org"
	}
	return "owner contract: field " + field
}

// CredentialLine names the credential a registration went on, as
// OwnerFieldLine names the owner field: `credential: machine <id> sent
// as credentialId`; on a gateway whose RegisterNodeInput has no
// credentialId, that it was left out; on a machine credential whose id
// could not be read, that none was sent; and on a console token
// `credential: rs_* console token, no credentialId`.
func CredentialLine(machine bool, id string, taken bool) string {
	switch {
	case !machine:
		return "credential: rs_* console token, no credentialId"
	case id == "":
		return "credential: machine with no machine id in the token file or its access token; no credentialId"
	case !taken:
		return "credential: machine " + id + "; credentialId left out: the gateway's RegisterNodeInput has no credentialId (a gateway before runsheet#2290)"
	}
	return "credential: machine " + id + " sent as credentialId"
}

// enterpriseNotAdmin is the refusal of an `enterprise:` the token's
// person does not administer.
const enterpriseNotAdmin = "enterprise %q: not one of the token's enterprises (%s) — the token must be an enterprise admin's"

// EnterpriseNotFound is ResolveEnterprise's refusal of slug, naming the
// enterprises the token's person does administer.
func EnterpriseNotFound(slug string, es []gateway.Enterprise) error {
	var names []string
	for _, e := range es {
		names = append(names, e.Slug)
	}
	listed := "none"
	if len(names) > 0 {
		listed = strings.Join(names, ", ")
	}
	return fmt.Errorf(enterpriseNotAdmin, slug, listed)
}

// FindEnterprise is slugOrID's id among es: an id is itself, else the
// enterprise whose slug or tenantSlug matches it, case-insensitive; no
// match is EnterpriseNotFound.
func FindEnterprise(slugOrID string, es []gateway.Enterprise) (string, error) {
	slugOrID = strings.TrimSpace(slugOrID)
	if _, err := uuid.Parse(slugOrID); err == nil {
		return slugOrID, nil
	}
	for _, e := range es {
		if strings.EqualFold(e.Slug, slugOrID) || (e.TenantSlug != "" && strings.EqualFold(e.TenantSlug, slugOrID)) {
			return e.ID, nil
		}
	}
	return "", EnterpriseNotFound(slugOrID, es)
}

// ErrOwnerOrgGateway refuses `mesh node start` on `owner: org` when the
// gateway's RegisterNodeInput takes no ownerKind org
// (gateway.Client.TakesOwnerOrg): one line naming the gateway it needs
// (gateway.OwnerOrgGateway), checked before any registerNode is sent.
var ErrOwnerOrgGateway = errors.New("owner: org needs a gateway that takes ownerKind org (" + gateway.OwnerOrgGateway + "); this gateway does not")

// orgNotFound is ResolveOrg's refusal of an `org:` none of the token's
// orgs matches.
const orgNotFound = "org %q: not one of the token's orgs (%s) — the machine credential must be one of the org's"

// FindOrg is slugOrID's id among orgs: an id is itself, else the org
// whose slug, tenantSlug or login matches it, case-insensitive; no match
// names the orgs the token does reach.
func FindOrg(slugOrID string, orgs []gateway.Org) (string, error) {
	slugOrID = strings.TrimSpace(slugOrID)
	if _, err := uuid.Parse(slugOrID); err == nil {
		return slugOrID, nil
	}
	var names []string
	for _, o := range orgs {
		for _, n := range []string{o.Slug, o.TenantSlug, o.Login} {
			if n != "" && strings.EqualFold(n, slugOrID) {
				return o.ID, nil
			}
		}
		names = append(names, o.Name())
	}
	listed := "none"
	if len(names) > 0 {
		listed = strings.Join(names, ", ")
	}
	return "", fmt.Errorf(orgNotFound, slugOrID, listed)
}

// ownerIDFor is the ownerId the owner registration sends: node.yaml's
// owner_id for a person; for an org its id — `org:` when it is one, else
// the cached org_id, else ResolveOrg once, cached on cfg and returned as
// resolved; for an enterprise its id — `enterprise:` when
// it is one, else the cached enterprise_id, else ResolveEnterprise once,
// cached on cfg for the rest of the process and returned as resolved
// for SaveRegistration to write. owner_id (the person) is never touched,
// so OwnedNodeID keeps converging.
func ownerIDFor(ctx context.Context, r Registrar, cfg *Config) (id, resolved string, err error) {
	if cfg.OwnerKind() == "org" {
		return orgOwnerID(ctx, r, cfg)
	}
	if cfg.OwnerKind() != "enterprise" {
		return cfg.OwnerID, "", nil
	}
	ent := strings.TrimSpace(cfg.Enterprise)
	if _, err := uuid.Parse(ent); err == nil {
		return ent, "", nil
	}
	if cfg.EnterpriseID != "" {
		return cfg.EnterpriseID, "", nil
	}
	if r.ResolveEnterprise == nil {
		return "", "", fmt.Errorf("enterprise %q: no way to resolve it to an id", ent)
	}
	id, err = r.ResolveEnterprise(ctx, ent)
	if err != nil {
		return "", "", err
	}
	cfg.EnterpriseID = id
	return id, id, nil
}

// orgOwnerID is ownerIDFor on `owner: org` (goal node-yaml-says-owner-org).
func orgOwnerID(ctx context.Context, r Registrar, cfg *Config) (id, resolved string, err error) {
	org := strings.TrimSpace(cfg.OrgLogin)
	if _, err := uuid.Parse(org); err == nil {
		return org, "", nil
	}
	if cfg.OrgID != "" {
		return cfg.OrgID, "", nil
	}
	if r.ResolveOrg == nil {
		return "", "", fmt.Errorf("org %q: no way to resolve it to an id", org)
	}
	id, err = r.ResolveOrg(ctx, org)
	if err != nil {
		return "", "", err
	}
	cfg.OrgID = id
	return id, id, nil
}

// retireLegacy retires the node's own legacy per-org rows: NodeIDIn of
// every org the answer names and every org of the token's person
// (LegacyOrgs), each once, never the owned id. Each id is derived from
// (org, slug), so it is this node's and never another's. One note per
// id: `retired legacy node <id> (org <name>)`, or a refusal — already
// retired, no such node — as `legacy node <id> (org <name>): <err>`,
// never fatal.
func retireLegacy(ctx context.Context, r Registrar, cfg *Config, a *gateway.NodeAnswer, owned string) []string {
	type org struct{ id, name string }
	var orgs []org
	for _, sa := range a.Scopes {
		orgs = append(orgs, org{sa.OrgID, Scope{Login: sa.Login, Slug: sa.Slug}.Name()})
	}
	var notes []string
	if r.LegacyOrgs != nil {
		person, err := r.LegacyOrgs(ctx)
		if err != nil {
			notes = append(notes, fmt.Sprintf("legacy nodes: my orgs: %v; retiring those of the answer's orgs alone", err))
		}
		for _, o := range person {
			orgs = append(orgs, org{o.ID, o.Name()})
		}
	}
	seen := map[string]bool{owned: true}
	for _, o := range orgs {
		id, err := cfg.NodeIDIn(o.id)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		if err := r.Retire(ctx, id); err != nil {
			notes = append(notes, fmt.Sprintf("legacy node %s (org %s): %v", id, o.name, err))
			continue
		}
		notes = append(notes, fmt.Sprintf("retired legacy node %s (org %s)", id, o.name))
	}
	return notes
}

// RegistrationLines is what a start logs of its registration, in
// order: StartLine first — node.log's first line after the start lines
// names the owner and the orgs — then the registration's Notes.
func RegistrationLines(cfg *Config, r Registered) []string {
	return append([]string{StartLine(cfg, r)}, r.Notes...)
}

// SaveRegistration records what a registration made in node.yaml —
// Contract and, on the owner contract, the node's one id — and saves it
// through save (Config.Save when nil) when either changed. A failed
// save is returned: `mesh node status` and the rollback reporter read
// the contract from the file (TRD §3 step 1).
func SaveRegistration(cfg *Config, r Registered, save func() error) error {
	contract := r.Contract()
	changed := cfg.Contract != contract
	cfg.Contract = contract
	if r.Owned != nil && r.Owned.NodeID != "" && cfg.NodeID != r.Owned.NodeID {
		cfg.NodeID, changed = r.Owned.NodeID, true
	}
	if r.EnterpriseID != "" {
		// Register cached it on cfg already; the file has not got it yet
		cfg.EnterpriseID, changed = r.EnterpriseID, true
	}
	if r.OrgID != "" {
		// Register cached it on cfg already, as EnterpriseID
		cfg.OrgID, changed = r.OrgID, true
	}
	if r.adopted {
		// Register set cfg.NodeID to the adopted id already
		changed = true
	}
	if r.IDSource != "" && cfg.NodeIDSource != r.IDSource {
		cfg.NodeIDSource, changed = r.IDSource, true
	}
	if !changed {
		return nil
	}
	if save == nil {
		save = cfg.Save
	}
	if err := save(); err != nil {
		return fmt.Errorf("save node.yaml (contract %s, node id %s): %w", contract, cfg.NodeID, err)
	}
	return nil
}

// SaveAdoption saves the id a registration adopted when the retry under
// it was refused too: node.yaml's node_id and node_id_source, never the
// contract, which no registration made — so the next start, or the
// confirm window's next attempt, registers under the adopted id, and a
// move to the owner contract still retires the legacy rows when one
// goes through. A registration that adopted nothing saves nothing.
func SaveAdoption(cfg *Config, r Registered, save func() error) error {
	if !r.adopted || r.Owned == nil {
		return nil
	}
	cfg.NodeID, cfg.NodeIDSource = r.Owned.NodeID, IDAdopted
	if save == nil {
		save = cfg.Save
	}
	if err := save(); err != nil {
		return fmt.Errorf("save node.yaml (adopted node id %s): %w", cfg.NodeID, err)
	}
	return nil
}

// FallbackLine is logged when the platform takes `owner` but answers
// the owner registration nil: a half-deployed platform. The probe chose
// the contract, so the node stays on it under its one id, serving the
// orgs node.yaml names as the per-org contract resolves them (Resolve),
// until a heartbeat's answer names its orgs; it never registers per org.
const FallbackLine = "register: the platform took owner but answered nil; keeping the owner contract and the orgs node.yaml names until a heartbeat answers"

// noConfiguredOrgNote is logged when node.yaml's orgs: names an org
// none of the token's person's orgs matches: the owner registration
// sends no orgId rather than an arbitrary one of the person's orgs.
const noConfiguredOrgNote = "register: none of the orgs node.yaml names is one of the person's; sending no orgId"

// ConfiguredOrgID is the org id the owner registration's orgId sends,
// from the token's person's orgs and node.yaml's orgs: restriction —
// never an org the node is not configured to serve. When configured
// names any, it is the id of the first of orgs whose Login or
// TenantSlug matches one of configured (case-insensitive, trimmed, the
// same rule ResolveScopes applies), and matched is true; none matching
// is "" and matched false. When configured is empty, it is the id of
// the first of orgs, unrestricted, as before (matched true), or "" and
// matched false when orgs is empty.
func ConfiguredOrgID(orgs []gateway.Org, configured []string) (id string, matched bool) {
	if len(configured) == 0 {
		if len(orgs) == 0 {
			return "", false
		}
		return orgs[0].ID, true
	}
	want := map[string]bool{}
	for _, o := range configured {
		want[strings.ToLower(strings.TrimSpace(o))] = true
	}
	for _, o := range orgs {
		login, slug := strings.ToLower(o.Login), strings.ToLower(o.TenantSlug)
		if want[login] || want[slug] {
			return o.ID, true
		}
	}
	return "", false
}

// OwnedNodeID is the id the node registers under on the owner
// contract: node.yaml's node_id when set, else
// gateway.NodeOwnerID(kind, ownerID, slug) — the platform's own formula
// (platform/ids/node.go) over the ownerId the registration actually
// sends (ownerIDFor: the enterprise's id on `owner: enterprise`,
// node.yaml's owner_id for a person), so it converges on the row the
// platform derives, else a fresh one. Registered.IDSource names the
// latter two IDDerived.
func OwnedNodeID(cfg *Config, kind, ownerID string) string {
	if cfg.NodeID != "" {
		return cfg.NodeID
	}
	if owner, err := uuid.Parse(ownerID); err == nil {
		return gateway.NodeOwnerID(kind, owner, cfg.Slug).String()
	}
	return uuid.NewString()
}

// Register registers the node. The probe comes first. A platform that
// does not take `owner` gets today's path exactly: Resolve, then
// RegisterIn per org, no owner sent. One that does gets one
// RegisterOwner under OwnedNodeID (orgId from ConfiguredOrg), and its answer's
// scopes, restricted by restrict, are what the node serves: node.yaml's
// orgs are checked against the answer, never against the person's
// orgs, so a restriction naming an enterprise org the person is not in
// holds. The probe alone chooses the contract: a nil answer from it
// notes FallbackLine and keeps the owner contract under the node's one
// id, serving what Resolve gives — node.yaml's orgs among the person's,
// each on its per-org id, as the per-org contract served them; a Resolve
// error is noted and serves none — never RegisterIn, so the loop
// heartbeats that id and takes the first answer that names its orgs. A
// non-nil answer is the owner contract whatever it names: an answer
// naming no org, or a restriction matching none of the orgs it names,
// is noted and the node serves none until a heartbeat's answer names
// one — never the per-org path, never a failed registration.
//
// On `owner: enterprise` the ownerId sent is the enterprise's id
// (ownerIDFor: resolved once, cached as enterprise_id). The first
// non-nil answer after a per-org contract — node.yaml's contract as it
// was before this registration — retires the node's legacy per-org rows
// (retireLegacy), one note each; a restart already on the owner
// contract, a nil answer and the per-org path retire nothing.
func Register(ctx context.Context, r Registrar, cfg *Config, restrict []string) (Registered, error) {
	was := cfg.ContractKind()
	var fieldNote []string
	if r.OwnerField != nil {
		fieldNote = []string{OwnerFieldLine(r.OwnerField(ctx))}
	}
	if r.CredentialLine != nil {
		fieldNote = append(fieldNote, r.CredentialLine(ctx))
	}
	if !r.TakesOwner(ctx) {
		reg, err := perOrg(ctx, r)
		reg.Notes = append(fieldNote, reg.Notes...)
		return reg, explainNotYours(err, r, cfg)
	}
	ownerID, resolved, err := ownerIDFor(ctx, r, cfg)
	if err != nil {
		return Registered{}, fmt.Errorf("register (owner %s): %w", cfg.OwnerKind(), err)
	}
	var orgID string
	if cfg.OwnerKind() == "org" {
		// the org owns it: the one org it serves is the orgId it sends
		orgID = ownerID
	} else {
		if orgID, err = r.ConfiguredOrg(ctx); err != nil {
			return Registered{}, err
		}
		if orgID == "" && len(restrict) > 0 {
			fieldNote = append(fieldNote, noConfiguredOrgNote)
		}
	}
	entID, orgResolved := resolved, ""
	if cfg.OwnerKind() == "org" {
		entID, orgResolved = "", resolved
	}
	source := cfg.NodeIDSource
	if cfg.NodeID == "" {
		source = IDDerived
	}
	id := OwnedNodeID(cfg, cfg.OwnerKind(), ownerID)
	answer, err := r.RegisterOwner(ctx, id, orgID, ownerID)
	adopted := false
	if named, ok := AdoptedID(err); ok && named != id {
		fieldNote = append(fieldNote, AdoptionNote(named, id))
		id, source, adopted = named, IDAdopted, true
		cfg.NodeID = id
		answer, err = r.RegisterOwner(ctx, id, orgID, ownerID)
	}
	if err != nil {
		var partial Registered
		if adopted {
			partial = Registered{Owned: &Owned{NodeID: id, Owner: Owner{Kind: cfg.OwnerKind()}}, Notes: fieldNote, EnterpriseID: entID, OrgID: orgResolved, IDSource: source, adopted: true}
		}
		return partial, explainNotYours(fmt.Errorf("register (owner %s): %w", cfg.OwnerKind(), err), r, cfg)
	}
	if answer == nil {
		owned := &Owned{NodeID: id, Owner: Owner{Kind: cfg.OwnerKind()}}
		reg := Registered{Owned: owned, Notes: append(fieldNote, FallbackLine), EnterpriseID: entID, OrgID: orgResolved, IDSource: source, adopted: adopted}
		// the orgs it served per org, on their per-org ids, so its claims
		// land on the rows that served them (ADR-015, 2026-10-03)
		if r.Resolve != nil {
			scopes, err := r.Resolve(ctx)
			if err != nil {
				reg.Notes = append(reg.Notes, fmt.Sprintf("orgs: %v; serving none until a heartbeat answers", err))
			}
			reg.Scopes = scopes
		}
		return reg, nil
	}
	reg := Registered{Owned: &Owned{NodeID: answer.NodeID, Owner: OwnerFrom(answer)}, Answer: answer, Notes: fieldNote, EnterpriseID: entID, OrgID: orgResolved, IDSource: source, adopted: adopted}
	var note error
	if reg.Scopes, note = ServedFrom(answer, restrict); note != nil {
		reg.Notes = append(reg.Notes, fmt.Sprintf("orgs: %v", note))
	}
	if was != ContractOwner && r.Retire != nil {
		owned := answer.NodeID
		if owned == "" {
			owned = id
		}
		reg.Notes = append(reg.Notes, retireLegacy(ctx, r, cfg, answer, owned)...)
	}
	return reg, nil
}

// perOrg is the registration before owners: the person's orgs, a Node
// in each.
func perOrg(ctx context.Context, r Registrar) (Registered, error) {
	scopes, err := r.Resolve(ctx)
	if err != nil {
		return Registered{}, err
	}
	return Registered{Scopes: scopes}, r.RegisterIn(ctx, scopes)
}

// ServedFrom is the scopes an answer names, restricted by node.yaml's
// orgs, each on the node's one id. It is never fatal: the scopes are
// what it could keep — none when the answer names no org or the
// restriction matches none of them — and the error, when not nil, is a
// note for the log naming what was left out.
func ServedFrom(a *gateway.NodeAnswer, restrict []string) (Scopes, error) {
	if a == nil {
		return nil, nil
	}
	scopes, err := ScopesFromAnswer(a, restrict)
	if err == nil {
		return scopes, nil
	}
	if len(a.Scopes) == 0 {
		return nil, err
	}
	want := map[string]bool{}
	for _, o := range restrict {
		want[strings.ToLower(strings.TrimSpace(o))] = true
	}
	var kept Scopes
	for _, sa := range a.Scopes {
		if len(want) == 0 || want[strings.ToLower(sa.Login)] || want[strings.ToLower(sa.Slug)] {
			kept = append(kept, Scope{Login: sa.Login, Slug: sa.Slug, ID: sa.OrgID, AccountID: sa.AccountID, NodeID: a.NodeID})
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("%v; the platform serves %s, so none is served", err, strings.Join(answerNames(a), ", "))
	}
	return kept, fmt.Errorf("%v; serving the rest", err)
}

// answerNames is each org an answer names, by Scope.Name.
func answerNames(a *gateway.NodeAnswer) []string {
	var names []string
	for _, sa := range a.Scopes {
		names = append(names, Scope{Login: sa.Login, Slug: sa.Slug}.Name())
	}
	return names
}

// LegacyIDs are the per-org node ids an owned node registered under
// before it was owned — Config.NodeIDIn of each scope's org — mapped to
// the org's id: sessions started under them are still this node's to
// adopt. The owner contract's scopes all carry the one id instead.
func LegacyIDs(cfg *Config, scopes Scopes) map[string]string {
	out := map[string]string{}
	for _, sc := range scopes {
		if id, err := cfg.NodeIDIn(sc.ID); err == nil && id != sc.NodeID {
			out[id] = sc.ID
		}
	}
	return out
}

// TakeAnswer takes the platform's answer to a registration or a
// heartbeat on the owner contract: the scopes it names (restricted by
// node.yaml's orgs, as last reloaded) replace the loop's, logged `orgs a
// → b` as a reload logs them, and an owner whose id or kind changed —
// a node handed from a person to an enterprise — is logged `owner
// changed: Chris → anythink`, with no restart. A nil answer keeps the last scopes; a
// non-nil one replaces them whatever it names, so an org the platform no
// longer serves is claimed from no more — one naming no org, or none
// the restriction keeps, leaves the node serving none. A restriction
// naming an org not served is logged, not fatal. The loop is Owned from
// then on.
func (l *Loop) TakeAnswer(a *gateway.NodeAnswer) {
	if a == nil {
		return
	}
	l.mu.Lock()
	l.answer = a
	restrict := l.restrict
	if !l.restrictSet && l.Cfg != nil {
		restrict = l.Cfg.Orgs
	}
	l.mu.Unlock()
	owner, was := OwnerFrom(a), l.owned()
	if was != nil && (was.Owner.ID != owner.ID || was.Owner.Kind != owner.Kind) {
		l.logf("owner changed: %s → %s", was.Owner.Who(), owner.Who())
	}
	id := a.NodeID
	if id == "" && was != nil {
		id = was.NodeID
	}
	l.mu.Lock()
	l.Owned = &Owned{NodeID: id, Owner: owner}
	l.mu.Unlock()
	scopes, err := ServedFrom(a, restrict)
	if err != nil {
		l.logf("orgs: %v", err)
	}
	if !sameOrgs(scopes, l.Scopes) {
		l.logf("orgs %s → %s", orgsOrNone(l.Scopes), orgsOrNone(scopes))
	}
	// the NodeID is the node's one id either way; the scopes are taken
	// whole so a changed login or slug is taken too
	l.mu.Lock()
	l.Scopes = scopes
	l.mu.Unlock()
	l.takeAnswerShares(a)
}

// owned is the loop's Owned under its lock.
func (l *Loop) owned() *Owned {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Owned
}

// orgsOrNone is Names joined by ", ", or "none" for no scope.
func orgsOrNone(s Scopes) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s.Names(), ", ")
}

// OutsideGateway is what a heartbeat outside the loop needs: the probe,
// the heartbeat and, on the per-org path alone, the person's orgs.
type OutsideGateway interface {
	TakesOwner(ctx context.Context) bool
	HeartbeatNode(ctx context.Context, nodeID, version string, running int, extras ...gateway.HeartbeatExtras) (*gateway.NodeAnswer, error)
	MyOrgs(ctx context.Context) ([]gateway.Org, error)
}

// Outside is HeartbeatOutside's result. Owned is the owner contract:
// the one heartbeat went under the saved id, and Answer is what it
// answered — nil included, which serves no Scopes here but never sends
// the command per org. Restrict is node.yaml's orgs the answer did not
// meet, said and not fatal.
type Outside struct {
	Owned    bool
	Answer   *gateway.NodeAnswer
	Scopes   Scopes
	Restrict error
}

// HeartbeatOutside is the heartbeat of a command outside the loop —
// `mesh node status`, the rollback reporter (ADR-015, Registration and
// the loop). A node whose last registration saved the owner contract,
// on a platform that still takes `owner`, heartbeats once, under its one
// id, whatever the answer — a nil answer keeps the contract and never
// falls through to MyOrgs or per-org heartbeats, which would send
// several and fail on an owner's orgs outside the token's person's.
// Any other node resolves its orgs and heartbeats in each; failed is
// told each org's error and stops the round when it returns one.
func HeartbeatOutside(ctx context.Context, gw OutsideGateway, cfg *Config, version string, extras gateway.HeartbeatExtras, failed func(Scope, error) error) (Outside, error) {
	if id := cfg.OwnedID(); id != "" && gw.TakesOwner(ctx) {
		answer, err := gw.HeartbeatNode(ctx, id, version, 0, extras)
		if err != nil {
			return Outside{}, fmt.Errorf("heartbeat: %w", err)
		}
		out := Outside{Owned: true, Answer: answer}
		if answer != nil {
			out.Scopes, out.Restrict = ServedFrom(answer, cfg.Orgs)
		}
		return out, nil
	}
	scopes, err := ResolveScopes(ctx, gw, cfg)
	if err != nil {
		return Outside{}, err
	}
	for _, sc := range scopes {
		if _, err := gw.HeartbeatNode(ctx, sc.NodeID, version, 0, extras); err != nil {
			if err := failed(sc, err); err != nil {
				return Outside{}, err
			}
		}
	}
	return Outside{Scopes: scopes}, nil
}
