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
platform side of this contract ships. A refusal is kept for
`NodeAnswerRetry` (10 minutes): until then both commands ask `status`
alone, so today's platform costs one request per heartbeat, not a
refused one and a retry, and a deploy that adds the fields is picked
up within the window. `gateway.Org` gains `TenantSlug`
(`json:"tenantSlug"`) from `myOrgs`, for the same org named by slug
elsewhere; `myOrgs` is a `Map` scalar, so it takes no selection and
the platform's row (which carries `tenantSlug`) arrives whole — a test
fails if the schema ever makes it an object type.

**The contract is the fixture.** `specs/contracts/registerNode.json`
and `specs/contracts/heartbeatNode.json` are an example input and the
exact answer JSON the client parses, each naming the runsheet goal
`a-node-has-an-owner` as the side serving the same shape. A test loads
each into `gatewaytest.Fake.AnswerJSON`, served as the fixture's own
bytes (only the fields the query selects, as GraphQL does), and
round-trips it through an `httptest` server and the real client; the
fixture's answer is also decoded into `NodeAnswer` refusing unknown
keys and checked for every key the client reads, so a misspelled key
fails rather than decoding to empty on both sides; the two repos' fixtures are expected to be the same
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

## Node model (2026-09-29)

2026-09-29, item 12, carried onto main by item 17. The pure model the node consumes the answer with —
types and functions only, no gateway calls and no wiring into
register/start/status (items 18 and 19 wire it in).

**`Scope` carries `Slug`.** An org's tenant slug rides beside its
login, always set; `Scope.Name()` is `Login`, or `Slug` when `Login` is
empty — how grapevine, with no GitHub login, is named to a person.
`ResolveScopes` and `Reloader` (the legacy per-org path) fill it from
`gateway.Org.TenantSlug`, so a node not yet switched onto the platform's
answer names grapevine by slug too. `Scopes.Names()` joins `Name()` over
every scope for display; `Scopes.Logins()` keeps the raw logins for a
caller comparing against GitHub, not showing them to a person. Every
log and status line in `internal/node` and `internal/commands` that
printed `sc.Login` for a human now calls `sc.Name()` (or `.Names()`);
`Login` stays where it is compared to an actual GitHub login — repo
owner matching, the `orgs:` restriction.

**`Config.Owner`** is `""`/`"person"` (bring-your-own infra, the
default) or `"enterprise"` (shared infra); `LoadConfig` rejects any
other value, naming the two allowed, and `Config.OwnerKind()` returns
which. `Config.NodeID` becomes the node's one id from the platform's
answer; on the legacy per-org path it is unused; each `Scope` still
carries its own per-org id from `NodeIDIn`.

**`ScopesFromAnswer(a *gateway.NodeAnswer, restrict []string) (Scopes,
error)`** is the pure conversion from the platform's answer to the
node's `Scopes`: one `Scope` per `a.Scopes` entry, `NodeID` the same
`a.NodeID` in every one (the node's single id, not per-org).
`restrict` is node.yaml's `orgs:` list, matched case-insensitively
against a scope's `Login` or `Slug`, so it still restricts an org
named only by slug, and holds for every entry of the answer — a
match never lifts it for the entries after (both functions keep the
requested names whole and track the matched ones apart); a restricted name the answer does not contain is
an error listing the missing names — the same rule `ResolveScopes`
applies — and an answer with nothing left to serve is `"the platform
answered with no org to serve"`. `Owner{Kind, ID, Name}` and
`OwnerFrom(a)` read the answer's owner; `Owner.String()` is `"owned by
<Name>"`, falling back to `"<Kind> <ID>"` when the platform sends no
name — `mesh node status` and the node's first log lines print this
once items 18 and 19 call it.

Tests in `internal/node/owner_test.go` open no socket: they decode
`specs/contracts/registerNode.json` and `heartbeatNode.json`'s
`answer` directly and run `ScopesFromAnswer`/`OwnerFrom` over it, so
the evaluator can check every one of them without a gateway. They are
named with the one prefix `TestModel…` (`go test ./internal/node -run
'^TestModel'`), and no other test in the package carries it — the
earlier `TestModelTier`, `TestModelsFor`, `TestModelRank` and
`TestModelAgainstEvidence` are `TestBudgetModelTier`,
`TestKindModelsFor`, `TestKindModelRank` and
`TestPlanModelAgainstEvidence` — so the socket-free run never picks
up a test that starts a gateway, as `-run Owner` picked up
`TestLoopServesEveryOrgOfItsOwner`.
