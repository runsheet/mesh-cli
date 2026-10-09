---
summary: The fields the gateway requires and refuses on registerNode, heartbeatNode and claimWorkItem, per bearer kind (person or machine), are one JSON fixture — `internal/gateway/gatewaytest/testdata/node-mutations.json`, loaded by `gatewaytest.LoadNodeMutationRules` and applied by `BearerRule.Check` — that runsheet's TestContracts is to generate as `specs/contracts/node-mutations.json` and mesh-cli's schema-refresh is to pull; until runsheet publishes it the committed copy is the contract.
status: accepted
---

# ADR-026: The node and the gateway share one rule fixture

2026-10-09, goal `the-node-and-the-gateway-share-one-register-contract`
(item 1). Builds on runsheet's ADR 026 (*a cross-repo contract is a
published fixture, not prose*) and amends nothing in mesh-cli's
ADR-015; it records the rules ADR-015's credentialId amendment
describes in a form a test can read.

## Context: three mismatches

Three times a node contract's two halves did not meet, and each time
runsheet's tests and mesh-cli's tests both passed:

1. **`owner` vs `ownerKind`** (2026-10-03). mesh-cli probed and sent
   `owner`; the platform's `RegisterNodeInput` took `ownerKind`. The
   foreign fixtures `specs/contracts/registerNode.json` were "the same
   JSON, checked by inspection", and nothing compared them (runsheet ADR
   026, *A foreign fixture is verified, not generated*).
2. **The node id derivation** (2026-10-03). mesh-cli's `gateway.NodeID`
   and the platform's uuid5 (`ids.NodeOwnerID(kind, owner, slug)`)
   gave the same node two different ids.
3. **`credentialId`** (2026-10-06/07, runsheet#2290). The gateway began
   to require `credentialId` — the bearer's own machine id — on a
   machine bearer's `registerNode` (`registersNode`, the machine branch
   of `gateway/scope_node.go`). runsheet's e2e proved it with a fake
   node; mesh-cli never sent the field, and both real nodes were refused
   `not yours` on their machine credentials for a day until a person
   read `scope_node.go` (ADR-015's amendment 2026-10-07).

The published mutation fixtures (`/contracts/registerNode.json` and
the rest) record *an input the rig accepted*. They say what fields
exist, not which a bearer must send and which it must not, so a
newly required field never shows as a difference in them. The rules
live in the gateway's scope files as code.

## Decision: the fixture format

The rules are one file, `node-mutations.json`, written so that Go's
`encoding/json` reads it and writes it back unchanged: keys sorted,
two-space indent, a trailing newline, every list sorted.

```json
{
  "kind": "rules",
  "mutations": {
    "registerNode": {
      "inputType": "RegisterNodeInput",
      "machine": {"refused": [...], "required": [...]},
      "person": {"refused": [...], "required": [...]}
    },
    "heartbeatNode": {...},
    "claimWorkItem": {...}
  },
  "source": "gateway/scope_node.go, gateway/scope_workitem.go"
}
```

Field by field:

- `kind`: always `"rules"`, beside runsheet ADR 026's `query`, `http`
  and `mutation` fixtures.
- `source`: the gateway files the rules are read from, comma-separated.
- `mutations`: keyed by the GraphQL mutation name.
  - `inputType`: the mutation's input object in the gateway's schema.
    Every field a rule names must be an input field of it.
  - `person`, `machine`: one rule per bearer kind. `person` is a
    person's `rs_*` console token or Latchkey sign-in; `machine` is a
    Latchkey machine credential (`mesh node login`).
    - `required`: fields the input must carry, present with a value
      other than null or `""`.
    - `refused`: fields the input must not carry, by the same test. The
      gateway reads an empty `credentialId` as none.

A field that is in neither list is the bearer's to send or leave out.

### The rules as of 2026-10-06

- **registerNode** (`RegisterNodeInput`). Both bearers require
  `aggregateId`, `name`, `namespace`, `ownerKind` and `slug`
  (`namespace` because the schema marks it non-null). A machine bearer
  also requires `credentialId`, its own machine id, and is refused
  `replacesCredentialId`. A person bearer is refused `credentialId`,
  because the credential argument must equal the empty
  `Principal.Machine`, and `owner`, the gateway's older spelling of
  what mesh-cli sends as `ownerKind`.
- **heartbeatNode** (`HeartbeatNodeInput`). Both bearers require what
  the schema marks non-null, `aggregateId` and `namespace`. Nothing is
  refused.
- **claimWorkItem** (`ClaimWorkItemInput`). Both bearers require
  `aggregateId` and `namespace`. Nothing is refused.

Undecided, in neither list:

- `claimWorkItem.nodeId`: the published fixtures show a machine
  sending it and a person's `claimWorkItem-person` not, and the schema
  makes it nullable. `scope_workitem.go` could not be read from this
  session (the item token reaches mesh-cli alone), so whether either
  bearer must send it is not settled; the runsheet generator settles
  it.
- The heartbeat's and claim's machine-only check that the row names
  the bearer's credential (`machineIsNode`) is about a value, not a
  field, so it is not a rule here.

## Decision: the loader

`internal/gateway/gatewaytest/rules.go` embeds the committed copy.

- `LoadNodeMutationRules() (NodeMutationRules, error)` returns the
  rules, a `map[string]MutationRule` keyed by mutation.
  `LoadNodeMutationFixture()` returns the whole file as
  `NodeMutationFixture`, whose fields are declared in key order so it
  marshals back byte for byte.
- `MutationRule{InputType string; Person, Machine BearerRule}`.
  `For(bearer)` picks the rule for `"person"` or `"machine"`
  (`BearerPerson`, `BearerMachine`).
- `BearerRule{Required, Refused []string}`. Its `Check(input
  map[string]any) error` returns `*FieldError{Mutation, Bearer, Field,
  Missing}` for the first required field missing, else the first refused
  field present. The error reads `registerNode: machine bearer: required
  field "credentialId" missing` or `registerNode: machine bearer:
  refused field "replacesCredentialId" present`.
- `NodeMutationRules.Check(mutation, bearer, input)` does the same by
  name. A mutation the fixture does not name passes.

The fake's validation of every input the node sends (goal item 3) and
the node's tests on both bearer kinds (item 4) are built on this.

## Decision: who generates it

- **runsheet generates it.** runsheet/runsheet's `e2e/TestContracts`
  is to write `specs/contracts/node-mutations.json` in exactly this
  format. It builds the file from the gateway's rule table (the
  `RegisterMutation` scopes of `gateway/scope_node.go` and
  `gateway/scope_workitem.go`) and the gateway's input types, and CI
  refuses a stale copy as it refuses every other contract. It is
  published at `https://api.runsheet.dev/contracts/node-mutations.json`.
- **mesh-cli pulls it.** The schema-refresh workflow (goal item 2)
  fetches it beside `schema.json` into
  `internal/gateway/gatewaytest/testdata/node-mutations.json` and opens
  a pull request when it changes. That pull request is where a newly
  required field turns into a failing mesh-cli test.
- **Until runsheet publishes it, the committed copy is the contract.**
  It was written by hand from the rules above, the gateway's schema
  (`internal/gateway/schema.json`, refreshed on this branch so that
  `RegisterNodeInput` carries `ownerKind`, `owner` and
  `replacesCredentialId`) and the gateway's published mutation
  fixtures. A change to a node mutation's rule in the gateway changes
  this file.

## Proof

`internal/gateway/gatewaytest/rules_test.go`:

- `TestNodeMutationRulesCoverEveryInputField`: registerNode,
  heartbeatNode and claimWorkItem are present, and every required or
  refused field is an input field of its `inputType` in `schema.json`.
- `TestNodeMutationRulesRoundTrip`: decode and re-marshal produce the
  file byte for byte.
- `TestNodeMutationRulesRegisterNode`: the credentialId rules, and a
  whole registration passing on each bearer.
- `TestFieldErrorNamesMutationBearerAndField`: a `*FieldError` naming
  mutation, bearer and field for a missing required field and for a
  present refused one.
