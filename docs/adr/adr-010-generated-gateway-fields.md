---
summary: The gateway client's per-entity field selections are generated into internal/gateway/fields_gen.go from a committed schema.json and one allow-list per entity (fields/<Type>.txt, one name per line, sorted, a trailing `?` marking a field an older platform may lack); CI regenerates against the committed schema, never the live gateway, and fails on a diff or on an allow-listed field the schema lacks.
status: accepted
---

# ADR-010: Generated gateway fields

## Context

`internal/gateway/client.go` kept each entity's GraphQL selection as
one string constant on one line — `workItemFields` alone was 264
characters — and a second constant or a literal for what an older
platform may lack (`workItemModelExtra`, the `desiredVersion` read in
`readNodes`). Any two features that add a field to one entity edit the
same line and conflict; client.go had 60 merge conflicts in the period
measured by goal `node-client-fields-are-generated`, most of them two
insertions at one point. Since `schema-is-public` the gateway's schema
can be introspected without a token.

## Decision

1. **A committed schema.** `internal/gateway/schema.json` holds every
   object, interface and input type the gateway serves with its fields
   and their types, types and fields sorted by name, one field per
   line. `go run ./internal/genfields -refresh` (from
   `internal/gateway`) introspects `https://api.runsheet.dev/graphql`
   and rewrites it. Only that step reads the live gateway.
2. **An allow-list per entity.** `internal/gateway/fields/<Type>.txt`
   (`WorkItemIndex`, `GoalIndex`, `ProjectIndex`, `DirectionIndex`,
   `NodeIndex`) names what the node selects on that type: one name per
   line, sorted, blank lines and `#` comments ignored. The node selects
   what its structs know, not everything the platform serves.
3. **Optional fields carry a `?`.** A field a platform the node still
   supports may lack ends in `?` (today `agentSlug?`, `evaluateModel?`,
   `model?`, `projectId?` on work items and `desiredVersion?` on
   nodes). The generator puts them in `<entity>Optional`; the client
   asks for `<entity>Fields` plus `<entity>OptionalFields` first and,
   once the gateway refuses one of the optional names, the required
   selection alone — the `itemsWithoutModel` and `nodesWithoutDesired`
   fallbacks, unchanged.
4. **The generated file.** `go generate ./internal/gateway/...` reads
   only schema.json and the allow-lists and writes `fields_gen.go`
   (`// Code generated ... DO NOT EDIT.`): per entity `<entity>Required`
   and `<entity>Optional` as `[]string`, one quoted field per line,
   sorted, and the selections joined from them at init. It fails naming
   the field when an allow-listed field is not in schema.json, and on a
   list out of order or repeated. The file is never edited by hand.
5. **CI checks against the committed schema.** The PR check runs
   `go generate ./internal/gateway/... && git diff --exit-code`: a stale
   `fields_gen.go`, or an allow-list naming a field the committed schema
   lacks, fails the build. CI never introspects the live gateway, so it
   is deterministic; refreshing schema.json is a commit of its own (by
   hand or by a scheduled bot).

`sessionToolFields` (a nested selection on the claim-time read) stays
hand-written.

## Consequences

- Adding a field is one line in one allow-list and a regenerated line
  in fields_gen.go; two features adding different fields touch
  different lines.
- A field the platform has not shipped cannot be selected until
  schema.json is refreshed with it — the rule that the platform half
  lands first, enforced by the generator.
- Selections are now sent in sorted order; GraphQL does not care.
- `gatewaytest/fake.go` derives from the same lists
  (`gateway.Fields`, `gateway.OptionalFields`, `gateway.Entities`): a row
  is served with every generated field (nil where unset), and a selection
  naming a field in neither list is refused as the gateway would. Its
  hand list of `HeartbeatNodeInput` stays: the public schema.json does not
  yet carry `limits`, `scaling`, `runtimes` or `adapters` (the platform
  half is not deployed), and the fake simulates platforms with and without
  them.

## Amendment 2026-10-02: protectedFiles, protectedRulePaths, protectedPaths

Goal `one-evaluator-for-ordinary-work` reads the item's and the
project's protected paths to decide whether an evaluation needs a
second opinion. `fields/WorkItemIndex.txt` gained `protectedFiles?`
and `protectedRulePaths?`, `fields/ProjectIndex.txt` gained
`protectedPaths?` — all three already in schema.json, so no refresh
was needed — and `readProjects` got the same probe-and-fallback the
item and goal reads already had: the optional selection first, the
required one alone once the platform refuses it.

An earlier attempt at this goal (branch
`mesh/one-evaluator-for-ordinary-work-1`) also added a `secondOpinion`
field to `EvaluateWorkItemInput`, plus `evaluators` and
`evaluatorsWhy`, and failed: the platform does not serve
`secondOpinion`, so selecting it was refused outright, not dropped as
an optional field would be. Rule 3 above is for a field the platform
*may* lack on an otherwise-known type; it does not let the client ask
for a field the schema has never carried. `secondOpinion`,
`evaluators` and `evaluatorsWhy` wait for the platform side to land
first (the standing rule: a process reacts only to events after its
own deploy) and are not selected, sent or named anywhere in
`internal/gateway` until it does.
