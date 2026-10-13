---
title: "ADR-030: An environment failure names its kind"
summary: A validation that fails on the node rather than the code carries one of a closed set of environment kinds — step_timeout (a step's exit 124), tool_missing (exit 127), credential_refused (git or gh refused by GitHub on the item's token), image_pull_refused (the registry refusing the sandbox image's pull) — on the attestation's command as environmentKind and at the head of every failWorkItem, releaseWorkItem and updateError reason as `<kind>: `, so the platform parks the item instead of re-planning it; any other non-zero exit carries none.
status: accepted
---

# ADR-030: An environment failure names its kind

2026-10-13, goal `a-node-validates-every-enrolled-repo-with-the-tools-it-needs`
(item 4). Amends [ADR-013](adr-013-validation-runs-from-a-declared-config.md)
and [ADR-014](adr-014-a-build-validates-before-it-pushes.md) (what a step's
result and the attestation carry) and ADR-001's credential release; the
platform half is runsheet goal
`an-item-that-fails-on-its-environment-is-parked-not-replanned`.

## Context

Between 2026-10-05 and 2026-10-12 items failed on the node, not on their
code, and each failure was reported like a code failure, so the item was
reworked and re-planned: latchkey-mints-a-session-id-token alone ran 46
items and 9 narrow re-plans. The console's `npm run check` ran `tsc` with
no `node_modules` and exited 127; runsheet's integration step timed out
at exit 124; the validator's `git fetch` and the builder's `git push` were
refused by GitHub; the sandbox image pull of
`ghcr.io/runsheet/mesh-node:dev` was unauthorized. Items 1–3 of the goal
fixed the causes (the image's tools, the item's token on fetch and push,
the registry credential on pull); this records how the next such failure
is told apart.

The platform half had fixed no names when this landed (its items 1–3
still in evaluation or pending, `mesh_items` 2026-10-13), so the names
below are the node's proposal and the platform reads them.

## Decision

`validate.EnvironmentKind` is a closed set (`validate.EnvironmentKinds`):

| kind | how it is detected | |
| --- | --- | --- |
| `step_timeout` | a step's exit 124 — `HostRunner`'s timeout, `sandboxRunner`'s runtime timeout, GNU `timeout`'s | `validate.Run` |
| `tool_missing` | a step's exit 127 — the shell found no such command | `validate.Run` |
| `credential_refused` | an error whose text is git or gh refused on its credential (`validate.IsCredentialRefusal`: `Repository not found`, a 401/403, `Write access to repository not granted`, …), or a `*CredentialError`/`*PushCredentialError` | the validator's fetch (`changedPaths`), a workspace's clone, the build's push |
| `image_pull_refused` | an error, or a step's podman exit 125 with output, naming a pull (`pull failed`, `initializing source`, `reading manifest`, `trying to pull`) and a refusal (`unauthorized`, `denied`, `forbidden`, 401/403) — `validate.IsImagePullRefusal` | a sandboxed step whose pod could not pull; the self-update's `EnsureImage` |

`validate.EnvironmentKindOf(step, err)` is the classifier: err first — a
kind it carries (an error with `EnvironmentKind()`, or a reason already
opening with one), an image pull's refusal, a credential refusal — then
the step's exit. Exit 0 and every other non-zero exit, exit 1 included,
are a code verdict: no kind.

**Where it is carried.**

- **The step and the attestation**: `StepResult.EnvironmentKind`, set by
  `validate.Run` on a failing step; the attestation's command
  `environmentKind`, left out when empty (`gateway.ValidationCommand`,
  `specs/contracts/recordValidationAttestation.json`'s second command).
  The input is a `Map`, so a platform that does not read it ignores it.
- **The reason**: every reason the node sends opens with `<kind>: `
  (`validate.WithKind`, read back by `validate.KindOfReason`) —
  `failWorkItem`'s through `Runner.fail` (`validate.Reason`: the
  validation failure `tool_missing: validation failed: step check exited
  127`, a classified validator error `credential_refused: validation:
  changed paths: …`, a refused push), `releaseWorkItem`'s through
  `ReleaseOnCredential` (`credential_refused: github credential: …`),
  and a self-update's `updateError` on a refused pull
  (`image_pull_refused: node image …: pull failed: …`).
- **node.log**: `validation step <name> exited <code> in <ms>ms (<kind>)`.

The node still fails, releases or retries as before: a failing step gets
the builder its one more turn and then fails the item, a credential
refusal at the workspace releases it. Whether an environment failure is
charged an attempt, parked or offered again is the platform's decision on
the kind, not the node's.

## Consequences

- The platform can park an environment failure as an infrastructure hold
  rather than spend the goal's budget re-planning it, from either the
  attestation or the reason, without parsing git's or podman's words.
- The set is closed: a new kind is an amendment here and on the platform.
  A kind detected by text (credential, pull) is as good as the patterns;
  a code failure whose output happens to say `unauthorized` is never
  kinded unless podman itself exited 125.
- An exit 124 or 127 a step's own script returns for its own reasons is
  read as the environment's. That is the price of reading exits, and the
  platform's hold, not a re-plan, is the cheaper mistake.

## Tests

`internal/validate/envkind_test.go`'s `TestEnvironmentKindOfEachFailure`
(124, 127, a credential refusal, an image-pull refusal each their kind;
exit 1 and a pass none) and `TestHostRunnerMissingToolIsToolMissing`;
`internal/node/envkind_test.go`'s
`TestValidationEnvironmentKindReachesTheGateway` (the attestation's
command and the `failWorkItem` reason on the fake gateway for 124, 127, a
refused pull and exit 1) and `TestCredentialReleaseReasonCarriesItsKind`
(the `releaseWorkItem` reason).
