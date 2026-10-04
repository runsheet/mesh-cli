---
summary: A workspace that cannot be prepared before the harness starts — a fetch, a checkout, the agent's identity, the context written into it — releases the item's claim with `workspace: <error>` as the reason (ReleaseWorkItem, attempts unchanged) instead of failing it, holds the item off that node for ten minutes, and fails it only when the platform refuses the release.
status: accepted
---

# ADR-024: A workspace failure releases the claim

2026-10-04, goal `two-sessions-fetch-one-repo-safely` (item 3).
Numbered 024 beside `adr-024-a-session-commits-as-its-agent.md` and
`adr-024-a-refused-claim-is-a-hold-until-the-goal-changes.md`, which
landed first; cite this one as "ADR-024, a workspace failure releases
the claim".

## Context

On 2026-10-03 the grapevine node (capacity 4) claimed four items of the
handbook repos within seconds. Item 1 of `the-handbook-reads-the-repos`
failed at `workspace: git fetch --quiet origin
+refs/heads/main:refs/remotes/origin/main: exit status 1: error: cannot
lock ref 'refs/remotes/origin/main' …`, and two minutes later item 2
failed at `git fetch --prune origin: … cannot lock ref … (unable to
update local ref)`. In both cases git refused a ref update because
another session of the same repo was writing that ref at the same
moment. `RunItem` answered every workspace error except a refused
credential with `FailWorkItem`, so each failure cost its item an
attempt and a failed status, and the goal was re-planned around one of
them. No builder had run and nothing about the item was wrong.

The goal's rule is that a workspace failure before the harness starts
belongs to the node, not the item. The TRD (§3 The node protocol)
already releases an item when its credential fails
(`ReleaseOnCredential`, adr-001-item-scoped-github-token) or a sandbox
limit kills it. The platform half is runsheet goal
`a-refusal-before-the-builder-runs-is-not-an-attempt`, where the
platform itself will refuse to count such a failure as an attempt.
Until that lands, the node has to avoid calling it one.

## Decision

- In `RunItem`, a non-credential `PrepareWithRetry` error, an
  `Identify` error and a `WriteContext` error each go through
  `ReleaseOnWorkspace(ctx, gw, itemID, nodeID, label, cause, logf)`
  (`internal/node/workspace_release.go`). This calls
  `ReleaseWorkItem(itemID, nodeID, "workspace: <error>")`, with the
  error scrubbed of tokens and truncated to 2000 characters. The item
  goes back to `ready` with no node and its attempts unchanged, and the
  reason becomes its `lastNote`. The node logs one line: `item
  <goal-slug>/<seq>: workspace failed, released for a retry: <error>`.
- After the release, this node holds the item off for
  `WorkspaceReleaseHold` (10 minutes). The hold uses the loop's
  `Declines` (`Declines.HoldFor`, kept whatever the runtimes), which the
  claim path already consults, so an item whose workspace always fails
  here does not spin between claim and release. It is logged once:
  `item <label>: held off this node until <time> after a workspace
  release`.
- The item is failed only when the platform refuses the release, since
  otherwise it would stay held by a node that has stopped working on it.
  In that case `FailWorkItem` is called with the same reason, logged
  `item <label>: workspace failed and the release was refused (<why>);
  failed instead: <reason>`. A cancelled context (the node's own
  shutdown) releases and fails nothing and returns `ctx.Err()`.

## Consequences

- A collision like grapevine's no longer costs an attempt or re-plans
  a goal. The item is claimed again, by another node at once or by
  this node after ten minutes.
- The trade-off: an item whose workspace is broken on every node (a
  base ref that does not exist, a repo the node cannot clone) is never
  failed. It keeps cycling between nodes, each holding it off for ten
  minutes, and only its `lastNote` says why. This lasts until the
  platform half, `a-refusal-before-the-builder-runs-is-not-an-attempt`,
  lands and decides on the platform when a refusal before the builder
  becomes the item's failure.
- A release the platform refuses still fails the item, so a platform
  without the release behaves as it did before this ADR.
