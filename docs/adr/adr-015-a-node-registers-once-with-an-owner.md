---
summary: The node registers with an owner (a person by default, or `enterprise` for shared infra), and registerNode/heartbeatNode answer with the node's one id and its scopes — the orgs it serves — so the node stops deriving them by re-reading the person's orgs; a platform whose schema takes neither `ownerKind` nor `owner` keeps today's per-org registration, and one that takes it but answers nil keeps the owner contract, serving the orgs node.yaml names until an answer names them.
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
keeps the owner contract. The node logs `FallbackLine` and is owned all
the same: its one id (`OwnedNodeID`), the owner's kind from node.yaml,
`contract: owner` and `node_id` saved. What it serves until an answer
names its orgs is amended below (2026-10-03): the orgs node.yaml names,
resolved as the per-org contract resolves them — never none at a start.
It makes no per-org registration; the loop heartbeats the one id and the
first answer that names orgs is what it serves
(`TestOwnedRegisterNilAnswerKeepsTheOwnerContract`).
The sequence is: the probe; **one** owner registration; on a nil answer
`Resolve`; nothing else. An
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
contract under its one id, serving at a start the orgs node.yaml names
(2026-10-03, below) and after it its last scopes — never the per-org
registration. A
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

## The node derives the owner row's id as the platform does (2026-10-03)

`v0.2.248` made the owner probe accept `ownerKind` and, for the first
time, took the owner contract against the real platform — and derived
the owner row's id its own way: `OwnedNodeID` was `gateway.NodeID(owner,
slug)`, the per-org formula with the owner's uuid in the org's place.
The platform refused it, naming the id its own formula names
(`platform/ids/node.go`,
`NodeOwnerID(kind, owner, slug) = uuid5(idNamespace, "node:"+kind+":"+owner.String()+":"+slug)`,
`kind` "" read as "person") — the grapevine node's heartbeat said
`node grapevine registers at 18a8b766-18f3-5e73-b4cf-efb91c01946a, the
id its form names, not 47a08dd9-bafb-5c64-bb16-7e806f0b1b95`.

**The formula and the namespace join the contract.** `gateway.NodeOwnerID`
(`internal/gateway/ids.go`) is the same formula over the same namespace
mesh-cli already holds for `NodeID`, `GoalID` and `ProjectID`
(`7b3c1d2e-4f5a-4b6c-8d7e-9f0a1b2c3d4e`). `OwnedNodeID` now takes the
owner kind and the ownerId the registration actually sends — `Register`
already resolves it (`ownerIDFor`): the enterprise's id on `owner:
enterprise`, node.yaml's `owner_id` for a person — so an enterprise
node derives from its enterprise id, never the admin person's.
`specs/contracts/registerNode.json` carries a worked example as a
top-level `ownerNodeId` object (`namespace`, `formula`, `kind`,
`owner`, `slug`, `id`), and `input.aggregateId`/`answer.nodeId` are that
same id, so the fixture is self-consistent; `internal/gateway/ids_test.go`
and `internal/node/owned_test.go` assert both sides of the formula
against it.

**The live id did not reproduce.** The planner could not derive
`18a8b766-18f3-5e73-b4cf-efb91c01946a` from `(person, the owner id the
node sent, grapevine)` under this formula and namespace — so the
deployed platform may feed `NodeOwnerID` a different owner uuid than
the node's own `owner_id` (a resolved account id, perhaps, rather than
the person's). The runsheet goal `registernode-answers-with-the-node-it-made`
is to fix the fixture on its side to the same worked example; this
goal's item 2 (adopting the id a refusal names, and retrying once)
covers any drift that remains between the two formulas.

Tests: `internal/node/owned_enterprise_test.go`.

## A node takes the id the platform names (2026-10-03)

v0.2.248 was the first node to take the owner contract against the
real platform, and its owner id was refused: the node derived it as
`gateway.NodeID(owner, slug)`, the platform as `ids.NodeOwnerID(kind,
owner, slug)`, and the platform refuses a registration under any other
id, naming its own — `node grapevine registers at
18a8b766-18f3-5e73-b4cf-efb91c01946a, the id its form names, not
47a08dd9-bafb-5c64-bb16-7e806f0b1b95` (platform/aggregates/node.go).
Deriving it the platform's way is one rule; this one is that a drift
never strands a node (`internal/node/owned.go`, `Register`).

**A refusal that names an id is adopted, and retried once.** When
`RegisterOwner` is refused and `AdoptedID(err)` finds a uuid after
`registers at ` that differs from the id sent, `Register` sets
`cfg.NodeID` to it, notes `owner id adopted from the platform: <adopted>
(derived <sent>)` on `Registered.Notes` (logged after the start line by
`RegistrationLines`) and calls `RegisterOwner` once more under the
adopted id. A refusal naming no id, or naming the id sent, is returned
at once with no retry; a second refusal is returned as any other — no
third call.

**The adopted id is kept even when the retry is refused.** We chose the
partial result: `Register` returns, beside the second refusal, a
`Registered` whose `Owned.NodeID` is the adopted id, `IDSource` adopted
and the note; the caller (`mesh node start`'s register closure, `mesh
node register`) logs the notes and saves it with `SaveAdoption`, which
writes `node_id` and `node_id_source` alone — never `contract`, which no
registration made, so a move to the owner contract still retires the
legacy rows when one goes through. The next attempt — the confirm
window's next retry or the next start — registers under the adopted id.

**How the id was arrived at is recorded.** `Registered.IDSource` and
node.yaml's `node_id_source` are `derived` when `OwnedNodeID` computed
the id (no `node_id` saved) and `adopted` after an adoption;
`SaveRegistration` writes it, and a start that registers under the saved
`node_id` leaves it as it was.

**The fake replays it.** `gatewaytest.Fake.RegistersAt`, when set,
refuses a `registerNode` under any other `aggregateId` with the
platform's exact wording naming `RegistersAt` and the sent id — the
refused call recorded in `Calls`, as the platform saw it — and registers
one under it.

**A self-update refused this way is not marked failed for good**
(2026-10-03): a rollback whose confirm error named an id is retried on
the next tick, under the adopted id — ADR-002's amendment of 2026-10-03,
a rollback on a refusal naming the owner id is not final.

Tests: `internal/node/owned_adopt_test.go`,
`internal/commands/nodestart_adopt_test.go`.

## A nil answer keeps the orgs node.yaml names (2026-10-03)

2026-10-03 13:51Z, the grapevine node took v0.2.264 as the canary. The
hot swap worked, the owner registration was refused naming an id, the
node adopted it, confirmed the update — and logged `node grapevine
(grapevine) owned by person, serving none`. The platform takes
`ownerKind` but answers registerNode with no scopes yet (runsheet goal
registernode-answers-with-the-node-it-made), and on a nil answer the
node kept "the last scopes", which on a first owner registration are
none. It claimed nothing until a person put `contract: per-org` back by
hand. The rule "keep the last scopes" was right for a heartbeat and
wrong for a start: at a start there is nothing to keep.

**At a start a nil answer serves what the per-org contract served.**
When the probe takes owner and `RegisterOwner` answers nil, `Register`
calls `Registrar.Resolve` — `ResolveScopes`: the token's person's orgs,
restricted by node.yaml's `orgs:`, exactly as the per-org contract
resolves them — and returns those as `Registered.Scopes`, each on its
**per-org node id** (`Config.NodeIDIn`), so the claims land on the rows
that served those orgs before. The node is still on the owner contract:
`Owned.NodeID` is the one owned id, `contract: owner` is saved, the
loop heartbeats that id once per tick, and no per-org registration is
made. `FallbackLine` now reads `register: the platform took owner but
answered nil; keeping the owner contract and the orgs node.yaml names
until a heartbeat answers`, and the start line names those orgs
(`serving grapevine`), never `serving none`.

**Until an answer names them.** `Loop.TakeAnswer` with a nil answer
keeps the loop's scopes as before, so a heartbeat under the owned id
answered nil leaves the resolved orgs served; the first non-nil answer
replaces them whole, on the answer's one id. `OwnedReloader` never
resolves again, so the orgs served on a nil-answering platform are those
resolved at the start.

**A Resolve that fails is noted, not fatal.** `orgs: <err>; serving
none until a heartbeat answers` follows `FallbackLine`, and the node
keeps the owner contract with no scopes, as before this amendment — the
probe still chooses the contract.

Tests: `internal/node/owned_test.go`
(`TestOwnedRegisterNilAnswerKeepsTheOwnerContract`,
`TestOwnedRegisterNilAnswerKeepsThePerOrgScopes`,
`TestOwnedTakeAnswerNilKeepsScopes`) and
`internal/node/owned_replay_test.go`
(`TestOwnedReplay20261003ClaimsGrapevine`: a `gatewaytest.Fake` that
takes `ownerKind` with a nil `Answer`, node.yaml's orgs `[grapevine]`,
the person's first org foghornid — the node starts `serving grapevine`
and its first tick claims grapevine's item on grapevine's per-org id).

## The owner registration names a configured org (2026-10-03)

The grapevine canary (goal `an-update-that-serves-no-org-is-not-confirmed`,
item 4): `Registrar.AnyOrg` sent the first of the token's person's orgs,
unrestricted — "the platform ignores orgId on this contract" held until
the id it named landed the derived row under foghornid, the person's
first org, when the node was configured to serve grapevine. The
platform does not yet derive `orgId` from the owner the row is
registered under (runsheet goal `registernode-answers-with-the-node-it-made`,
queued), so an unconfigured org sent today is not purely decorative.

**The owner registration passes no arbitrary org.** `Registrar.AnyOrg`
is renamed `Registrar.ConfiguredOrg` and its contract changes: an org
the node is configured to serve (node.yaml's `orgs:`), never the
person's first unconditionally. `ConfiguredOrgID(orgs []gateway.Org,
configured []string) (id string, matched bool)` (`internal/node`, pure,
no gateway) is the rule, the same one `ResolveScopes` applies to the
per-org path — case-insensitive, trimmed, matched against an org's
`Login` or `TenantSlug`: when `configured` names any, the id of the
first of `orgs` matching one of them; when `configured` is empty, the
first of `orgs`, unrestricted, as before; when `configured` names some
but none matches, `""` and `matched` false. `Register` sends no orgId
in that last case and notes it — `register: none of the orgs node.yaml
names is one of the person's; sending no orgId` — rather than falling
back to the first, so a restriction naming an org the person is not in
never silently derives a row under one they are.

Tests: `internal/node/configuredorg_test.go`
(`TestConfiguredOrgIDPicksAConfiguredOrg`,
`TestConfiguredOrgIDNoRestrictionIsTheFirst`,
`TestConfiguredOrgIDNoneMatchesSendsNone`,
`TestOwnedRegisterSendsTheConfiguredOrg`).

## RegisterNodeInput gains `kind` (2026-10-03)

ADR-023 (`mesh mcp`, Claude Code is a personal node) needs a node row
marked apart from an operator's: `mesh mcp`'s own node registers on the
owner contract above (`Owner "person"`) but also carries `Kind
"personal"`, so the platform and `mesh node status` can tell it from a
node a person runs as a service.

**`RegisterNodeInput.Kind`**, sent as a `kind` key only when the
introspected `RegisterNodeInput` has the field — `gateway.NodeKindField`
("kind"), `Client.TakesNodeKind(ctx)` cached an hour the same way as
`TakesOwner`/`OwnerField`. An older platform's `registerNode` still
accepts the call unchanged, with no `kind` sent. `internal/node/personal.go`'s
`RegisterPersonal` is the first caller: it sends `Kind "personal"`
alongside `Owner "person"`.

Tests: `internal/gateway` (the `TakesNodeKind` probe, cached and
decided by introspection the same way as `TakesOwner`), and
`internal/node/personal_test.go` (a registration's input carries `kind`
`personal` when the platform's schema has it, none when it does not).

## A session's commands name their actor (2026-10-03)

Goal every-actor-has-a-name: today the activity feed says `node:<id>`
for planner, builder and evaluator alike. A node registers once with an
owner (above); the sessions it runs are agents of that node, and each
names itself on the commands it sends. This section is the gateway
half; the node's wiring (which session carries which role) extends it.

**The shape.** `actor` is a string input field,
`agent:<role>@<slug>` — `<role>` one of `builder`, `evaluator`,
`planner` (`gateway.Role`: `RoleBuilder`, `RoleEvaluator`,
`RolePlanner`) and `<slug>` the node's slug. A caller marks its context
with `gateway.WithActor(ctx, role, slug)`; `gateway.ActorFrom(ctx)`
answers the string. `gateway.ActorField` is `"actor"`.

**Where it rides.** `Client.MutateFields` puts it on the input of five
commands when the ctx carries one: `claimWorkItem`,
`reportWorkItemProgress`, `finishWorkItem` (its already-done path
too, which shares `FinishWorkItemInput`), `evaluateWorkItem` (its
no-verdict path too) and `planGoal`. No other command carries it, and a
ctx without `WithActor` carries none — `mesh mcp`'s pass-through work
tools (ADR-023) act as the person, not an agent, and send no actor.
The platform half (runsheet trd §6a) records it on the event.

**The introspection gate.** The key is sent only when the platform's
input type takes it: `ClaimWorkItemInput`,
`ReportWorkItemProgressInput`, `FinishWorkItemInput`,
`EvaluateWorkItemInput` and `PlanGoalInput` are each introspected
(`__type(name: $t)`) once and the answer kept per type for
`ScalingProbeEvery`, as `UsageReportInput`'s tool `error` is. A probe
that fails, or a type without `actor`, sends the input without it —
never an error. The client never sends the key to be refused and
resends: a refused attempt would be recorded carrying it. `actor` is an
input, not a selection, so `fields/*.txt` and the generated selections
are unchanged.

Tests: `internal/gateway/actor_test.go` (each role's actor on all five
commands; none, and no error, on a platform without the field —
`gatewaytest.Fake.NoActor`, whose inputs refuse the key, so a client
that skipped the probe fails loudly; the probe kept an hour; none with
no `WithActor`).

**The node's wiring.** Each session marks its ctx once where it opens,
with `actorContext(ctx, cfg, role)` (`internal/node/identity.go`, over
`gateway.WithActor`), so every command it sends carries
`agent:<role>@<slug>`. The builder: the `Loop`'s `claimWorkItem`, the
`Runner`'s progress and finish (`RunItem`, `AdoptItem`), the
`Rebaser`'s session (its claim, progress and finish) and the progress
heartbeat of an adopted session. The evaluator: the `Evaluator`
(`Evaluate`, `AdoptEvaluation`), whose `evaluateWorkItem` carries it on
a verdict and on no verdict alike. The planner: the `Planner` (`Plan`,
`AdoptPlan`), whose `planGoal` carries it. `mesh mcp`'s pass-through
work tools mark none: they act as the person. The `<slug>` is
`agentSlug(cfg)` — node.yaml's `slug`, else `node` — the same fallback
`identityFor` names the agent's commits for (ADR-024), so a session's
commands and its commits name one agent. Tests:
`internal/node/actor_test.go` (a build's claim, progress and finish as
`agent:builder@<slug>`, and `agent:builder@node` with no slug; an
evaluation's `evaluateWorkItem` as `agent:evaluator@<slug>` on both
paths; a plan's `planGoal` as `agent:planner@<slug>`), each read with
`gateway.ActorFrom` on the ctx the node's gateway receives.

## FinishWorkItem carries a description naming who built and paired (2026-10-03)

Goal every-actor-has-a-name item 3: the PR body `FinishWorkItem` opens
the pull request with should name who built the item and who it was
paired with, not just the operator's token. The evaluator is not known
at finish time — an item can be evaluated after it is finished, and
twice — so the description does not name it; it says where to look.

**The shape.** `internal/node.FinishDescription(FinishWho)` renders:

```
## Who

Built by agent:builder@<slug> on <model> (<adapter>)
Paired with <name> <email>
[one more such line per further pairer, or, with none:]
Paired with nobody

Evaluated by: recorded by the evaluation's actor
```

`FinishWho.Actor` is the build session's own actor string (this ADR's
section above, `gateway.ActorFrom`); `Pairers` is `internal/node.Pairer`
(every-actor-has-a-name item 2, node.yaml's `pair:`). "Evaluated by:
recorded by the evaluation's actor" is deliberate: the evaluator is
named by `EvaluateWorkItem`'s own `actor` (`agent:evaluator@<slug>`),
recorded on the evaluation's event, not folded into the finish's
description — the platform reads the evaluator from there, not from
this text.

**Where it rides.** `gateway.DescriptionField` is `"description"` on
`FinishWorkItemInput`, sent by `Client.FinishWorkItem` and
`FinishWorkItemAlreadyDone` (a variadic `description ...string`, so a
caller that has none sends none) only when the introspected
`FinishWorkItemInput` takes it (`Client.withDescription`, the same
once-an-hour probe as `actor`'s, above). The `Runner`'s `buildRun.description`
renders it from the session's actor, model, adapter (`Adapter`) and
`ident.Pairers`, and `afterBuild` passes it to both finish calls, so a
pushed finish and an already-done one carry the same text.

Tests: `internal/node/finishdesc_test.go` (`FinishDescription`'s exact
lines, with pairers and with none; a `Runner` test against
`gatewaytest.Fake` asserting the recorded `finishWorkItem` input's
`description` names the builder's actor and a pairer, that
`Fake.NoDescription` omits the key while the finish still succeeds, and
that an already-done finish carries the same description).

## ScopeAnswer carries `shares` (2026-10-03)

Goal `every-org-gets-its-share-of-a-node` item 3: a shared node divides
its capacity into per-org floors, equal by default or weighted by the
enterprise's own setting. The weight rides the same owner answer this
ADR already defines (`NodeAnswer.Scopes`), not a separate read.

**The shape.** `ScopeAnswer` gains `Shares int` (`json:"shares"`) — the
enterprise's per-org weight, `0` when the platform sends none or omits
the field (equal shares). `nodeAnswerFields` is the hand-written
selection `RegisterNode`/`HeartbeatNode` ask the payload for beside
`status` (this ADR's `mutateForAnswer`); an older platform's `scopes`
element type without `shares` refuses the *whole* selection on an
unknown field, the same failure mode `TakesOwner` and `TakesNodeKind`
guard against, so the node must not ask for it blind.

**The probe.** `Client.takesScopeShares(ctx)` introspects
`DispatchResult` — the shared mutation payload type `registerNode` and
`heartbeatNode` return their hand-selected fields on — once, follows
its `scopes` field's element type through any `NON_NULL`/`LIST`
wrapper, and answers whether that type carries `shares`; the answer is
kept for `ScalingProbeEvery`, the same cadence as
`takesHoldingUnclaimedUntil`, and a failed probe is `false`, kept just
as long. `nodeAnswerFieldsFor(ctx)` picks
`scopes { orgId login slug accountId shares }` when the probe says yes,
else today's selection with no `shares` — `fields_gen.go` is untouched,
since this answer is hand-selected, not a generated entity (ADR-010).

**The fixtures.** `specs/contracts/registerNode.json` and
`heartbeatNode.json` give every scope in the answer a `shares` integer
(runsheet 2, wardroomid 1, grapevine 1; heartbeatNode's fourth,
foghornid, 0) and each fixture's description names `shares` as the
enterprise's per-org weight, a setting on the enterprise the platform
serves as part of the owner contract, absent or 0 meaning equal shares.

Tests: `internal/gateway/nodeanswer_test.go`
(`TestNodeAnswerSharesFromFixtures` decodes both fixtures and checks
each scope's `Shares` by login; `TestTakesScopeSharesFalseAsksWithoutShares`
and `TestTakesScopeSharesTrueAsksWithShares` assert, via
`gatewaytest.Fake.NoScopeShares` and the recorded `Call.Query`, that the
selection sent omits or carries `shares` to match what the platform's
`scopes` element type has, and that the decoded answer's `Shares`
follows).
