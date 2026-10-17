---
summary: A node keeps one clone per repo at ~/.mesh/repos/<owner>/<name>, cloned and fetched under the per-repo lock, and makes each new item's ~/.mesh/work/<goal>/<seq> a git worktree of it on its own per-worktree token; evaluation, goal-doc, plan and research checkouts stay clones of their own; sessions install against one pnpm store and one npm cache per node under ~/.mesh/store; RemoveWorkspace removes a worktree and prunes, never the clone; a full clone from before is used until swept.
status: accepted
---

# ADR-028: One clone per repo, one worktree per item

2026-10-10, goal `one-clone-per-repo-and-one-pnpm-store-per-node`
(items 27 to 34). Follows mesh-specs ADR 017,
`adr-017-one-clone-per-repo-and-one-store-per-node`, which decides the
layout; this records how mesh-cli carries it out. Amends ADR-023's
"per-item checkouts are independent clones, never worktrees of a shared
mirror" (`adr-023-fetches-of-one-repo-are-serialised-on-the-node.md`)
and ADR-007's mounts (`adr-007-the-sandbox-on-the-node.md`). ADR-027's
`<seq>-eval` checkout stands as it is.

## Context

On the Mini on 2026-10-10 the two nodes held **917** item checkouts
under `~/.mesh/work`. Every one was a full clone: **1.2 GB** of git
objects between them and **200 GB** of `node_modules`, one copy per
item, none ever shared.

Both came from how Prepare made a checkout: `gh repo clone` for every
new item into `~/.mesh/work/<goal>/<seq>`, so a repo of ten items was
ten clones over the network, each with the whole history; and every
session's install wrote its packages into its own tree, its store and
cache living in a HOME thrown away with the pod. An item needs its own
files, index and HEAD, and its branch checked out in one place — not
its own object store or its own copy of every package. Git worktrees
give the first over one object store; pnpm's content-addressable store
gives the second.

## Decision

### One clone per repo

A node keeps one clone of each repo at `~/.mesh/repos/<owner>/<name>`
(`ReposRoot()`, `Git.ReposRoot`, `SharedClonePath`; empty, it is
`repos` beside the spec's work root). The first item of a repo clones it
— `gh repo clone` on the operator's `gh`, or `git clone` over https on
the item's token when one is issued — and every later item of the repo,
of any goal, fetches it (`git fetch --prune origin`). Both run in
`syncSharedClone` under the **existing per-repo lock** (ADR-023: the
process mutex keyed by `RepoLockKey` and the flock on
`<root>/.locks/<owner>--<name>.lock`), the choice between clone and
fetch made inside the lock, so two first items of a repo clone it once.
A clone that fails is removed before the lock goes. The lock is released
before any worktree is touched.

### One worktree per item

A new item's checkout — a spec with a repo, a branch other than its
base, a seq and no `Path` (`Git.sharedClone`) — is made by
`addWorktree`: the locked sync, `git worktree prune` (a registration
whose directory is gone never blocks the path or branch), then `git
worktree add` on the item's branch — from `origin/<branch>` when an
earlier attempt pushed it, the local branch an interrupted attempt left,
else a new branch from `origin/<base>`. The node log shows `git worktree
add`, not `gh repo clone`, for every item of a repo after its first. The
objects and refs are shared; the files, index and HEAD are the item's
own, and git refuses a branch checked out in two worktrees.

A rework of the same item reuses its worktree: Prepare finds `.git`
there and fetches and checks out in place, keeping a local branch that
is the pushed one or ahead of it (its unpushed commits survive; the push
still fast-forwards). It never adds a second worktree for an item.

### Per-worktree tokens

Each worktree carries its own item token. `Authorize` on a new worktree
item makes the worktree itself (on the token, by environment —
`withGitToken`, never config or argv), and `writeHelper` writes its
credential helper with `git config --worktree` after enabling
`extensions.worktreeConfig` on the shared clone; a full clone keeps
`--local`. The shared clone's own config holds no token, so one item's
token is never left behind for the next, and `ItemCred.ForPush`'s
rewrite of one worktree's helper leaves the other's alone.
The shared clone's own config holds no session's values at all: origin
and `extensions.worktreeConfig`, written once per clone under the
repo's lock after a read, and each branch's tracking; the agent's
identity and hooks go in `config.worktree` as the helper does, the
token on a command's `GIT_CONFIG_*` environment, a stale `config.lock`
is removed after a minute, and a wait on the repo lock names its
holder ([ADR-030, a session never writes the shared clone's
config](adr-030-a-session-never-writes-the-shared-clones-config.md)). The sandbox
mounts the clone's whole `.git` (below; `.git/worktrees` read-only but
the session's own entry, ADR-034), so a session could read a
sibling worktree's `config.worktree`; it is a token for the same repo,
an hour long.

### Evaluation and goal-doc checkouts stay clones

An evaluation's `<goal>/<seq>-eval` (ADR-027, `EvalWorkspaceSpec`,
`Path` set) and the goal doc's `<goal>/0` on the base branch (Seq 0,
`Branch == BaseRef`), like a spec with no branch (`CheckoutBase`,
detached at the base), stay full clones of their own off the shared
clone. Each would otherwise ask git for a branch another worktree holds
— the build's branch in its worktree, the base in the shared clone — and
git refuses that. A clone of its own asks nothing of the shared one.

### Plan and research checkouts stay clones

Plan and research checkouts (`PlanRepoSpec`, `ResearchRepoSpec`, under
`<goal>/plan/repos/` and `ResearchDir`) stay shallow clones, as mesh
ADR 017 §6 decides: read-only, at the default branch, never pushed, one
per plan rather than per item, and none of the installs that make the
disk. A worktree would tie them to the clone's lock, its
`.git/worktrees` and the sweep for no saving.

### The sandbox mounts the clone's git dir

`sandbox.GitCommonDir(workspace)` is a worktree's shared git common dir,
absolute and symlink-resolved, `""` for a full clone; `Session.Mounts`
mounts it read-write at its own path beside the workspace, so the
worktree's `.git` file resolves in the pod and commits land in the
shared object store. Nothing else of `~/.mesh/repos` is mounted. A full
clone's mounts are unchanged; a `GitCommonDir` error is logged and the
full-clone list kept.

**Amended 2026-10-17 by [ADR-034](adr-034-a-session-cannot-prune-the-shared-clones-worktrees.md):**
`<common>/worktrees` is mounted again read-only over the common dir and
the workspace's own entry, `<common>/worktrees/<id>` (`sandbox.GitDir`),
read-write over that — the only writable path under `.git/worktrees`.
Every other item's work tree is absent in the pod, so its entry's
`gitdir` pointer dangles there, and a `git worktree prune` in the pod
deleted live entries on the host, twice on dl360. Pruning stays the
node's, on the host under the repo lock.

### One pnpm store and one npm cache per node

Every session installs against `~/.mesh/store/pnpm` and
`~/.mesh/store/npm` (`StoreRoot()`, `PackageStores`, `sandbox.Stores`):
`npm_config_store_dir`, `pnpm_config_store_dir` (pnpm 11 reads no
`npm_config_` variable) and `PNPM_HOME` name the store, `npm_config_cache`
the cache, in `BaseEnv`; both are mounted read-write at their own paths.
Neither holds a credential.

**The EXDEV choice.** `link(2)` refuses to cross mount points (`EXDEV`)
even on one filesystem, so inside a pod, where the store and the
workspace are separate bind mounts, pnpm falls back from hardlinking to
copying: there the store saves the download, not the disk. Mounting a
parent of both would let pnpm hardlink, but it would hand the session
every other item's worktree, which ADR 017 §2 refuses — so they stay
separate mounts. A session on the host hardlinks from the same store
(measured: a second `node_modules` of one lockfile 172 KB beside the
first's 27 MB under `du -s`, pnpm reporting reused 2, downloaded 0).

### What RemoveWorkspace removes

`Git.RemoveWorkspace(ctx, dir)` is the sweep's remover: a worktree
(its `.git` a file) by `git worktree remove --force` and `git worktree
prune` in its shared clone, under the repo's lock; a full clone by
`os.RemoveAll`; a directory already gone, or half-removed by an
interrupted sweep, removed and pruned from whichever clone under the
repos root still registers it, so its branch prepares again. It never
removes the shared clone — a path under the repos root is refused — nor
the package store or cache.

### Upgrading from per-item clones

A node upgraded from the per-item-clone layout keeps its checkouts: one
under `~/.mesh/work/<goal>/<seq>` whose `.git` is a directory is fetched
and checked out in place for that item's rework and evaluation, as
before, until the sweep removes it whole. No step converts it. Every new
item takes the worktree path.

## Consequences

- A repo of ten items is one clone from GitHub and ten `git worktree
  add`s; the e2e `TestOneCloneTwoWorktreesBuildAndPushE2E`
  (`internal/node/oneclone_e2e_test.go`, `-tags e2e`) shows one `gh repo
  clone`, two `git worktree add`, both branches pushed to the origin and
  the clone left after both are removed.
- The clone is shared state: a clone git cannot use stops every item of
  the repo on the node, released as a workspace failure (ADR-024), not
  failed.
- Whatever a finished worktree held unpushed goes with `--force`, as a
  whole clone's did before.
- In a pod the package store saves downloads, not disk, until a mount
  layout lets pnpm hardlink without exposing other items.
