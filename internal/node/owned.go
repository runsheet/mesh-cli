package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Owned is a node registered once with its owner (ADR-015,
// Registration): its one id, the same in every org it serves, who owns
// it, and the platform's answer that said so.
type Owned struct {
	NodeID string
	Owner  Owner
	Answer *gateway.NodeAnswer
}

// Registrar is what a registration does, one function per step, so the
// choice between the owner contract and the legacy per-org path
// (Register) is read and tested without a gateway.
type Registrar struct {
	// TakesOwner is the probe: whether the platform's RegisterNodeInput
	// has `owner` (gateway.Client.TakesOwner).
	TakesOwner func(ctx context.Context) bool
	// Resolve lists the orgs of the token's person, restricted by
	// node.yaml's orgs (ResolveScopes). Only the per-org path reads it.
	Resolve func(ctx context.Context) (Scopes, error)
	// RegisterOwner is the one registration on the owner contract, under
	// the node's own id and declaring its owner's kind; nil is a
	// platform that took `owner` but answered status alone.
	RegisterOwner func(ctx context.Context, nodeID string) (*gateway.NodeAnswer, error)
	// RegisterIn registers one Node per org, as before owners.
	RegisterIn func(ctx context.Context, scopes Scopes) error
	// Logf logs a line (optional).
	Logf func(format string, args ...any)
}

func (r Registrar) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Registered is Register's result: the contract the registration made,
// the scopes to serve and, on the owner contract, the node's one id,
// its owner and the answer (nil on the per-org path).
type Registered struct {
	Contract string
	Owned    *Owned
	Scopes   Scopes
}

// OwnedNodeID is the id the node registers under on the owner
// contract: node.yaml's node_id when set, else one from (owner, slug),
// so registering again converges on it, else a fresh one.
func OwnedNodeID(cfg *Config) string {
	if cfg.NodeID != "" {
		return cfg.NodeID
	}
	if owner, err := uuid.Parse(cfg.OwnerID); err == nil {
		return gateway.NodeID(owner, cfg.Slug).String()
	}
	return uuid.NewString()
}

// Register registers the node (ADR-015, Registration). The probe comes
// first: a platform whose RegisterNodeInput has no `owner` gets today's
// path exactly — Resolve, then RegisterIn per org, no owner sent. One
// that has it gets one RegisterOwner under OwnedNodeID. A nil answer —
// the platform took `owner` but answered status alone — falls to the
// per-org path, the owner registration having made no row of its own
// worth serving. A non-nil answer is the owner contract whatever it
// names: its scopes, restricted by restrict, are what the node serves,
// and none — an answer naming no org, or a restriction matching none of
// those it names — is a node serving nothing, logged, never an error
// and never the per-org path.
func Register(ctx context.Context, r Registrar, cfg *Config, restrict []string) (Registered, error) {
	if !r.TakesOwner(ctx) {
		return perOrg(ctx, r)
	}
	id := OwnedNodeID(cfg)
	answer, err := r.RegisterOwner(ctx, id)
	if err != nil {
		return Registered{}, fmt.Errorf("register (owner %s): %w", cfg.OwnerKind(), err)
	}
	if answer == nil {
		r.logf("register: the platform took owner but answered no node; registering per org until it does")
		return perOrg(ctx, r)
	}
	if answer.NodeID != "" {
		id = answer.NodeID
	}
	scopes := ServedFrom(answer, restrict, r.logf)
	return Registered{Contract: ContractOwner, Owned: &Owned{NodeID: id, Owner: OwnerFrom(answer), Answer: answer}, Scopes: scopes}, nil
}

// perOrg is the registration before owners: the person's orgs, a Node
// in each.
func perOrg(ctx context.Context, r Registrar) (Registered, error) {
	scopes, err := r.Resolve(ctx)
	if err != nil {
		return Registered{}, err
	}
	if err := r.RegisterIn(ctx, scopes); err != nil {
		return Registered{}, err
	}
	return Registered{Contract: ContractPerOrg, Scopes: scopes}, nil
}

// ServedFrom is the scopes an answer names, restricted by node.yaml's
// orgs (ScopesFromAnswer), each on the node's one id. It is never
// fatal: what ScopesFromAnswer refuses is logged through logf and the
// node serves what is left — the orgs the restriction names that the
// answer has, or nothing at all.
func ServedFrom(a *gateway.NodeAnswer, restrict []string, logf func(string, ...any)) Scopes {
	if a == nil {
		return nil
	}
	if scopes, err := ScopesFromAnswer(a, restrict); err == nil {
		return scopes
	}
	if len(a.Scopes) == 0 {
		logf("orgs: the platform answered with no org to serve; serving nothing until it changes")
		return nil
	}
	want := map[string]bool{}
	for _, o := range restrict {
		want[strings.ToLower(strings.TrimSpace(o))] = true
	}
	found := map[string]bool{}
	var kept Scopes
	for _, sa := range a.Scopes {
		login, slug := strings.ToLower(sa.Login), strings.ToLower(sa.Slug)
		if want[login] || want[slug] {
			found[login], found[slug] = true, true
			kept = append(kept, Scope{Login: sa.Login, Slug: sa.Slug, ID: sa.OrgID, AccountID: sa.AccountID, NodeID: a.NodeID})
		}
	}
	if len(kept) == 0 {
		logf("orgs: none of %s is served by this node's owner; serving nothing until it changes", strings.Join(restrict, ", "))
		return nil
	}
	logf("orgs: %s is not served by this node's owner; serving %s", strings.Join(missingOrgs(want, found), ", "), strings.Join(kept.Names(), ", "))
	return kept
}

// WriteNodeID records what a registration made in node.yaml — Contract
// and, on the owner contract, the node's one id as NodeID — and saves it
// (SaveConfig). A failed write is returned, never only logged: the next
// start registers under that id and `mesh node status` reads the
// contract from the file.
func WriteNodeID(cfg *Config, r Registered) error {
	cfg.Contract = r.Contract
	if r.Owned != nil && r.Owned.NodeID != "" {
		cfg.NodeID = r.Owned.NodeID
	}
	if err := SaveConfig(cfg); err != nil {
		return fmt.Errorf("save node.yaml (contract %s, node id %s): %w", cfg.Contract, cfg.NodeID, err)
	}
	return nil
}
