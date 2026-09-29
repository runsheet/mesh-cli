package node

import (
	"context"
	"fmt"
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
	// has `owner` (gateway.Client.TakesOwner).
	TakesOwner func(ctx context.Context) bool
	// Resolve lists the orgs of the token's person, restricted by
	// node.yaml's orgs (ResolveScopes). Only the per-org path reads it.
	Resolve func(ctx context.Context) (Scopes, error)
	// AnyOrg is an org id for the owner registration's orgId, which the
	// platform ignores on the owner contract: the first of the token's
	// person's orgs, unrestricted, or "" when there is none. No
	// restriction applies, since the answer is what names the orgs.
	AnyOrg func(ctx context.Context) (string, error)
	// RegisterOwner is the one registration on the owner contract: the
	// node's own id, an org id the platform ignores, the owner's kind.
	RegisterOwner func(ctx context.Context, nodeID, orgID string) (*gateway.NodeAnswer, error)
	// RegisterIn registers one Node per org, as before owners.
	RegisterIn func(ctx context.Context, scopes Scopes) error
	// Logf logs a line (optional).
	Logf func(format string, args ...any)
}

// Registered is Register's result: the scopes to serve and, on the
// owner contract, the node's one id and its owner (nil on the legacy
// per-org path).
type Registered struct {
	Scopes Scopes
	Owned  *Owned
}

// FallbackLine is logged once when the platform takes `owner` but its
// answer names no scopes: a half-deployed platform.
const FallbackLine = "register: the platform took owner but answered no scopes; serving per org until it does"

// OwnedNodeID is the id the node registers under on the owner
// contract: node.yaml's node_id when set, else one from (owner, slug),
// so it converges, else a fresh one.
func OwnedNodeID(cfg *Config) string {
	if cfg.NodeID != "" {
		return cfg.NodeID
	}
	if owner, err := uuid.Parse(cfg.OwnerID); err == nil {
		return gateway.NodeID(owner, cfg.Slug).String()
	}
	return uuid.NewString()
}

// Register registers the node. The probe comes first. A platform that
// does not take `owner` gets today's path exactly: Resolve, then
// RegisterIn per org, no owner sent. One that does gets one
// RegisterOwner under OwnedNodeID (orgId from AnyOrg), and its answer's
// scopes, restricted by restrict, are what the node serves: node.yaml's
// orgs are checked against the answer, never against the person's
// orgs, so a restriction naming an enterprise org the person is not in
// holds. A nil answer from it — and only a nil one — logs FallbackLine
// and runs the legacy path in full — Resolve, then RegisterIn per org —
// so the sequence is one owner registration, then one per org. A
// non-nil answer is the owner contract whatever it names: an answer
// naming no org, or a restriction matching none of the orgs it names,
// is logged and the node serves none until a heartbeat's answer names
// one — never the per-org path, never a failed registration.
func Register(ctx context.Context, r Registrar, cfg *Config, restrict []string) (Registered, error) {
	if !r.TakesOwner(ctx) {
		return perOrg(ctx, r)
	}
	orgID, err := r.AnyOrg(ctx)
	if err != nil {
		return Registered{}, err
	}
	answer, err := r.RegisterOwner(ctx, OwnedNodeID(cfg), orgID)
	if err != nil {
		return Registered{}, fmt.Errorf("register (owner %s): %w", cfg.OwnerKind(), err)
	}
	if answer == nil {
		if r.Logf != nil {
			r.Logf("%s", FallbackLine)
		}
		return perOrg(ctx, r)
	}
	served, note := ServedFrom(answer, restrict)
	if note != nil && r.Logf != nil {
		r.Logf("orgs: %v", note)
	}
	return Registered{Scopes: served, Owned: &Owned{NodeID: answer.NodeID, Owner: OwnerFrom(answer)}}, nil
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
// a node handed from a person to an enterprise — is logged `owner <old>
// → <new>`, with no restart. A nil answer keeps the last scopes; a
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
		l.logf("owner %s → %s", was.Owner.String(), owner.String())
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
