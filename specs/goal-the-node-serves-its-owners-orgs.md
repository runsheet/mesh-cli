---
title: Goal — The node registers once with an owner, person or enterprise, serves the orgs the platform derives from it, and names an org without GitHub by its slug
kind: plan
status: running
parent: prd
summary: A node declares its owner — person (bring-your-own infra) or enterprise (shared infra) — registers once, and takes the orgs it serves from the platform's answer; the mesh-cli half is merged (#434, #485) and in review (#561), mesh-specs' TRD and ADR-014 are merged (#102, #104, #113), and the mini's live checks wait on the runsheet platform half.
---

# Goal — The node registers once with an owner, person or enterprise, serves the orgs the platform derives from it, and names an org without GitHub by its slug

Mesh goal `the-node-serves-its-owners-orgs`, running: items 1, 5, 10, 15
and 17 merged, item 32 (PR #561) in review, this document is item 33.

## Statement

Chris, 2026-09-29 00:35Z, on seeing the mini appear in his new org
grapevine: 'so nodes right now can be assigned to a person instead of
an org? i like that, lets you bring your own infra personally' — and
earlier 'we want to be able to share mini with any of the anythink
enterprise'. Today the node follows its token's person by accident of
implementation (`internal/node/loop.go` re-read the person's orgs
every pass and registered a Node row in each). The model: **a node has
an owner, a person or an enterprise.** A person's node (bring your own
infra) serves every org the person is a member of, with the person's
own accounts and the org's tools — exactly today's behaviour, made
explicit and visible. An enterprise's node (shared infra) serves every
org in the enterprise, with the enterprise's credentials
(credentials-follow-the-enterprise), and orgs outside it only by an
explicit share from an enterprise admin. An owner can hand a node from
person to enterprise (the mini, today Chris's, becomes anythink's).
Sibling goals: `a-node-has-an-owner` (runsheet: the platform half) and
`the-node-serves-its-owners-orgs` (mesh: the node half), agreed through
the contract fixture (`specs/contracts/registerNode.json`,
`specs/contracts/heartbeatNode.json`). Node half: the node registers
once, declaring its owner, and takes its Scopes from the platform's
answer to that registration and each heartbeat, not from re-reading the
person's orgs; one `NodeID` for the node; every gateway call that
carried a per-org node id follows; `mesh node status` and the first log
lines name the owner and the orgs served; a hand-over is picked up on
the next heartbeat, no restart. An org without a GitHub login
(grapevine) is named by its tenant slug. Until the platform half lands
the node keeps today's behaviour; the switch is by the platform's
answer, so the mini's upgrade is safe either way.

## Acceptance

mesh-cli tests: a registration answered with an owner and three orgs
yields one node id and three scopes, claims from each, a fourth org
appearing on the next heartbeat is served without restart, and an org
without login is named by slug. Live, after the platform half: one
Node row, 'owned by Chris' in status, every org he belongs to shows the
Mini, grapevine's items claimable.

## Items

| Seq | Title | Status | Branch | PR | Cost | Model |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Gateway: registration declares an owner, registerNode and heartbeatNode return the platform's answer, Org carries tenantSlug, the contract is a fixture | merged | mesh/the-node-serves-its-owners-orgs-1 | runsheet/mesh-cli#434 | $10.37 | opus |
| 2 | Node model: node.yaml declares the owner, one NodeID per node, scopes from the answer | failed | mesh/the-node-serves-its-owners-orgs-2 | — | $5.16 | opus |
| 3 | Loop: heartbeat once with its own id, take orgs from the answer | cancelled | — | — | $0.00 | opus |
| 4 | mesh node status names the owner and the orgs served | cancelled | — | — | $0.00 | sonnet |
| 5 | mesh-specs: the TRD's Node aggregate and node protocol, ADR-014 | merged | mesh/the-node-serves-its-owners-orgs-5 | runsheet/mesh-specs#102 | $2.02 | sonnet |
| 6 | README: bring your own node or run enterprise infra | cancelled | — | — | $0.00 | haiku |
| 7 | Node model (item 2 again, from its surviving branch) | failed | mesh/the-node-serves-its-owners-orgs-7 | — | $4.03 | opus |
| 8 | Loop: heartbeat once with its own id (again) | cancelled | — | — | $0.00 | opus |
| 9 | mesh node status (again) | cancelled | — | — | $0.00 | sonnet |
| 10 | mesh-specs: ADR-014 (item 5 again; already_done) | merged | mesh/the-node-serves-its-owners-orgs-10 | runsheet/mesh-specs#104 | $1.57 | sonnet |
| 11 | README (again) | cancelled | — | — | $0.00 | sonnet |
| 12 | Node model: Scope carries Slug, NodeAnswer becomes Scopes | failed | mesh/the-node-serves-its-owners-orgs-12 | — | $4.19 | opus |
| 13 | Registration and the loop: register once, legacy untouched | cancelled | — | — | $0.00 | opus |
| 14 | mesh node status on the owner contract | cancelled | — | — | $0.00 | sonnet |
| 15 | mesh-specs: the `contract:` field, the single owner probe, status/log wording | merged | mesh/the-node-serves-its-owners-orgs-15 | runsheet/mesh-specs#113 | $0.68 | haiku |
| 16 | README (again) | cancelled | — | — | $0.00 | haiku |
| 17 | Node model carried onto main from branch 12 | merged | mesh/the-node-serves-its-owners-orgs-17 | runsheet/mesh-cli#485 | $2.23 | opus |
| 18 | Registration and the loop: register once, hand-over picked up, first log lines name the owner | failed (3 attempts) | mesh/the-node-serves-its-owners-orgs-18 | — | $8.00 | opus |
| 19 | mesh node status + README | cancelled | — | — | $0.00 | sonnet |
| 20 | Loop: the owned node holds the answer | failed | — | — | $0.41 | opus |
| 21 | Start: registers once with its owner | cancelled | — | — | $0.00 | opus |
| 22 | mesh node status on the owner contract | failed | — | — | $0.50 | sonnet |
| 23 | README (again) | cancelled | — | — | $0.00 | haiku |
| 24 | Registration and the loop carried onto main from branch 18 | cancelled (2 attempts; PR closed unmerged) | mesh/the-node-serves-its-owners-orgs-24 | runsheet/mesh-cli#547 | $5.48 | opus |
| 25 | mesh node status and the rollback reporter follow node.yaml's contract | cancelled | mesh/the-node-serves-its-owners-orgs-25 | — | $6.19 | sonnet |
| 26 | README and goal doc (again) | cancelled | — | — | $0.00 | sonnet |
| 27 | Registration on the owner contract (branch 18's three failures designed out) | failed (3 attempts) | mesh/the-node-serves-its-owners-orgs-27 | — | $3.83 | opus |
| 28 | Loop on the owner contract | cancelled | — | — | $0.00 | opus |
| 29 | Start: registers through Register | cancelled | — | — | $0.00 | opus |
| 30 | mesh node status and the rollback reporter | cancelled | — | — | $0.00 | opus |
| 31 | README and goal doc (again) | cancelled | — | — | $0.00 | sonnet |
| 32 | Registration, the loop, status and the rollback reporter on the owner contract (PR #547's work, carried onto main) | review | mesh/the-node-serves-its-owners-orgs-32 | runsheet/mesh-cli#561 | $2.61 | opus |
| 33 | README and goal doc: this item | running | mesh/the-node-serves-its-owners-orgs-33 | — | — | sonnet |
| 34 | mesh-specs: ADR-014 to accepted with the node half's evidence | pending | — | — | — | sonnet |

Not every attempt above is on a distinct branch: several seqs (2/7/12,
3/8/13/18/20/24/27/28, 4/9/14/19/22/25/30, 6/11/16/19/23/26/31) are the
same design replanned after a failed or cancelled try, each on its own
branch when one exists. The sibling PR fixing the org-slug-matching bug
this design depends on, `runsheet/mesh-cli#512` (5e2b33e), is not a
formal item of this goal — see What landed.

## What landed

**Gateway: `RegisterNodeInput.Owner` and the platform's answer
(runsheet/mesh-cli#434, item 1).** `RegisterNodeInput` gains `Owner`
(`"person"` or `"enterprise"`), sent only when `TakesOwner(ctx)` finds
it on the introspected schema (cached an hour, like `takesScaling`).
`RegisterNode`/`HeartbeatNode` return `(*NodeAnswer, error)`: `NodeID`,
`Owner{Kind, ID, Name}`, `Scopes []ScopeAnswer{OrgID, Login, Slug,
AccountID}`. A payload with none of `nodeId`/`owner`/`scopes` parses to
`nil, nil`: a platform that doesn't take `owner` keeps the node on
today's per-org path, never an error. `gateway.Org` gains
`TenantSlug`. The contract is a fixture —
`specs/contracts/registerNode.json` and `heartbeatNode.json` — loaded
into `gatewaytest.Fake` and round-tripped through the real client.

**Node model carried onto main from branch 12 (runsheet/mesh-cli#485,
item 17).** `Scope` carries `Slug`; `Scope.Name()` falls back to it
when `Login` is empty — how grapevine, with no GitHub login, is named.
node.yaml declares `owner:` (`""`/`"person"`/`"enterprise"`).
`ScopesFromAnswer` turns a `NodeAnswer` into `Scopes`, every one
sharing the node's one `NodeID`, restricted by node.yaml's `orgs` the
same way `ResolveScopes` already was. The socket-free tests were moved
to run by exact name in the same PR.

**The org-slug-matching bug (runsheet/mesh-cli#512, commit 5e2b33e —
not a formal item of this goal, but a fix its own design depends on.)**
`ResolveScopes` and `Reloader` had matched node.yaml's `orgs:` against
an org's `Login` alone, so `--org grapevine` refused "not a member of
grapevine" even for an admin of it, and a reload dropped grapevine
every 30 seconds on the mini with "no org left to serve" — observed
live. The same PR fixed `releaseWorkItem`, which had been sending
`reason` (not a field) and no `nodeId` (`NON_NULL`): every release had
been failing, which is the "stalled: no heartbeat for 45 minutes"
signature across the estate. All three readers of the orgs list —
`ResolveScopes`, `ScopesFromAnswer`, `Reloader` — now agree.

**Registration, the loop, status and the rollback reporter on the
owner contract (runsheet/mesh-cli#561, item 32, in review — carries PR
#547's probe-first design onto main).** This is the code this branch
builds on (`internal/node/owned.go`, `internal/node/config.go`,
`internal/commands/nodecmd.go`). `Register` probes `TakesOwner` first,
before anything else — "the probe alone chooses the contract" — and a
platform that doesn't take `owner` runs today's per-org path
unchanged. One that does gets one `RegisterOwner` call under
`OwnedNodeID`; a nil answer notes `FallbackLine` and keeps the owner
contract under the node's one id with no scopes yet, never falling
back to per-org. `Loop.TakeAnswer` replaces the loop's scopes from
every non-nil heartbeat answer — logging `owner changed: <old> →
<new>` and `orgs <old> → <new>` — and keeps the last scopes on a nil
one. `SaveRegistration` persists `contract: owner` or `contract:
per-org`, and `node_id:` on the owner contract. `StartLine` is
node.log's first line after the start lines, printed before the
registration's own notes (`RegistrationLines`) so the owner and the
orgs are always named first. `mesh node status` and the rollback
reporter (`HeartbeatOutside`) heartbeat once under the saved
`OwnedID()` on the owner contract, whatever the answer — never falling
through to `MyOrgs` or per-org heartbeats on a nil one.

**mesh-specs#102 (item 5).** The TRD's Node aggregate and node
protocol gain the owner and scopes shape; ADR-014 "A node has an
owner, a person or an enterprise" records the decision.

**mesh-specs#104 (item 10).** Corrects ADR-014's Consequences section:
`Scope.Slug`, not `Scope.Login`, carries the tenant slug for an org
with no GitHub login — keeping the ADR consistent with the TRD and the
mesh-cli fixture.

**mesh-specs#113 (item 15).** A dated amendment onto both ADR-014 and
the TRD's node protocol: node.yaml's `contract:` field, the single
owner probe on the legacy path, and the status/log wording.

## Left to do

The live checks wait on the runsheet platform half, goal
`a-node-has-an-owner`. Ran `gh pr list --repo runsheet/runsheet --state
merged --search 'a-node-has-an-owner'` as asked: it answers `[]`. That
is not strong evidence either way — `gh api repos/runsheet/runsheet`
answers 404 for this node's own GitHub access (the same as
`runsheet/mesh-specs`), so this node's credential cannot see the
platform repo at all, and `mesh_projects` lists no `runsheet/runsheet`
project this node can read goals or items from either. The platform
half has not visibly shipped by any means available here; it has not
been confirmed absent.

Once it does, check on the mini:

- `mesh node status` shows **one** Node row for the mini, not one per
  org;
- its `owned by Chris` line, naming Chris as the mini's owner;
- every org Chris belongs to — runsheet, wardroomid, and grapevine once
  he is a member — shows the Mini on its Nodes page;
- grapevine's items are claimable: the node claims from it the same
  tick its first owner-contract heartbeat names it.

The mini's upgrade picks this up with **no node.yaml change**: `mesh
node upgrade --binary` swaps the binary and restarts at once
(adr-002's amendment, adopting the running sessions). The next `mesh
node start` probes `TakesOwner` fresh — the probe's own cache is
per-process — and, finding it true for the first time, registers once
under `OwnedNodeID` (node.yaml's existing `node_id:`, if a prior
per-org registration happens to have left one matching `(owner,
slug)`, else a fresh one) and `SaveRegistration` writes `contract:
owner` on its own, the first time it sees the contract change — logged
once, with the old and new value, like every other reload field.
`owner:` itself stays unset (person, the default) unless the mini is
later handed to an enterprise by editing it.

## Known gaps

- **An item in evaluation under an old per-org id is not matched at
  the first switch to the owner contract.** `internal/node/loop.go`
  and `internal/commands/nodecmd.go` match a running evaluation to the
  node's *current* id only. A node that switches from per-org to the
  owner contract mid-flight starts heartbeating and claiming under its
  one new id while an evaluation claimed under an old per-org id is
  still outstanding on the platform under that id. `LegacyIDs`
  (`internal/node/owned.go`) exists for the adoption side of this —
  sessions started under an org's old id are still this node's to
  adopt — but nothing yet reconciles an *evaluation* the platform still
  associates with an old id once the contract has switched. Not
  dangerous — the stray evaluation simply stalls and times out on the
  platform's own 15-minute timer rather than misleading anyone — but it
  is a gap, not a fix, and no item on this goal closes it.

- **The three failed designs of item 18, and why PR #547's probe-first
  design replaced them.** Branch 18 failed evaluation three times
  before being abandoned: attempt 1 called `ResolveScopes` before
  `TakesOwner`, so an `orgs:` restriction or an enterprise-served org
  could be rejected against the token holder's own `MyOrgs` before the
  platform's answer ever had a chance to supply its scopes; attempt 2
  treated a *non-nil* answer naming zero scopes the same as a nil one,
  silently falling back to per-org registration and letting
  `TakeAnswer` keep stale scopes across a heartbeat that should have
  cleared them; attempt 3 found the loop never retained the
  registration's own answer at startup, so a reload's `orgs:` check
  only worked once some *later* heartbeat had populated it. Item 27
  (branch 18's design "designed out") still failed three further times
  on a narrower question — whether a status-only empty answer (neither
  `nodeId`, `owner` nor `scopes` on the payload) is distinguishable
  from a genuine empty owner answer at the gateway layer — before PR
  #547 (branch 24, closed unmerged once its head was carried onto main
  as item 32 / PR #561) settled the shape now on this branch: the probe
  decides the contract first and alone, before any scope is resolved
  and before the loop's first tick, and a nil registration answer keeps
  the owner contract rather than ever falling back to per-org. Items
  28–32 build on that ordering without revisiting it.

## Models

From the items table: item 1 (#434) was planned sonnet but sharpened to
opus after a failed first evaluation (2 evaluations, $10.37 total).
Items 5 (#102, sonnet), 10 (#104, sonnet) and 15 (#113, haiku) each
passed their first evaluation, at $2.02, $1.57 and $0.68. Item 17
(#485) passed first time on opus ($2.23), as planned — and so did item
32 (#561), also opus ($2.61) — the only two of the registration-and-loop
redesign attempts (18, 20, 22, 24, 27 before it, every one planned and
run on opus or sonnet) to reach a passing evaluation at all; the rest
failed or were cancelled on replan first.

## Log

- 2026-09-29 · item 1 (#434) and mesh-specs#102 (item 5) merge: the
  gateway half and the TRD/ADR-014 land.
- 2026-09-29 · mesh-specs#104 (item 10) and #113 (item 15) merge: the
  ADR corrected and amended; item 17 (#485) merges the node model onto
  main.
- 2026-09-29–2026-10-02 · items 18–31: repeated registration-and-loop
  and status designs fail evaluation or are cancelled on replan (PR
  #547, branch 24, closed unmerged); #512 (5e2b33e) fixes org-slug
  matching and `releaseWorkItem` on the mini, observed live.
- 2026-10-02 · item 32 (#561) passes evaluation, carrying #547's
  probe-first design onto main; this document (item 33) records it with
  the mini's live checks still owed to the runsheet platform half.
