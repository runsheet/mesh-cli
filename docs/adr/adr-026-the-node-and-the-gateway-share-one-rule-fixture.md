---
summary: The node and the gateway check the node mutations — registerNode, heartbeatNode, claimWorkItem — against one recorded rule fixture, `internal/gateway/gatewaytest/testdata/node-mutations.json`, which names per mutation and bearer kind (person or machine) the input fields the gateway requires and refuses; runsheet generates it from its schema and gateway/scope_node.go and checks it in TestContracts, and mesh-cli's committed copy is refreshed from runsheet main, so a field the gateway starts to require is a failing test here, not a node refused in production.
status: accepted
---

# ADR-026: The node and the gateway share one rule fixture

2026-10-09, goal `the-node-and-the-gateway-share-one-register-contract`
(item 5: the fixture and this ADR; the Go loader, the fake's check and
the refresh workflow are later items of the goal).

## Context

Three times the two halves of a node contract did not meet, and each
time runsheet's tests passed, mesh-cli's tests passed, and only
production met:

1. **`owner` vs `ownerKind`** (2026-10-03). mesh-cli's ADR-015 probed
   `RegisterNodeInput` for a field named `owner`; runsheet ADR 039 had
   written `ownerKind`. Each side wrote a fixture and the two were never
   compared, so every probe answered false and the node stayed on its
   per-org path until the platform took the client's spelling (ADR 039,
   "The wire: owner as the client spells it").
2. **The node id derivation** (2026-10-03). The node sent an
   `aggregateId` of its own, `gateway.NodeID(ownerId, slug)`; the
   platform makes the row at `ids.NodeOwnerID(kind, ownerId, slug)`, a
   uuid5 of another shape, so a node heartbeated a stream that did not
   exist until `registerNode` answered with `nodeId`.
3. **`credentialId`** (2026-10-06/07). runsheet#2290 made the gateway
   (`gateway/scope_node.go`, the machine branch of `registersNode`)
   require `credentialId` on a machine bearer's registration; its e2e
   proved it with a fake node, and mesh-cli never sent the field. Both
   real nodes were refused `registerNode: not yours` on their machine
   credentials for a day, until a person read scope_node.go (ADR-015's
   amendment 2026-10-07).

The schema says which fields exist and which are non-null; it does not
say which a gateway rule requires or refuses for a given bearer.
`credentialId` is a nullable `UUID` in `RegisterNodeInput`, and nothing
the node introspects told it a machine bearer must send it. That rule
lives only in scope_node.go — so it has to be recorded where both
repos' tests read it.

## Decision

mesh-cli commits a copy of the rule fixture runsheet publishes, at
`internal/gateway/gatewaytest/testdata/node-mutations.json`, and the
gatewaytest fake validates every `registerNode`, `heartbeatNode` and
`claimWorkItem` input the node sends against it for the bearer kind the
fake is told it is serving — refusing an input that misses a required
field or carries a refused one, naming the field and the bearer kind.
The node's register, heartbeat and claim tests run on both bearer
kinds, so a machine-token registration without `credentialId` fails a
test here.

## The fixture format

One JSON object, written canonically — `json.dumps(d, indent=2,
sort_keys=True) + "\n"` — so a refresh's diff is the rule change and
nothing else:

```json
{
  "description": "…",
  "kind": "rules",
  "mutations": {
    "registerNode": {
      "inputType": "RegisterNodeInput",
      "machine": {"refused": ["replacesCredentialId"], "required": ["aggregateId", "credentialId", "name", "namespace", "slug"]},
      "person":  {"refused": ["credentialId", "owner"], "required": ["aggregateId", "name", "namespace", "slug"]}
    },
    "heartbeatNode": {"inputType": "HeartbeatNodeInput", "machine": {…}, "person": {…}},
    "claimWorkItem": {"inputType": "ClaimWorkItemInput", "machine": {…}, "person": {…}}
  }
}
```

- `kind` is `rules`, telling this file apart from the request/answer
  fixtures under `specs/contracts/` (`registerNode.json`,
  `heartbeatNode.json`), which record one exchange, not a rule.
- `mutations` is keyed by the GraphQL mutation name. Each carries
  `inputType`, the input type's name in `internal/gateway/schema.json`,
  so a loader can check every named field exists in the schema the node
  was generated from (ADR-010), and one object per bearer kind:
  `person` (an `rs_*` console token, or a person's session) and
  `machine` (a Latchkey machine credential, `mesh node login`).
- Each bearer's `required` and `refused` are sorted arrays of input
  field names. `required` is what the gateway refuses the call without
  for that bearer — beyond the schema's non-null, where the rule
  demands it (`credentialId` on a machine's `registerNode`). `refused`
  is what the gateway refuses the call for carrying from that bearer:
  `replacesCredentialId` from a machine (a machine re-registers only on
  the row's own credential, never replacing it — ADR 039,
  "Re-registration follows the keeper"), and `credentialId` and the
  pre-`ownerKind` spelling `owner` from a person (a person bearer
  naming a credential the row does not name is refused — ADR-015's
  amendment 2026-10-07).
- A field in neither list is the schema's business: allowed when the
  schema has it.

The first copy is transcribed by hand from runsheet ADR 039 and the
rule table in `gateway/scope_node.go`; every later copy is runsheet's
generated file, byte for byte.

## The runsheet generator

runsheet publishes `specs/contracts/node-mutations.json`, generated
from its GraphQL schema (the input types) and the rule table in
`gateway/scope_node.go` (the per-bearer required and refused fields),
and checked by `TestContracts`: the test regenerates it and fails when
the committed file differs, so a change to a node mutation's rule that
does not regenerate the fixture is a failing test in runsheet.
mesh-cli's committed copy is refreshed from it: the schema-refresh
workflow fetches `specs/contracts/node-mutations.json` from runsheet
main beside the schema and opens a pull request when it changes, and
that pull request's own tests — the fake validating the node's inputs —
say whether the node already sends what the new rule asks.

## Consequences

- A field the gateway starts to require of a bearer is a red pull
  request in mesh-cli on the next refresh, before any node runs a
  release that lacks it; the third mismatch above would have been a
  failing `registerNode` test on the machine bearer.
- A rule change is regenerated in runsheet, not edited by hand in
  mesh-cli: the copy here is never hand-edited after the first, as
  `fields_gen.go` is never hand-edited (ADR-010).
- Only the three node mutations are covered. Another mutation a node
  sends gains a row by the same format when its rule differs by bearer;
  the format needs no change for it.
- The fixture records rules, not wire shapes: the request/answer
  fixtures (`specs/contracts/registerNode.json`, `heartbeatNode.json`)
  stay as they are, and the derivation of the node's id stays the
  platform's answer (`nodeId`), not a rule here.
- AGENTS.md in both repos is to say that a change to a node mutation's
  rule regenerates the fixture (a later item of this goal).
