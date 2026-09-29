---
summary: The node registers with an owner (a person by default, or `enterprise` for shared infra), and registerNode/heartbeatNode answer with the node's one id and its scopes — the orgs it serves — so the node stops deriving them by re-reading the person's orgs; a platform that answers with neither owner nor scopes keeps today's per-org registration.
status: accepted
---

# ADR-015: A node registers once with an owner

2026-09-29, goal `the-node-serves-its-owners-orgs` (item 1). Sibling to
the runsheet goal `a-node-has-an-owner`, which serves the same shape at
`/contracts/`; the contract between them is agreed as a fixture, not
prose (`contracts-are-fixtures-not-prose`), in
`specs/contracts/registerNode.json` and
`specs/contracts/heartbeatNode.json`.

## Context

Today the node follows its token's person by accident of
implementation: `internal/node/loop.go` re-reads the person's orgs
every pass and registers a `Node` row in each, one per org. Chris wants
a node to have an owner — a person (bring-your-own infra, exactly
today's behaviour made explicit) or an enterprise (shared infra,
serving every org the enterprise has, on the enterprise's own
credentials) — with one `Node` row, not one per org, and an org without
a GitHub login (`grapevine`) named by its tenant slug rather than a
blank login.

This item is the gateway half only: the wire shape and the client that
parses it. The node consuming it — one `NodeID`, `Scope.Login`
carrying a slug, `mesh node status` naming the owner — is later work in
this goal.

## Decision

**Registration declares an owner.** `RegisterNodeInput` gains `Owner`
(`"person"` or `"enterprise"`), sent as an `owner` key only when set —
an older platform's `registerNode` still accepts today's call
unchanged. `TakesOwner(ctx)` reports whether the introspected
`RegisterNodeInput` has the field, cached an hour the same way as
`takesScaling`; the node checks it before setting `Owner` so a call
never asks a platform that would refuse it.

**The platform answers with the node's shape.** `RegisterNode` and
`HeartbeatNode` return `(*NodeAnswer, error)`: `NodeID`, `Owner{Kind,
ID, Name}`, and `Scopes []ScopeAnswer{OrgID, Login, Slug, AccountID}` —
one scope per org the node serves, `Slug` carrying the org's
`tenantSlug` for one with no GitHub login. `Client.MutateFields`
returns the mutation's decoded payload; `Mutate` is now a thin wrapper
over it, byte-identical for every existing caller. A payload with none
of `nodeId`/`owner`/`scopes` — today's platform, or one that refused
the new selection with "Cannot query field" — parses to a `nil`
answer and **no error**: the node keeps registering per org until the
platform side of this contract ships. `gateway.Org` gains `TenantSlug`
(`json:"tenantSlug"`) from `myOrgs`, for the same org named by slug
elsewhere.

**The contract is the fixture.** `specs/contracts/registerNode.json`
and `specs/contracts/heartbeatNode.json` are an example input and the
exact answer JSON the client parses, each naming the runsheet goal
`a-node-has-an-owner` as the side serving the same shape. A test loads
each into `gatewaytest.Fake.Answer` and round-trips it through an
`httptest` server and the real client, proving the fixture and the
parser agree; the two repos' fixtures are expected to be the same
JSON, checked by inspection until a shared contracts path exists.

## Consequences

A node built against this client compiles and runs unchanged against
today's platform: `Owner` is never sent, `TakesOwner` answers false,
and `RegisterNode`/`HeartbeatNode` return a `nil` answer every call, so
nothing downstream is forced to consume `NodeAnswer` yet. Once the
platform side answers with an owner and scopes, the node's own change —
one `NodeID` in `internal/node/config.go`'s `Scope`, `mesh node status`
naming the owner, a fourth org appearing on a later heartbeat served
without restart — lands as a later item on this branch's goal, switched
on by the answer alone; no flag day.
