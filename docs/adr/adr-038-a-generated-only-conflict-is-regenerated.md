---
title: "ADR-038: A generated-only conflict is regenerated"
summary: A requested rebase first tries the mechanical path — when `.runsheet/validate.yml` declares `generated` (paths, generate, check), the node rebases onto the base itself, resolves each conflict confined to the generated paths by taking the base's side and running `generate`, and claims success only when every `check` passes, then pushes with a lease, attests and finishes with no account opened, no session and no model cost (the rebase kind with harness none of runsheet ADR 061); a conflict in any other path, a failing generate or check, or anything else falls through, the checkout reset to origin/<branch>, to today's rebase session unchanged.
status: accepted
---

# ADR-038: A generated-only conflict is regenerated

2026-10-19, goal
`a-generated-file-conflict-is-regenerated-not-rebased-by-hand` (items
1–3). Follows runsheet `adr-061-mesh-is-a-job-system` (a rebase kind
with harness none) and runsheet ADR 008 (the rebase); leans on ADR-014
(the validation's runner and the rebased head's attestation) and
ADR-028 (a rebase gets its own worktree).

## Context

On 2026-10-18 runsheet#4615 waited hours for a rebase-session slot
while its only conflict was in generated files — `platform/registry_gen.go`
and the loom output — that a person resolved twice by hand with the
same recipe: take main's side of each generated file, run the
generators over it, run `generate --check`, `go build ./...` and
`go vet ./...`, `git rebase --continue`, push with `--force-with-lease`.
No judgement was in it; a session was a model account, a slot and an
hour for a shell script.

The same day showed where the recipe stops: a textual conflict in a
comment in `cmd/runsheet/main.go`, and a silent auto-merge that left
two `Blobs` fields in `platform/processes/services.go`, both needed a
session or a person. The second conflicted nowhere; only the repo's
build caught it.

## Decision

1. **A repository declares its generated files.** `.runsheet/validate.yml`
   takes a `generated:` block beside its suites:

   ```yaml
   generated:
     paths: [platform/loomgen/**, platform/registry_gen.go, gateway/**/*_gen.go]
     generate: ["cd platform && go tool loom generate", "cd platform && go generate ./registry.go"]
     check: ["cd platform && go tool loom generate --check", "go build ./...", "go vet ./..."]
     timeout: 10m
   ```

   `paths` are globs as a suite's are; `generate` and `check` run in
   order, each on `timeout` (10 minutes when left out). Paths without
   generate commands is refused, naming `generated`
   (`validate.Config.Generated`, `Declared`, `Covers`).

2. **A rebase tries the mechanical path before a session.** After the
   rebase's own worktree (ADR-028) and the item's token are made, and
   before the context, the pod, the harness and `Credentials.Open`, a
   repository that declares `generated` gets `regenerateRebase`: the
   node fetches the base, rebases onto `origin/<base>`, and at each
   commit the rebase stops on with every unmerged path a generated one
   takes the base's side of each (the rebase's `--ours`), runs
   `generate`, stages what changed under the generated paths and
   continues. Its commands run through the validation's runner — on the
   host, or in a pod of the org's sandbox; a sandboxed node never runs a
   repository's generators on the host.

3. **Success needs every check to pass.** Once the rebase completes,
   every `check` runs; only when all pass is the head pushed —
   `--force-with-lease=<branch>:origin/<branch>` on the item's token —
   read back from origin, said in one progress note, `rebased <branch>
   onto <base> at <sha> by regenerating <paths> (no session)`, attested
   as any rebased head (ADR-014, After a rebase) and finished with
   `FinishWorkItemRebase(item, head, paths)`, no usage report: no
   account was opened, no session ran, nothing cost a model. The check
   is what catches the silent duplicate a clean auto-merge can leave.

4. **Anything else falls through unchanged.** A conflicted path outside
   the generated ones, a failing generate or check, a rebase with no
   conflict to regenerate, a validate.yml that does not load, a base
   that cannot be fetched or a push that is refused is said in one line
   — `regenerate: <why>; a rebase session instead`, logged and noted —
   the worktree is reset to `origin/<branch>` and cleaned, and the
   rebase session runs exactly as it did before, on the same
   `RebasePrompt`, with origin's branch untouched. A repository with no
   `generated` block, or no validate.yml, says nothing and goes straight
   to the session.

## Consequences

- A generated-only conflict costs minutes of the node's CPU, not a
  session slot and a model's hour; the slot waits only for conflicts
  that need judgement.
- The finish still reads `method: session` on the platform: the
  platform's `FinishWorkItemRebase` has no method for a regeneration
  yet, and telling the two apart there is a platform change of its own.
- The rebase claim, its heartbeat and its 15-minute timer are the
  session's: a regeneration ends long before the timer, so nothing of
  the claim changes.
- `regenerate_e2e_test.go`'s `TestGeneratedConflictIsRegenerated`
  rebases a generated-only conflict with a harness and a credential
  read that fail the test if used; `TestNonGeneratedConflictFallsThrough`
  sees a non-generated conflict and a failing check each start one
  session on today's prompt with origin untouched.
