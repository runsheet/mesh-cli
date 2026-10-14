---
title: "ADR-033: A gone base branch is read as merged"
summary: An item's base other than the repo's default branch that origin no longer has — `git ls-remote --exit-code` answering 2, so a credential or network refusal stays its own error — is a dependency's branch GitHub deleted when it merged; the node then takes validation's changed paths, the evaluation's diff and the retry turn's changed paths from origin/<default>, continues an already-pushed branch's checkout without the base, and logs `base <dep> is gone (merged)`; until it ships, such a failed item is recovered by hand with retryWorkItem {by, reason} and, if the goal reads blocked, resumeGoal.
status: accepted
---

# ADR-033: A gone base branch is read as merged

2026-10-14, goal `a-merged-dependencys-gone-branch-fails-no-validation`
(items 1–3). Builds on [ADR-014](adr-014-a-build-validates-before-it-pushes.md)
(a build validates before it pushes), [ADR-030](adr-030-an-environment-failure-names-its-kind.md)
(an environment failure names its kind), [ADR-027](adr-027-an-evaluation-has-its-own-checkout.md)
(an evaluation's own checkout) and [ADR-028](adr-028-one-clone-per-repo-one-worktree-per-item.md)
(one clone per repo, one worktree per item).

## Context

An item that depends on another starts from the dependency's branch: its
`baseRef` is `mesh/<goal>-<seq>` of the item it waits on. The platform
asks the base again when it schedules (runsheet
`platform/processes/schedulegoal.go`: a merged dependency's branch is
gone, so the item starts from the default branch) — but only at
scheduling. When the dependency merges *while the item builds*, GitHub
deletes its branch, and everything the node does afterwards against the
base fails:

- validation's changed paths fetch `+refs/heads/<dep>` and fail
  (`validation: changed paths: … couldn't find remote ref`), so the item
  is failed with no attestation, though its code is fine;
- the evaluation's `<seq>-eval` checkout (`Git.Prepare`'s base fetch) and
  its diff fail the same way, so the item is never judged, or judged
  `could not diff`;
- the retry turn's changed paths come back empty.

The item then sits failed and its goal blocked, by an event that is the
ordinary outcome of the work going well.

## Decision

**A gone base is read as merged.** When the base's fetch fails, the node
asks `git ls-remote --exit-code origin refs/heads/<base>`. Only its exit
2 — origin answered, and has no such ref — makes the failure a
`*BaseGoneError` (`Git.EnsureMergeBase`, `Git.baseGone`). Any other
answer keeps the fetch's own error: a fetch refused for its credential
is still `credential_refused` (ADR-030) and a network failure still an
error, never mistaken for a merge. A base that *is* the default branch
takes no fallback.

**Diff, validate and evaluate against the default branch.** On a
`*BaseGoneError` the node uses `origin/<default>` (the repo's default
branch, `main` when unread), which by then holds the dependency:

- validation's changed paths (`validator.changedPaths`, the target's
  `defaultBranch`), said once per build in node.log and on the item:
  `item <label>: validation: base <dep> is gone (merged); changed paths
  from origin/<default>`;
- the evaluation's diff, changed and protected paths and the prompt's
  `BaseRef` (`goneBaseFallback`): `item <label>: evaluate: base <dep> is
  gone (merged); diff from origin/<default>`;
- the retry turn's changed paths (`Runner.retryChangedPaths`): `item
  <label>: retry turn: base <dep> is gone (merged); changed paths from
  origin/<default>`;
- `Git.Prepare` of a branch already on origin — the evaluation's own
  checkout, a rework — continues `origin/<branch>` without the base:
  `workspace <dir>: base <dep> is gone (merged); continuing
  origin/<branch>`. A new branch still needs its base and fails as before.

A squash-merged dependency's commits are not the item's ancestors, so
against the default branch its files count as changed too. That selects a
superset of the suites, which is safe: the attestation names HEAD, the
commit the platform verifies, whatever selected its steps.

**Recovery by hand until this ships.** A node without it fails such an
item at `validation: changed paths: …`. Recover it with `retryWorkItem
{by, reason}` on the failed item (refused if a narrow re-plan has already
replaced it), then `resumeGoal {namespace, aggregateId}` if the goal reads
`blocked`. The retry is claimed fresh and the platform asks the base
again, starting it from the default branch.

## Consequences

- An item whose dependency merges mid-build attests on main and lands; its
  node.log shows the fallback lines instead of a failure.
- A credential or network refusal of the base fetch is unchanged: the
  ls-remote check keeps it apart, so it is still released or failed with
  its kind, never read as a merge.
- A squash-merged dependency's suites run again on the item's head: a
  little more validation, never less.
- Rebase and main validation take no fallback: a rebase is onto the
  default branch already, and a main validation diffs a merge commit
  against its first parent.
- No platform contract changes; the node reports nothing new beyond the
  progress note.

## Proof

`internal/node/basegone_test.go` (EnsureMergeBase's `*BaseGoneError`, the
validator's fallback said once, an unreachable origin and a credential
refusal not read as gone), `evaluate_basegone_test.go` (the evaluation's
diff and the retry turn's paths), and
`internal/node/basegone_e2e_test.go`'s
`TestBaseGoneMergedDependencyValidatesOnMainE2E` (`-tags e2e`): item 2
claimed on item 1's branch, item 1 squash-merged and its branch deleted
on origin between the build's commit and validation; the node attests
item 2's head, finishes it, and the evaluator diffs it against
`origin/main` and passes, node.log carrying the fallback lines and no
`changed paths:` failure.
