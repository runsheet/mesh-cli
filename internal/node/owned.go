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
	// node.yaml's orgs (ResolveScopes).
	Resolve func(ctx context.Context) (Scopes, error)
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

// Register registers the node. A platform that does not take `owner`
// gets today's path exactly: Resolve, then RegisterIn per org, no owner
// sent. One that does gets Resolve (for an org id), then one
// RegisterOwner under OwnedNodeID; its answer's scopes, restricted by
// restrict, are what the node serves. A nil answer from it logs
// FallbackLine and runs the legacy path in full — RegisterIn per org of
// the resolved scopes — so the sequence is one owner registration, then
// one per org.
func Register(ctx context.Context, r Registrar, cfg *Config, restrict []string) (Registered, error) {
	scopes, err := r.Resolve(ctx)
	if err != nil {
		return Registered{}, err
	}
	if !r.TakesOwner(ctx) {
		return Registered{Scopes: scopes}, r.RegisterIn(ctx, scopes)
	}
	answer, err := r.RegisterOwner(ctx, OwnedNodeID(cfg), scopes[0].ID)
	if err != nil {
		return Registered{}, fmt.Errorf("register (owner %s): %w", cfg.OwnerKind(), err)
	}
	if answer == nil || len(answer.Scopes) == 0 {
		if r.Logf != nil {
			r.Logf("%s", FallbackLine)
		}
		return Registered{Scopes: scopes}, r.RegisterIn(ctx, scopes)
	}
	served, err := ServedFrom(answer, restrict)
	if err != nil {
		if len(served) == 0 {
			return Registered{}, err
		}
		if r.Logf != nil {
			r.Logf("orgs: %v; serving the rest", err)
		}
	}
	return Registered{Scopes: served, Owned: &Owned{NodeID: answer.NodeID, Owner: OwnerFrom(answer)}}, nil
}

// ServedFrom is ScopesFromAnswer that keeps what it can: a restriction
// naming an org the answer does not serve is the error, beside the
// scopes of the orgs it does name.
func ServedFrom(a *gateway.NodeAnswer, restrict []string) (Scopes, error) {
	scopes, err := ScopesFromAnswer(a, restrict)
	if err == nil {
		return scopes, nil
	}
	all, aerr := ScopesFromAnswer(a, nil)
	if aerr != nil {
		return nil, aerr
	}
	want := map[string]bool{}
	for _, o := range restrict {
		want[strings.ToLower(strings.TrimSpace(o))] = true
	}
	var kept Scopes
	for _, sc := range all {
		if want[strings.ToLower(sc.Login)] || want[strings.ToLower(sc.Slug)] {
			kept = append(kept, sc)
		}
	}
	return kept, err
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
// → <new>`, with no restart. A nil answer, or one naming no org, keeps
// the last scopes; a restriction naming an org not served is logged,
// not fatal. The loop is Owned from then on.
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
	if len(scopes) == 0 {
		return
	}
	if !sameOrgs(scopes, l.Scopes) {
		l.logf("orgs %s → %s", strings.Join(l.Scopes.Names(), ", "), strings.Join(scopes.Names(), ", "))
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
