---
summary: The node registers with an owner (a person by default, or `enterprise` for shared infra), and registerNode/heartbeatNode answer with the node's one id and its scopes — the orgs it serves — so the node stops deriving them by re-reading the person's orgs; a platform whose schema takes neither `ownerKind` nor `owner` keeps today's per-org registration, and one that takes it but answers nil keeps the owner contract.
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
answer and **no error**: a platform whose `RegisterNodeInput` has no
`owner` keeps the node registering per org until the platform side of
this contract ships; one that takes `owner` and answers nil keeps the
node on the owner contract (the 2026-10-02 section below). A refusal is kept for
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
owner matching, the `orgs:` restriction. The same holds for the org
half of a project's `org/project`: the planner's, director's, builder's,
evaluator's, rebaser's and channel's context fetch and prompt name it by
`Scopes.NameOf(orgID)` / `scope.Name()` (`LoginOf` is gone), so a
grapevine project is `grapevine/<project>`, never `/<project>`; and
`gateway.Org.Name()` (login, else tenant slug) names the orgs
`mesh node login` and `mesh goal set` print.

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

## Registration and the loop (2026-09-29)

2026-09-29, item 18. The model above wired into `mesh node register`,
`mesh node start` (`registerNode`) and the loop.

**The probe.** A registration first asks `gateway.Client.TakesOwner`
whether the platform's `RegisterNodeInput` has `owner`
(`node.Register`, over a `node.Registrar` of one function per step, so
the decision is read without a gateway). The probe comes **before**
the person's orgs are read: on the owner contract node.yaml's `orgs:`
restriction is checked against the answer, never against `MyOrgs`, so
a restriction naming an enterprise org the token's person is not in, or
an org without a login by its slug, holds
(`TestOwnedRegisterProbesBeforeResolving`). Only the per-org path
resolves the person's orgs (`ResolveScopes`), exactly as before.

**Two paths.** `TakesOwner` false is today's path exactly: one
`RegisterNode` per org under its per-org id (`registerIn`), no owner
sent, `Reloader` re-reading MyOrgs each tick, one heartbeat per scope.
`TakesOwner` true is one `RegisterNode` under the node's own id —
node.yaml's `node_id` when set, else `gateway.NodeID(owner, slug)`
(`OwnedNodeID`, so it converges), else a fresh uuid — with `orgId` the
first of the person's orgs, unrestricted (`Registrar.AnyOrg`; the
platform ignores it on this contract; the fixture's input shows the
shape) and `owner` `Config.OwnerKind()`. A
non-nil answer makes the node *owned*: `cfg.NodeID` is the answer's
`nodeId`, saved to node.yaml (a save that fails fails the start, never
logged and ignored), the scopes are `ScopesFromAnswer(answer,
cfg.Orgs)`, and node.log's line right after `StartLines` names the
owner and the orgs (its wording is the 2026-10-02 section's below).

**The nil-answer fallback, exactly.** A platform that takes `owner` but
answers nil — no `NodeAnswer` at all — is half-deployed; only a nil
answer is. A non-nil answer naming no org, or none that node.yaml's
`orgs:` keeps, is the owner contract all the same: the node is owned,
logs `orgs: …` naming what was left out, serves none and claims nothing
until a heartbeat's answer names an org — never the per-org path, never
a failed registration (`TestOwnedRegisterEmptyAnswerIsNotNil`,
`TestOwnedRegisterUnmatchedRestrictionIsNotFatal`). **The probe chooses
the contract, never the answer**: a nil answer after a positive probe
keeps the owner contract. The node logs `FallbackLine` — `register: the
platform took owner but answered nil; keeping the owner contract and the
last scopes until a heartbeat answers` — and is owned all the same: its
one id (`OwnedNodeID`), the owner's kind from node.yaml, `contract:
owner` and `node_id` saved, and its last scopes, which at a start are
none. It makes no per-org registration and never reads the person's
orgs; the loop heartbeats the one id and the first answer that names
orgs is what it serves (`TestOwnedRegisterNilAnswerKeepsTheOwnerContract`).
The sequence is: the probe; **one** owner registration; nothing else. An
earlier draft (item 27) fell back per org on a nil answer, registering a
Node per org after the owner registration and saving `contract:
per-org`; that let a nil answer undo the probe's choice, and was
rejected.

**`Loop.Owned` and `Loop.TakeAnswer`.** An owned loop (`Loop.Owned`:
the one id and the `Owner`) heartbeats **once** per tick under its id,
and every non-nil answer goes through `TakeAnswer(*gateway.NodeAnswer)`:
the answer's scopes, restricted by node.yaml's `orgs:` as last
reloaded, replace `Loop.Scopes` under `l.mu`, logged `orgs a → b` with
`Names()` as a reload logs them; a restriction naming an org the answer
does not serve is logged (`orgs: not a member of …`), not fatal, and the
rest are served. Only a nil answer keeps the last scopes: a non-nil one
replaces them whatever it names, so an org the platform stopped serving
is claimed from no more on the next tick, and one naming no org (or
none the restriction keeps) leaves the node serving none, logged `orgs
a → none` (`TestOwnedTakeAnswerDropsRemovedOrgs`,
`TestOwnedTakeAnswerUnmatchedRestriction`). The owned node's reload is `OwnedReloader`: node.yaml alone
(capacity, limits, accounts, context, claim, the orgs restriction —
applied at once to the last answer), never MyOrgs, never a
registration.

**The hand-over.** An answer whose owner's id or kind differs from the
last is logged (`owner changed: Chris → anythink` since 2026-10-02,
below) when the mini goes from Chris to the enterprise — and taken,
with the scopes it names, on that tick: no restart.

**Every call that carried a per-org node id** reads `Scope.NodeID`,
which `ScopesFromAnswer` sets to the one id in every scope, so each
follows without change: `ClaimWorkItem`, `ClaimPlan`,
`ClaimWorkItemRebase`, `ClaimDirectionResearch`, `GetDesiredVersion`
(first non-empty over the scopes — the one node), `IssueItemRepoToken`,
the claim-time `Credentials.Open` and its usage, the sessions'
`state.json` `NodeID`, the validation attestation's. `FinishWorkItem`,
`EvaluateWorkItem`, `FinishWorkItemRebase` and `ReportWorkItemProgress`
carry the item, not the node. The upgrade confirm is the loop's first
heartbeat — the owned one. Outside the loop, `reportRollback` (the
rollback heartbeat) and `mesh node status` heartbeat through
`node.HeartbeatOutside`: once under node.yaml's `node_id` when its saved
`contract:` is `owner` and the platform takes `owner` — whatever that
heartbeat answers, a nil answer included, which serves no orgs this time
but never falls through to `MyOrgs` or a heartbeat per org (that would
send several and fail on an owner's orgs outside the token's person's;
`TestOwnedOutsideHeartbeatNilAnswerKeepsTheOneID`,
`TestOwnedRollbackNilAnswerStaysOnTheOneID`,
`TestOwnedStatusNilAnswerStaysOnTheOneID`) — and per org otherwise. Channel mode still resolves per org (unchanged).
Adoption: an owned node's first start after the switch also looks for
sessions under its old per-org ids (`LegacyIDs`, `AdoptWith.Legacy`),
so the builds a per-org node left running are taken up, not left to
time out.

**Tests.** Socket-free, `TestOwned…` in `internal/node/owned_test.go`
(`go test ./internal/node -run '^TestOwned'`): the fixtures' answers
through `Register` and `TakeAnswer`. The loop test that bound a
loopback socket through `gatewaytest` is gone: 2026-10-02, below.

## Registration and the loop, carried onto main (2026-10-02)

2026-10-02, item 24. Item 18's work above, carried onto main after
5e2b33e (`orgs:` matched by login or tenant slug on every path,
`releaseWorkItem` naming its node) and three things fixed that the last
evaluation and TRD §3 name. What holds, restated so it is read in one
place:

**Probe first.** `node.Register` asks `TakesOwner` before anything
else. A platform without `owner` runs the per-org path exactly —
`ResolveScopes`, then one `RegisterNode` per org, no `owner` sent, no
extra Node row. Only then, on a platform that takes it, is the one
`RegisterNode` under `OwnedNodeID(cfg)` with `owner` `cfg.OwnerKind()`
made.

**Nil versus empty.** A **nil** answer — and only nil — is a
half-deployed platform: `FallbackLine`, and the node stays on the owner
contract under its one id with its last scopes — never the per-org path. A
**non-nil** answer is the owner contract whatever it names: its scopes,
restricted by node.yaml's `orgs:` (`ServedFrom`, never fatal, an
unmatched restriction logged), are what the node serves — an answer
with no scopes serves none and never runs the per-org path. The same
holds per heartbeat (`TakeAnswer`): nil keeps the last scopes, non-nil
replaces them whole, so an org removed is claimed from no more and an
empty answer serves none.

**The answer is retained in the loop.** `Registered` carries the
answer (`Registered.Answer`) and `mesh node start` hands it to the loop
with `Loop.TakeAnswer` before the first tick. Without it a node whose
heartbeats since registration all answered nil had no answer to apply
a changed `orgs:` restriction to; with it `OwnedReloader`'s restriction
narrows the registration's answer at once
(`TestOwnedStartHandsTheAnswerToTheLoop`).

**`contract:` in node.yaml.** `Config.Contract` (yaml `contract:`) is
the registration the node last made: `owner` or `per-org`
(`ContractOwner`, `ContractPerOrg`); empty, on a node.yaml from before
it, reads `per-org` (`Config.ContractKind`). `registerNode` and `mesh
node register` save it beside `node_id` through `SaveRegistration`
whenever either changed, and a failed save is returned as an error —
the start fails, never logged and ignored. It is the TRD §3 step 1 field
`mesh node status` and the rollback reporter read (item 25)
(`TestOwnedContractPersisted`): both heartbeat under the one id only
when it says `owner` (`Config.OwnedID`), so a per-org node that kept a
`node_id` is reached per org alone, exactly as before owners
(`TestOwnedContractChoosesTheHeartbeat`).

**The log wording** (mesh-specs ADR 014, TRD §3). node.log's first line
after `StartLines` is `node <name> (<slug>) owned by <owner>, serving
<orgs>` — `node mini (runsheet-mini) owned by Chris, serving runsheet,
wardroomid, grapevine` — and on the per-org path `node <name> (<slug>)
per org, serving <orgs>` (`node.StartLine`; `mesh node register` prints
it too). What the registration has to say — the nil answer's
`FallbackLine`, an `orgs:` restriction the answer did not meet — is
carried on `Registered.Notes` and logged after it, never before
(`RegistrationLines`, `TestOwnedRegistrationLinesNameTheOwnerFirst`).
A hand-over is `owner changed: Chris → anythink`
(`Owner.Who`, the name alone), an org change `orgs a, b → a, b, c`. An
org without a GitHub login is named by `Scope.Name()` — its tenant slug
— in every one of them, never a blank.

**One id everywhere.** Every call that carried a per-org node id reads
`Scope.NodeID`, which the answer's scopes all set to its one id; the
held items a restart takes up (`OrgWorkItems` in `claimed`/`running`)
are matched against it and, on an owned node, its old per-org id as
well, as the adoption's `LegacyIDs` are.

**Tests open no socket.** The loop test
`TestOwnedLoopClaimsFromEveryAnsweredOrgAndAFourthWithNoRestart` drives
`Loop.Tick` against an in-process `node.Gateway` (`ownedGW` in
`owned_test.go`), never `httptest`: one `HeartbeatNode` per tick under
the one id, a `ClaimWorkItem` in each of registerNode.json's three orgs
on it, and heartbeatNode.json's fourth org claimed from on the next
tick with no restart. All run with `go test ./internal/node -run
'^TestOwned'`.

## The owner field is ownerKind (2026-10-03)

The platform's `RegisterNodeInput` spells the owner kind `ownerKind`
(runsheet goal registernode-answers-with-the-node-it-made), not
`owner` as this ADR first wrote it. A node that probed for `owner`
alone found none on the live platform and stayed on the per-org
contract forever — the Mini's row in Grapevine, an org outside its
enterprise (goal the-mini-serves-its-enterprise-and-nothing-else).

**The probe accepts either spelling.** `Client.OwnerField` introspects
`RegisterNodeInput` once per `ScalingProbeEvery` and answers
`ownerKind`, `owner` (a platform before it), or `""`; `ownerKind`
wins where both are present. `TakesOwner` is `OwnerField != ""`, so
whichever side deploys first, the probe is true once both have.

**RegisterNode sends the one the schema has**, never both:
`RegisterNodeInput.Owner` goes out under `OwnerField`'s name.

**ownerId is the owner's id.** The owner contract requires `ownerId`
to name the owner itself — the person's user id for a person, the
enterprise's id for an enterprise — and the platform admits an
enterprise registration only from an admin of that enterprise
(`adminOnEnterprise(ownerId)`). The node resolves an enterprise's id
from node.yaml's `enterprise:` through `Client.MyEnterprises`
(`myEnterprises`, a Map scalar decoded like `myOrgs`). The node
retires its own legacy per-org rows with `Client.RetireNode`
(`retireNode`) once it registers on the owner path.

The fake (`gatewaytest`) lists `ownerKind` by default and `owner` when
`OwnerFieldLegacy`; `UnknownFields` hides either spelling. It serves
`myEnterprises` from `Fake.Enterprises` the caller admins, refuses an
enterprise registration whose `ownerId` the caller does not admin with
an error naming `adminOnEnterprise`, and serves `retireNode` (the row's
status `retired`; an unknown id `no such node`). Tests:
`internal/gateway/ownerfield_test.go`.

## The enterprise is named, and the legacy rows retire themselves (2026-10-03)

The section above gives the node `ownerKind` and the client its
`MyEnterprises` and `RetireNode`; this one is how the node uses them
(`internal/node/owned.go`, `Register`; `internal/commands/nodecmd.go`,
`registrar`).

**The enterprise is resolved once and cached.** On `owner: enterprise`
the owner registration's `ownerId` is the enterprise's id
(`ownerIDFor`): node.yaml's `enterprise:` when it is a uuid, else
node.yaml's `enterprise_id:` when cached, else
`Registrar.ResolveEnterprise` — `myEnterprises` searched by slug or
tenantSlug, case-insensitive (`FindEnterprise`) — once per process. A
slug the token's person does not administer is a fatal registration
error, `enterprise "<slug>": not one of the token's enterprises (<slugs>)
— the token must be an enterprise admin's`. The resolved id is cached on
the config at once and written to node.yaml as `enterprise_id:` by
`SaveRegistration` beside `node_id:`, so a later start on a token that
cannot list enterprises still registers. `owner_id:` (the person) is
never overwritten: `OwnedNodeID` keeps converging on the same id. A
person's node sends `owner_id:` as before. `mesh node register` keeps
`enterprise_id:` only for the same `enterprise:`.

**The legacy rows retire themselves.** The first non-nil answer on the
owner path after a per-org contract — node.yaml's `contract:` as it was
when the registration began — retires the node's own legacy per-org
rows (`retireLegacy`, `Registrar.Retire` → `retireNode`): `NodeIDIn(org)`
for every org the answer names and every org of the token's person
(`Registrar.LegacyOrgs`, `myOrgs` unrestricted), each once, the owned id
never. Each id is derived from (org, slug), so it is always this node's
own row and never another node's. Each is one note on
`Registered.Notes`, logged after the start line and printed by `mesh
node register`: `retired legacy node <id> (org <name>)`, or a refusal —
already retired, `no such node` — as `legacy node <id> (org <name>):
<err>`, never fatal. The per-org path never retires, nor a nil answer
(the platform half-deployed), nor a registration whose contract was
already owner — a restart does not re-sweep. A nil answer saves the
owner contract all the same, so a node whose first owner registration
was answered nil keeps its legacy rows until a person retires them
(`runsheet nodes retire`); the platform answers once it takes the owner
field, so this is the half-deployed platform's case alone. The contract's transition
is the trigger; nothing else is recorded.

**The log names the owner and its field.** `Owner.Who` names an
enterprise with its kind — `node mini (runsheet-mini) owned by anythink
(enterprise), serving …`, `owner changed: Chris → anythink
(enterprise)` — and a person bare. Every registration notes the owner
field the platform's `RegisterNodeInput` took (`Registrar.OwnerField`,
`OwnerFieldLine`): `owner contract: field ownerKind`, `owner contract:
field owner`, or `owner contract: no owner field (registerNode takes
neither ownerKind nor owner); registering per org` — at start, and again
on a registration retried in the confirm window, so a change of the
probe's answer is said.

Tests: `internal/node/owned_enterprise_test.go`.
