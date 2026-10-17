---
summary: A node keeps one clone per repo at ~/.mesh/repos/<owner>/<name>, cloned and fetched under the per-repo lock, and makes each new item's ~/.mesh/work/<goal>/<seq> a git worktree of it on its own per-worktree token; evaluation, goal-doc, plan and research checkouts stay clones of their own; sessions install against one pnpm store and one npm cache per node under ~/.mesh/store; every worktree is locked while its item is live and nothing runs a blanket `git worktree prune`, RemoveWorkspace refusing a live dir and removing only a finished worktree and its own registration, never the clone; a worktree whose .git/worktrees/<n> is gone is rebuilt for its branch at its recorded head and the step retried, never failed or released; a full clone from before is used until swept.
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
`addWorktree`: the locked sync, then — under the lock again —
`forgetStaleRegistrations` (a registration whose directory is gone and
that names this path, or holds the branch and is neither locked nor
live, is forgotten alone, so it never blocks the add), then `git
worktree add` on the item's branch — from `origin/<branch>` when an
earlier attempt pushed it, the local branch an interrupted attempt left,
else a new branch from `origin/<base>`. The node log shows `git worktree
add`, not `gh repo clone`, for every item of a repo after its first. The
objects and refs are shared; the files, index and HEAD are the item's
own, and git refuses a branch checked out in two worktrees. The
worktree added is locked, `git worktree lock --reason 'mesh: item
<goal>/<seq> live'` (`lockWorktree`), and Prepare and Authorize lock one
they reuse; the shared clone carries `gc.worktreePruneExpire=never`, set
once under the repo lock after a read, so neither a prune nor a `git gc`
— on the host or in a pod that sees no other item's worktree — drops a
live item's registration (below, What RemoveWorkspace removes).

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
is removed after a minute — and a `config.lock` older than five
minutes that no git process holds (`PreclearGitLockAge`,
`gitLockHolder` scanning `/proc` for a git whose cwd or `GIT_DIR` is
under the worktree or the clone's git dir) is removed *before* the
write, every `git remote set-url` going through `withStaleLockRetry`,
which calls `preclearStaleLock` first, the lock found through `git
rev-parse --git-common-dir` so a worktree's is its clone's — and a wait
on the repo lock names its holder ([ADR-030, a session never writes the shared clone's
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

**Amended 2026-10-17, goal `a-workspace-whose-worktree-is-gone-is-rebuilt`
item 2:** the node runs no blanket `git worktree prune` anywhere. A
prune drops every registration whose directory it cannot see, and on
dl360 on 2026-10-16 admin dirs went from under live and resumed
sessions — `worktrees/313` during a `git remote set-url`, `worktrees/14`
during validation's `rev-parse`.

`Git.RemoveWorkspace(ctx, dir)` is the sweep's remover. It refuses a dir
held live on this node — `HoldWorkspace`/`WorkspaceLive`
(`internal/node/liveworkspaces.go`), held by the Runner, the Evaluator
and the Rebaser while they work an item: claimed, running, evaluating or
rebasing here — and a path under the repos root. A worktree (its `.git`
a file) is unlocked, removed by `git worktree remove --force` and its
own `.git/worktrees/<n>` removed if git left it, in its shared clone
under the repo's lock (`removeWorktree`) — no other registration is
touched. A full clone goes by `os.RemoveAll`; a directory already gone,
or half-removed by an interrupted sweep, is removed and its own
registration forgotten by whichever clone under the repos root still
names it (`registrationsNaming`, `forgetRegistrations`), so its branch
prepares again. The quartermaster's dead-checkout check
(`pruneGoneWorktrees`) forgets only registrations whose directory is
gone and that are neither locked nor live. Every live worktree is
locked (above), so a bare prune or `git gc` a session runs in its pod
leaves it too (`TestWorkspaceCleanupNeverRemovesALiveWorktree`; ADR-034
for the pod's mounts). It never removes the shared clone nor the
package store or cache.

### Rebuilding a worktree whose admin dir is gone

Amended 2026-10-17, goal `a-workspace-whose-worktree-is-gone-is-rebuilt`
item 3. A worktree whose `.git` file names a `.git/worktrees/<n>` that no
longer exists, while its shared clone does (`worktreeAdminGone`), fails
every git command with `fatal: not a git repository:
…/.git/worktrees/<n>` — before this, the item's failure, and on
grapevine eight narrow re-plans of an item whose code was fine.

`Git.RebuildWorktree(ctx, spec)` recreates it under the repo's lock:

1. the head is the branch's as recorded — the shared clone's
   `refs/heads/<branch>`, which outlives the admin dir, else
   `origin/<branch>`, else `spec.Head`; none is an error;
2. `git worktree add --no-checkout` of that branch at a temporary path
   whose base name is the lost admin dir's, so git registers it under the
   same `.git/worktrees/<n>` and the `.git` file reads as it did;
3. that `.git` file is moved over the stale one, `git worktree repair`
   points the admin dir at the workspace, and a mixed `git reset`
   rebuilds the index — the working tree's files, committed or not, are
   left as they were;
4. the worktree is locked as `addWorktree` locks one.

It logs `workspace <dir>: worktree admin <path> gone; rebuilt for
<branch> at <sha>`. A checkout that is no worktree of the shared clone —
an evaluation's `<seq>-eval`, a plan's — is removed whole, for the
Prepare after it to clone again.

`Prepare` and `Authorize` check before any git command, so the
`git remote set-url` of an Authorize runs on the rebuilt worktree.
`PrepareWithRetry` and a build's validation run (`validator.run`, its
changed paths, its head and the attestation's `git rev-parse
<sha>^{tree}`) go through `rebuildAndRetry`: an error matching `not a git
repository: .*/\.git/worktrees/` (`IsWorktreeAdminGone`) is one rebuild
and one retry of the step. The worktree's `config.worktree` went with
its admin dir, so the retry first writes the item token's helper again
(`ItemCred.Reauthorize`); the agent's identity and hooks come back with
the next `Identify`. Nothing is sent to `FailWorkItem` or
`ReleaseOnWorkspace` (ADR-024) for it; a rebuild that itself fails is
the step's error as before (`TestWorkspaceWithMissingWorktreeAdminIsRebuilt`).

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
- A sandboxed session's pod mounts its worktree's admin dir by path; a
  rebuild keeps the path but not the directory, so a pod running across
  a rebuild sees the old one gone until its next session. The rebuild
  restores the host's view, and the build's steps after it.
- In a pod the package store saves downloads, not disk, until a mount
  layout lets pnpm hardlink without exposing other items.

## Amendment 2026-10-17: validations of pushed commits take no worktree

Goal `a-node-validates-from-a-served-diff-with-a-shared-cache`,
[ADR-034](adr-034-a-node-validates-from-a-served-diff-with-a-shared-cache.md).
A main validation — a merge batch candidate (ADR-032) or a merged item's
merge commit — is no longer a detached worktree of the shared clone at
`<work root>/merge-batches/<id>-<attempt>` or
`<work root>/main-validations/<item>-<attempt>`. The node reads the
commit and the compare from GitHub's Git Data API on the run's minted
token, materialises the merge base from its one shared tree cache
(`<user cache dir>/mesh/cache`, filled from this ADR's shared clone when
it holds an object, else from GitHub) into a scratch dir of its own
under `<work root>/.scratch`, applies the diff there and removes the
scratch on every path: no fetch into the clone, no repo lock, no
`.git/worktrees` entry. The worktree stays only as the fallback, when
the served diff cannot give the commit (a diff past GitHub's cap, a
truncated tree, no minted token, a tree that does not match), said once
per run per reason. Item worktrees, their reworks and the evaluation's
own `<seq>-eval` checkout are as above. mesh-specs ADR 017's §2 ("the
locked fetch … before a new worktree, a rework or an evaluation") needs
the same narrowing on its side; that is recorded as a note for
runsheet/mesh-specs, not edited here.

## Amendment 2026-10-17: one clone per repo per org

On a node serving two or more orgs, "one clone per repo" becomes one
clone per repo **per org**: each org's clones live under its own
`~/.mesh/orgs/<org>/repos`, its worktrees under `~/.mesh/orgs/<org>/work`,
and a worktree of one org is never added from another org's clone
(`docs/adr/adr-034-each-org-has-its-own-roots.md`). A node serving one org
keeps the paths above unchanged.
