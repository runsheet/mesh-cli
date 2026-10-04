---
summary: Every `gh repo clone` and `git fetch` the node's `Git` runs for one repo holds a per-repo lock — a process-wide mutex keyed by owner/name and an exclusive flock on `<root>/.locks/<owner>--<name>.lock` — for the fetch alone, never a checkout, a session or a push; each item keeps its own independent clone at `~/.mesh/work/<goal>/<seq>`, no shared mirror and no worktrees, because ADR-007's sandbox mounts the workspace alone.
status: accepted
---

# ADR-023: Fetches of one repo are serialised on the node

2026-10-04, goal `two-sessions-fetch-one-repo-safely` (item 1). The
number is shared with the other ADR-023s landed the same week; the
slug tells them apart. Item 2 of the goal adds the retry of a fetch
that still fails on a ref lock, item 3 the release of the claim.

## Context

On 2026-10-03 the grapevine node (capacity 4) claimed four items of the
handbook repos within seconds. Its log:

```
workspace: git fetch --quiet origin +refs/heads/main:refs/remotes/origin/main: exit status 1: error: cannot lock ref 'refs/remotes/origin/main': is at d843d265…
```

for item 1 of `the-handbook-reads-the-repos`, and two minutes later, for
item 2:

```
git fetch --prune origin: … error: cannot lock ref 'refs/remotes/origin/main': is at c67b5444… but expected d843d265… (unable to update local ref)
```

Both are git refusing a ref update because another git process was
writing the same ref at that moment. Each cost its item an attempt and
a failed status, and the goal re-planned around one of them.

On main the node prepares an item by `gh repo clone` into
`~/.mesh/work/<goal>/<seq>` (`Git.Prepare`): every item is its own
clone, and two items' fetches write two different
`refs/remotes/origin/main`. The race is two git processes in *one*
checkout — an item prepared again while its evaluation, rebase or
`PushedHead` read fetches in the same directory, or a retried prepare
overlapping the last one. A test that runs four prepares of one spec at
once, with origin's main moved, reproduces the first error line on
every run without a lock.

## Decision

`Git` takes a **per-repo lock** around every command that writes refs
from the remote (`internal/node/repolock.go`):

- **The key** is `RepoLockKey(repo)`: owner/name, lower-cased, however
  the repo is spelt — `owner/name`, an https URL, an ssh URL — so a
  `Prepare` keyed by `spec.Repo` and a later `PushedHead` keyed by the
  checkout's `git remote get-url origin` share one lock. Any other
  remote, a path as the tests pass, keys as itself.
- **The lock** is two halves taken in order: a `sync.Mutex` per key,
  process-wide (the runner, evaluator, rebaser, planner and director
  each hold their own `Git`), then an exclusive advisory `flock`
  (`LOCK_EX`, polled non-blocking so a cancelled context gives up) on
  `<root>/.locks/<owner>--<name>.lock`, so two node processes on one
  machine — an upgrade's old and new, or a node and `mesh mcp` — are
  serialised too. The root is `Git.LockRoot` when set, else `Prepare`'s
  `WorkspaceSpec.Root`, else the checkout's grandparent: the work root
  `~/.mesh/work` for an item's `~/.mesh/work/<goal>/<seq>`.
- **What it covers**: the `gh repo clone` and `git fetch --prune origin`
  in `Prepare`, and the fetches of `CheckoutBase`, `baseHistory`,
  `EnsureMergeBase` (its deepening and unshallowing included), `Fetch`
  (its clone too) and `PushedHead`. All go through one helper,
  `Git.fetchLocked`, which takes the lock, runs the one command and
  lets it go — the place item 2's retry lives.
- **What it does not cover**: `git checkout`, `reset`, `merge-base`,
  `rev-parse`, a session, a commit, and `Push` — the lock is released
  before any of them. A session runs for an hour; a lock across it
  would serialise builds, not fetches. A push writes the remote's ref,
  not the checkout's remote-tracking one.

## Layout

Each item's checkout is an **independent clone** at
`~/.mesh/work/<goal>/<seq>`, with its own object store and its own
refs: no shared mirror of the repo on the node, and no `git worktree`.
Two items of one repo never write the same remote-tracking ref, so the
lock serialises only fetches that would collide within one checkout and
fetches across checkouts, which cost seconds.

Worktrees off a shared mirror were considered and are not used: a
worktree's `.git` is a file pointing at a gitdir inside the mirror,
outside the checkout, and ADR-007's sandbox (mesh-specs ADR 007,
Mounts) mounts a session's pod exactly its workspace and a temporary
HOME — never another path of the host. A worktree's git would find its
gitdir missing inside the pod, and mounting the mirror would hand every
session every other item's objects and refs. The clones cost disk and a
full fetch each; the goal accepts that (its acceptance (1): "clones that
never write the mirror's refs").

## Consequences

- Four items of one repo prepared at once (`TestPrepareFourItemsOfOneRepoAtOnce`)
  and four prepares of one spec (`TestPrepareOneSpecFourTimesAtOnce`)
  see no `cannot lock ref`; `TestPrepareReleasesTheRepoLock` proves the
  file is left unlocked after `Prepare` returns.
- A slow fetch of a repo holds up another fetch of the same repo, never
  a session or another repo's fetch.
- The lock files stay under `<root>/.locks/`, one per repo, empty; they
  are never removed (removing one while another process waits on it
  would split the lock).
- Advisory locks bind only processes that take them: an operator's own
  `git fetch` in a workspace is not serialised.
- A fetch that still fails on a ref lock — another git outside the
  node — is item 2's retry to absorb.
