---
summary: A repository opts into attested validation with `.runsheet/validate.yml` — named steps, each a command, an "affected by" path list, a timeout, artifacts to digest and where its test results are; `internal/validate` loads it, selects the steps a diff touches plus the always ones, runs them and produces the signed-shape `Attestation`. Built ahead of runsheet ADR 032 landing on mesh-context, to the shape the goal's brief describes.
status: accepted
---

# ADR-013: Validation runs from a declared config

2026-09-28, goal `node-attests-its-validation` (item 1).

## Context

runsheet ADR 032 (attested-validation-replaces-duplicate-ci) has the
platform sign and store a validation attestation as TestEvidence, with
the node's part of the work left as "the follow-up in mesh-cli". As of
this item neither that ADR nor the goal `ci-uploads-test-evidence` is
indexed on mesh-context (`mesh_document`/`mesh_search` against
runsheet/runsheet found neither), so this package is built to the shape
the item's brief describes rather than the ADR's own words. Item 2
(`gateway.RecordValidationAttestation`, #318) already shipped ahead of
this one, on a `ValidationAttestation` type whose doc comment names this
package explicitly and expects a later item to convert one into the
other, field for field — the two shapes are related but not identical
(see Consequences).

## Decision

**`.runsheet/validate.yml`** is one `steps:` list, each entry a `name`
(lint, typecheck, unit, build, or any integration suite's own name), a
`command` (a shell string, run with `sh -c`), `paths` (gitignore-style
globs — the "affected by" list), `timeout` (a Go duration, default
10m), `always` (bool), and two optional fields: `artifacts` (globs
digested after the step runs) and `results` (where its tests are:
`stdout` for `go test -json` on the step's own stdout, `go-test-json:
<path>` or `junit:<path>` for a file at that path, relative to the repo
root). A repository with no such file is `validate.ErrNoValidation`,
not a failure — the caller skips validation, it does not fail the
build.

**Path matching is a small doublestar-lite** (`validate.Match`): each
non-`**` segment through `path.Match` (so `*`, `?`, `[...]` work within
a segment) and a `**` segment matching zero or more path segments, so
`**/*.go` and `internal/**` both work; a bare pattern with no `/`
matches a file of that name at any depth. Neither `bmatcuk/doublestar`
nor any other glob library was already in go.mod (only
`ryanuber/go-glob`, single-segment, an indirect dependency of something
else) — the brief allowed either that or `path.Match` per segment,
and a dependency-free ~40 line implementation over the small pattern
set validate.yml needs won out over adding one for it.

**`Select(cfg, changedPaths)`** returns every step marked `always` plus
each step whose `paths` match any changed path, in the config's own
order, each once — pure and config-driven, so the node's own diff
(`git diff --name-only`, later items) is the only caller-side state.

**`Run(ctx, steps, Runner, opts)`** drives an injected `Runner`
interface (`Run(ctx, cmd, timeout, stdout, stderr) (exitCode, err)`) so
a later item's sandboxed runner is a second implementation, not a
rewrite. `HostRunner` (the unsandboxed path, until the sandbox runtime
is present, matching the goal's "on the host until the sandbox runtime
is present, saying so in the attestation") runs `sh -c` in the
workspace. **It puts the child in its own process group and kills the
whole group at the timeout** (`setProcessGroup`/`killGroup`,
`host_unix.go`/`host_windows.go` — the same pattern
`internal/sessionunit` already uses for a live session): killing only
the direct `sh` process leaves a forked grandchild (a real step's
command is rarely a single exec-able binary; even `sleep 5` inside `sh
-c` forks rather than exec-replacing) still running and holding
stdout/stderr open, which hangs `cmd.Wait()` indefinitely since Go's
default `exec.CommandContext` cancellation only signals the one
process it started. A step that times out reports exit code 124 (the
convention GNU `timeout` itself uses) and `timedOut: true`. The first
step whose command errors or exits non-zero stops the run unless
`opts.ContinueOnFailure`.

**`SuiteHash(dir, step)`** is `sha256(command || git-blob-hash(f) ||
f for f in the step's matched files, sorted)` — the git blob hash
(`"blob "+len+"\0"+content`, sha1, the same value `git hash-object`
reports) rather than a plain content hash, so it lines up with what a
`git ls-tree` of the same paths would show. It changes exactly when the
command or an affected file's content changes, and not when an
unrelated file does (`TestSuiteHashChangesWhenAnAffectedFileChanges`).

**`Attestation`** carries `commit`, `tree` (`git rev-parse
<commit>^{tree}`, so it does not depend on `HEAD` matching `commit` at
build time), `runnerImage` (the caller's own string — an image digest,
or `host:<hostname>`), `sandboxed`, `steps` (`[]StepResult`: name,
command, suiteHash, exitCode, durationMs, testsRun/testsPassed/
testsFailed, output — head and tail, capped 8 KB total — timedOut,
artifacts `[{path, sha256}]`), `nodeId`, `sessionId`, `itemId`, `at`
(RFC 3339 UTC) and `validation` (`{file: ".runsheet/validate.yml",
hash}` — the config's own sha256, so the attestation stands on its own
even if validate.yml later changes). `Build(dir, commit, results,
meta)` assembles it; `Failed()` names the first failing step;
`MinutesByStep()` is each step's whole minutes, rounded up, for the
usage record's per-step validation minutes.

**Dogfood**: this repo's own `.runsheet/validate.yml` runs `vet` and
`build` always, and `unit` (`go test -json ./...`, `results: stdout`)
on `**/*.go`, `go.mod`, `go.sum` — the same three commands
`go vet ./... && go build ./cmd/mesh/ && go test ./...` AGENTS.md
already asks of every change, now declared rather than only documented.

## Consequences

- `internal/validate`'s `Attestation`/`StepResult` json shape is
  **not** byte-identical to `gateway.ValidationAttestation`
  (`step`/`ms`/`artifacts []string` there vs. `name`/`durationMs`/
  `artifacts []{path,sha256}` here) — that type's own doc comment
  already names this as expected and asks a later item to convert
  field for field. That conversion, the node wiring (running validation
  after a build commits, before `FinishWorkItem`, uploading through
  `RecordValidationAttestation` with the item token) and the sandboxed
  `Runner` are later items of this goal, not this one.
  `harness.HandBackPrompt`'s "commit what works and hand back" turn on
  a failing step is likewise a later item's wiring, not this package's.
- A step's `results` syntax (`stdout` / `go-test-json:<path>` /
  `junit:<path>`) is this package's own invention, not yet confirmed
  against ADR 032's actual text — worth checking once the document is
  indexed, alongside the `Attestation` field-for-field mapping above.
- `HostRunner`'s process-group kill is Unix/Windows-specific
  (`host_unix.go`/`host_windows.go`, mirroring
  `internal/sessionunit/proc_unix.go`/`proc_windows.go`); a future
  sandboxed `Runner` needs no such handling since Podman itself owns
  the process tree.
