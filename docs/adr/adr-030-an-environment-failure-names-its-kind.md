---
title: "ADR-030: An environment failure names its kind"
summary: A validation that fails on the node rather than the code carries one of a closed set of environment kinds — step_timeout (a step's exit 124), tool_missing (exit 127), credential_refused (git or gh refused by GitHub on the item's token), image_pull_refused (the registry refusing the sandbox image's pull), runtime_broken (podman itself unable to start a pod: its rootless storage corrupted or locked, its user namespace refused, or no runtime there at all) — on the attestation's command as environmentKind and at the head of every failWorkItem, releaseWorkItem and updateError reason as `<kind>: `, so the platform parks the item instead of re-planning it; any other non-zero exit carries none. A tool_missing reason names the missing tool and the step's command (amendment 2026-10-16).
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

## Amendment 2026-10-16: a tool_missing reason names its command

Goal `a-node-low-on-disk-claims-nothing-and-a-missing-tool-is-named`
(items 1 and 3). Merge batch 17 failed with `tool_missing: validation
failed: step X exited 127` (anythink node.log, 2026/10/15 17:53 local):
the kind was right and the reason said nothing of what was missing, so
the operator had to find the step's command in validate.yml and guess
which program in it the pod lacked.

A `tool_missing` step's reason now names the missing tool and the
command it ran:

    tool_missing: validation failed: step <name> exited 127: command not found: <tool> (in `<command>`)

- **The tool** is read from the step's own output by
  `validate.MissingTool(output)`, in whichever words the shell said it:
  dash's `sh: 1: <tool>: not found`, POSIX `/bin/sh: <tool>: not found`,
  bash's `bash: <tool>: command not found`, and Go's exec package's
  `exec: "<tool>": executable file not found in $PATH` for a step run
  with no shell. The first match wins.
- **None matched** — a `make` target or a script that returned 127 for
  its own reasons — the reason names the command alone:
  ``validation failed: step <name> exited 127: `<command>` ``.
- **Every other kind and a code failure** keep their wording,
  `validation failed: step <name> exited <code>`.

`validationFailure.Error()` (`internal/node/validation.go`) renders it,
and every path that reports a validation failure goes through
`validate.Reason`: a build's `failWorkItem`, a merge batch candidate's
`failMergeBatchValidation` and a main validation's
`failWorkItemMainValidation` (ADR-032), and node.log's `… not attested`
and `… failed:` lines. A candidate whose tool is missing is failed, not
attested red — an environment failure is no verdict on the candidate
(ADR-032) — so the reason is where the command is named; a build's
attestation still carries the step's `command` beside `environmentKind:
tool_missing`. The attestation's shape is unchanged: no field is added,
the contract is not touched.

## Tests

`internal/validate/envkind_test.go`'s `TestEnvironmentKindOfEachFailure`
(124, 127, a credential refusal, an image-pull refusal each their kind;
exit 1 and a pass none) and `TestHostRunnerMissingToolIsToolMissing`;
`internal/node/envkind_test.go`'s
`TestValidationEnvironmentKindReachesTheGateway` (the attestation's
command and the `failWorkItem` reason on the fake gateway for 124, 127, a
refused pull and exit 1) and `TestCredentialReleaseReasonCarriesItsKind`
(the `releaseWorkItem` reason).

The amendment: `internal/validate/missingtool_test.go`'s
`TestMissingToolFromOutput` (sh, bash, `/bin/sh`, Go's exec, no match);
`internal/node/toolmissing_test.go` (the build's reason, the fallback to
the command, exit 1 unchanged, a candidate not attested); and
`internal/node/toolmissing_e2e_test.go`'s `TestToolMissingE2E` — a merge
batch candidate on the node's own Loop whose step exits 127 in the pod,
its `failMergeBatchValidation` reason on the fake gateway naming
`tool_missing`, the tool and the command and nothing attested; and a
build whose step really exits 127 on the host, its attestation's command
`environmentKind: tool_missing` with the command and its `failWorkItem`
reason the same wording.

## Amendment 2026-10-15: `runtime_broken` — a container runtime that cannot start a pod

Goal `a-runtime-that-cannot-start-a-session-stops-claiming`, item 1.
Between 2026-10-15 23:05 and 2026-10-16 00:49 a root `podman` run on
grapevine poisoned the node user's rootless storage (memory
`root-podman-poisons-rootless-storage-2026-10-16`): every sandboxed
session's pod failed, each reported as a plain code failure and
retried and re-planned like one, because nothing told the two apart.

`validate.EnvironmentKind` gains a fifth member, `runtime_broken`: the
container runtime itself unable to start a pod, never the agent's own
command failing inside one that started fine. `validate.IsRuntimeBroken`
matches podman's own words for it:

| words | what they mean |
| --- | --- |
| `error opening storage` | the rootless store will not open |
| `creating temporary storage` | … or lock, including an overlay `permission denied` under it |
| `database configuration mismatch` | the store's own book-keeping disagrees with what is on disk |
| `readlink …containers/storage…` / `…/overlay/…: no such file or directory` | the store's book-keeping points at a layer that is gone |
| a `permission denied` naming a path under `containers/storage` or `/overlay/` | the same, scoped to the store's own paths so an unrelated permission failure of the agent's own command is never caught by it |
| `cannot set up namespace` / `newuidmap` | the user namespace podman needs could not be made |

A pull podman refuses stays `image_pull_refused` (checked first in
`EnvironmentKindOf`); every other non-zero exit, including an ordinary
exit 1, stays uncategorised, as before.

**`internal/sandbox` wraps it at the source.** `*sandbox.RuntimeError`
(`Op`, podman's own words, `Unwrap`) is `validate.RuntimeBroken`'s
`Kinded`: `Podman.CreatePod` (the pod and its network-rules init
container), `RunAgent`'s container create and `StartAgent` answer one
when podman's own output names the runtime rather than the agent's exit
— including a podman that did not run at all, which `RunAgent` now
always wraps rather than returning bare. `(*Sandbox).Probe(ctx) error`
is the cheapest check that the runtime can start a pod at all: its
version, then a throwaway pod labelled `mesh.probe`, created and removed
at once; a runtime that cannot answers the same `*RuntimeError`. `Fake`
gains `CreatePodErr` (honoured by `Probe` alongside `VersionErr`) so
`internal/node` can break and heal a runtime in a test.

**Where this is headed.** The node side of `claiming stops` — probing
the runtime before it claims, holding and reporting `runtime_broken` on
the heartbeat, and the platform parking the item on it — is later items
of the goal; this item is the typed failure and the probe alone, so
those items build on a classification that already has its own tests.

### Tests

`internal/validate/envkind_test.go`'s `TestIsRuntimeBroken` (each
podman phrase above its own kind; an agent's own `permission denied` or
`readlink` naming no storage path, an image-pull refusal and an
ordinary test failure none) and the new cases in
`TestEnvironmentKindOfEachFailure` (podman's 125 with each phrase,
`runtime_broken`). `internal/sandbox/podman_test.go`'s
`TestCreatePodWrapsARuntimeBrokenFailure`,
`TestCreatePodWrapsARuntimeBrokenNetworkRulesFailure`,
`TestCreatePodLeavesAnOrdinaryNetworkFailureUnwrapped`,
`TestRunAgentWrapsARuntimeBrokenFailure`,
`TestRunAgentWrapsAPodmanThatDoesNotRunAtAll`,
`TestRunAgentLeavesAnOrdinaryExitUnwrapped` and
`TestStartAgentWrapsARuntimeBrokenFailure`;
`internal/sandbox/sandbox_test.go`'s `TestProbeOnAHealthyRuntime`,
`TestProbeOnABrokenRuntime` and
`TestRuntimeErrorReasonOpensWithItsKind` (`validate.Reason` opens
`runtime_broken: `).
