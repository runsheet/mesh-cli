---
summary: A repository opts into attested validation with `.runsheet/validate.yml` in the platform's schema — `suites:`, each a name, kind, command, paths, reports and jobs, plus the node's own timeout, always, artifacts and results; `internal/validate` loads it, selects the suites a diff affects (a suite with no paths is affected by every change) plus the always ones, runs them and produces the `Attestation`, whose suite hash is the platform's canonical `validatespec.SuiteHash` of the file. Amended 2026-10-03 from the first `steps:` shape built ahead of runsheet ADR 032.
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

## Amended 2026-10-03: the platform's schema and hash

Goal `the-nodes-attestation-reaches-the-platform`, item 4. ADR 032 is
now indexed (The file: `.runsheet/validate.yml`), and the first shape
above disagreed with it in the two places a pull request's verify step
reads: runsheet's own file is `suites:`, which loaded here as zero
steps, and the step hash above is not the hash the verify step
compares — it answers `wrong_suite` unless the attestation's
`suiteHash` equals `validatespec.SuiteHash` of validate.yml at the
base, and unless every suite `validatespec.Affected` names ran with
exit 0 (ADR 032, decision 2). So:

**The schema is the platform's.** `validate.Load`/`Parse` read
`suites:`, each entry `name`, `kind` (`lint | typecheck | unit | build
| integration`, `validate.Kind`), `command`, `paths` (repo-relative
globs; none means every change), `reports` (files the command writes)
and `jobs` (the CI check names it stands in for, the platform's), and
beside them the node's own `timeout`, `always`, `artifacts` and
`results`. `steps:` is read as a deprecated alias of the same shape, so
an old file still loads. Unknown keys are ignored. What the platform's
`Parse` refuses is refused here too — a suite with no name or command,
a repeated name, a kind not among the five, a path `protected.Clean`
refuses — except a missing kind, let through for an old `steps:` file.
Names, kinds, commands and paths are trimmed and paths deduplicated,
as the platform's are.

**Selection is the platform's `Affected` plus `always`.**
`validate.Select` takes every suite with no paths, every suite marked
`always`, and every suite one of whose paths matches a changed file, in
the file's order. It may take none — a diff that no suite's paths
match — and the run still produces and uploads an attestation whose
commands are empty (ADR-014's amendment): with nothing affected, the
verify step has nothing to find missing. `validate.Match` reads a bare
filename at any depth and a trailing `**` as zero or more segments,
which is never narrower than `platform/protected`'s matching, so the
node never runs fewer suites than the platform counts as affected.

**The suite hash is the file's canonical hash.**
`validate.SuiteHash(content)` (and `Config.Hash`) is the lower-case hex
sha256 of `json.Marshal` of the canonical suites in file order, each
`{name, kind, command, paths}` — `canonicalSuite` in
`internal/validate/hash.go`, copied from runsheet/runsheet
`platform/validatespec/validatespec.go` at
`1d08f03b6c84804ef46d3e9306424400057b03df` with the same fields, order,
tags and a nil `paths` written `[]`. A comment, a blank line, key order,
`reports`, `jobs`, `timeout`, `always`, `artifacts` and `results` leave
it; `name`, `kind`, `command`, `paths` and the suites' order change it
(`TestSuiteHashIsTheCanonicalSuitesNotTheBytes`). It is pinned against
the platform's own answer for runsheet's validate.yml
(`testdata/runsheet-validate.yml`, `84015ff3…`,
`TestSuiteHashIsThePlatformsOnRunsheetsFile`). The per-step hash of
matched files and `StepResult.suiteHash` are gone; the attestation's
`validation.hash` is the file's `SuiteHash`.

**Reports.** A step's report is its `results` file or stdout, else the
`reports:` files it names that exist, cut so a run's reports are at
most 256 KiB in all (ADR 032).

This repository's `.runsheet/validate.yml` is `suites:`, `vet` kind
`lint`, `build` kind `build`, `unit` kind `unit`, keeping `always`,
`timeout` and `results`.

## Amended 2026-10-15: a checkout's dependencies install before its suites

A suite's `command` is an npm script the repository has (runsheet ADR
032), and an npm script needs the repository's `node_modules`. A
builder's workspace has them — its session installed them — and that
is the only checkout this package ever validated until ADR-032 (a node
validates a merge batch candidate) ran the suites on a fresh worktree:
on the dl360, 2026-10-15, every one of runsheet/console's 17 merge
batches since batching began died in under a second, `typecheck`
exiting 127 (`sh: tsc: not found`, read as `tool_missing`), and no
console pull request merged for a day.

**`validate.Install(ctx, dir, runner)`** runs the install the checkout
needs before `validate.Run`, through the same `Runner` the suites use
(so in the session's pod, with the node's shared store mounted,
ADR-028), under `DefaultTimeout`. `validate.InstallCommand(dir)`
reads it from the files: a `package.json` beside a `pnpm-lock.yaml` is
`pnpm install --frozen-lockfile`, a `yarn.lock` `yarn install
--frozen-lockfile`, a `package-lock.json` or `npm-shrinkwrap.json`
`npm ci`, `package.json` alone `npm install`; no `package.json`, or a
`node_modules` already there, is none. Go, Python and the rest fetch
their own modules as their suites run and are not this amendment's.
`validator.run` (internal/node) calls it when the diff selected at
least one suite, logs `validation: dependencies installed with <cmd>
in <ms>ms`, and on a failure returns the `*validate.InstallError` —
kinded **`deps_install`**, a new member of ADR-030's closed set — so
the run is the environment's (the item parks, the batch is not
attested) and the suites never ran on a checkout with no dependencies.
The install is not a step of the attestation: the platform verifies the
suites `validate.yml` declares, and the install is the node's own
preparation, like the pod and the services before it.

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
