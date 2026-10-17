---
title: "ADR-030: A session never writes the shared clone's config"
summary: Two sessions of one repo on a node share one clone's .git/config, so a session writes nothing there — its identity, hooks and credential helper go in its worktree's own config.worktree, its item token rides GIT_CONFIG_* in the command's environment, origin and extensions.worktreeConfig are written once per clone under the repo lock after a read, a git lock older than a minute is removed with a log line and the write retried once, a config.lock older than five minutes no git process holds is removed before the write, and a wait on the repo lock names the item holding it.
status: accepted
---

# ADR-030: A session never writes the shared clone's config

2026-10-13, goal `a-shared-clone-is-never-written-by-two-sessions-at-once`
(items 1 to 4). Amends [ADR-028](adr-028-one-clone-per-repo-one-worktree-per-item.md)
(what the shared clone's config holds) and builds on ADR-023's per-repo
lock (`adr-023-fetches-of-one-repo-are-serialised-on-the-node.md`) and
ADR-024's workspace-failure release. Follows mesh-specs ADR 017
(`adr-017-one-clone-per-repo-and-one-store-per-node`), whose "the
clone's remote URL carries no token … passed per command" it carries
through to every config write. The number repeats
[ADR-030, an environment failure names its kind](adr-030-an-environment-failure-names-its-kind.md);
the two are separate decisions.

## Context

Since one clone per repo landed (ADR-028, 2026-10-10) every item's
checkout is a worktree of `~/.mesh/repos/<owner>/<name>`, and a
worktree's `--local` config *is* the shared clone's `.git/config`. Each
session still wrote to it on its way in: `Authorize` and
`syncSharedClone` ran `git remote set-url origin` on every tokened
sync, `addWorktree` set `extensions.worktreeConfig` on every add, and
`Identify` ran `git config --local user.name`, `user.email` and
`core.hooksPath` — unlocked, and with `core.hooksPath` the last writer's
for every item of the repo.

Git takes `.git/config.lock` for each of those writes and refuses a
second writer outright. On 2026-10-12/13 two sessions of one repo
starting together on the dl360 nodes failed on `could not lock config
file …/.git/config: File exists` again and again; the item was released
or failed for a file the node, not its code, had written. Five goals —
`a-reward-is-earned-then-claimed`,
`an-owner-has-a-board-of-their-own-businesses`,
`latchkey-mints-a-session-id-token`, `applying-follows-the-business` and
others — burned 9 to 12 narrow re-plans each on it, and four were
blocked. A process killed mid-write also left a `config.lock` behind
that failed every later session of the repo until someone removed it by
hand, and a session waiting on the per-repo lock said nothing of who
held it.

## Decision

### 1. A session's own values go in its worktree's config

`Git.Identify` writes `user.name`, `user.email` and `core.hooksPath`
with `git config --worktree` when the checkout is a worktree
(`isWorktree`), into `.git/worktrees/<id>/config.worktree`, and with
`--local` only for a full clone (an evaluation's, the goal doc's, a
plan's, or one from before the layout). The credential helper already
went there (`writeHelper`, ADR-028). Each worktree reads its own agent
and hooks; the shared config holds none of them, so two sessions never
contend for its lock and no item's hooks run for another. The rebase
session calls the same `Identify`.

### 2. The item token rides the command's environment

Every git command the node runs on an item's token — the shared
clone's `git clone` and `git fetch`, `git worktree add`, the
validator's changed-paths and newer-head fetches, a build's push —
carries the token as `GIT_CONFIG_COUNT`,
`GIT_CONFIG_KEY_n`, `GIT_CONFIG_VALUE_n` (an empty
`credential.<github>.helper` first, then the token's) in its own
environment (`withGitToken`, `gitTokenEnv`, `OnItemToken`). Nothing is
written to a file for it, so concurrent commands with different tokens
never meet, and the token is gone when the command exits.

### 3. Origin and worktreeConfig are written once per clone, under the lock

The shared clone's `remote.origin.url` and `extensions.worktreeConfig`
are read first (`git remote get-url origin`, `git config --get`), which
takes no lock and is all the steady state needs. Only a value that
differs is written, and then under the repo's lock and read again inside
it (`sharedOrigin`, `syncSharedClone`, `addWorktree`), so one writer
sets it and every later session sees it set. A branch's tracking
(`branch.<b>.*`) is written by `push -u` or `checkout --track` only when
the branch does not track origin already; a new branch is made
`--no-track` from the base.

### 4. A stale lock is removed, not held

A git config write that fails on a lock (`IsGitLockFailure`,
`GitLockPath` reading git's message) calls `ClearStaleGitLock`: a lock
file older than `StaleGitLockAge`, one minute, is removed and logged
`removed stale git lock <path> (age <d>)`, and the write is retried
once (`withStaleLockRetry`; `gitConfigWrite` for Identify). A younger
lock is another writer's: it is left alone and the error stands, a
workspace failure released as ADR-024 says, never an item failure.

**Amended 2026-10-17** (goal `a-workspace-whose-worktree-is-gone-is-rebuilt`
item 1): `withStaleLockRetry` first calls `preclearStaleLock`, so a
repository's `config.lock` is cleared before the write, not only after
one fails on it. The lock is the clone's (`commonDirConfigLock`, `git
rev-parse --git-common-dir`); one older than `PreclearGitLockAge`, five
minutes, that no git process holds (`gitLockHolder`, which scans `/proc`
for a `git` whose cwd or `GIT_DIR` is under the worktree or the lock's
directory) is removed with the same `removed stale git lock` line
(`TestStaleConfigLockIsCleared`).

### 5. A wait on the repo lock names its holder

`lockRepo` records the holder in the lock file after the flock —
`label=<goal>/<seq> role=<r> pid=<n> since=<RFC3339>` — and in-process
beside the mutex (`repoHolders`). Sessions mark their ctx with
`WithLockHolder` (the Runner's `RunItem`/`AdoptItem`, the Evaluator's,
the Rebaser's). A call that has to wait logs `repo lock <key>: waiting
on <label> (pid <n>, held <d>)` and then `repo lock <key>: acquired
after <d>` (`SetRepoLockLogf`, node.log under `mesh node start`); a free
lock logs nothing.

## Alternatives

- **A credential helper file per worktree** (a script or store file the
  worktree's helper points at). It moves the token out of config but
  not the helper's name: something still has to say which file, which
  is a config write, and a file on disk outlives the command and is one
  more secret to clean up. The environment carries the token for
  exactly one command and writes nothing.
- **`includeIf "gitdir:…"` per worktree** in the shared config. Each
  new worktree would add its own include section to the one shared
  `.git/config` — the very write that contends for the lock — and
  removing a worktree would have to remove it again. `config.worktree`
  is git's own per-worktree file, read only by that worktree, created
  without touching the shared one once `extensions.worktreeConfig` is
  on.
- **`git -c key=value` on every command.** It puts the token in argv,
  visible in `ps` and in the node's `$ git …` log lines, and it covers
  only the commands the node runs: the session's own `git commit` and
  hooks would not see an identity passed that way. `GIT_CONFIG_*` is
  the same mechanism by the environment, and the identity and hooks the
  session's commands need live in `config.worktree`.

## Consequences

- Two sessions of one repo started within a second of each other both
  reach their first command. `TestConcurrentSessionsOnOneRepoE2E`
  (`internal/node/concurrent_sessions_e2e_test.go`, `-tags e2e`) runs
  twenty pairs released by one barrier on a warm shared clone and twenty
  where neither has cloned: `Authorize` on an item token, `Prepare`,
  `Identify`, then `git status` and a commit as the agent; every session
  succeeds, no git output or node log line says `could not lock config
  file`, and the warm clone's `.git/config` is byte for byte what it was
  after round one. Run on the code before item 1 it fails at round one,
  `core.hooksPath` in the shared config.
- The shared config is written at a clone's first use — clone, origin,
  `extensions.worktreeConfig` — and by a branch's first tracking write;
  nothing else. That first tracking write (`push -u` or `checkout
  --track`) is still outside the repo lock, once per branch; a
  collision there falls to the stale-lock retry or the workspace
  release.
- `ensureWorktreeConfig`, the fallback that enables
  `extensions.worktreeConfig` from `Identify` or `writeHelper` on a
  clone `addWorktree` did not set up, writes unlocked; it runs only on a
  clone made before the layout.
- A rebase session pushes from inside its own session on the
  worktree's helper (`config.worktree`), not on `OnItemToken`; it
  writes nothing shared either way.
- Only the Runner's `Git` sets `Logf` today, so a stale lock removed on
  the evaluator's, rebaser's or planner's `Git` is removed silently.
- A session's identity, hooks and helper are now in a file inside the
  shared clone's `.git/worktrees/<id>/`, which the sandbox mounts with
  the rest of the clone's git dir (ADR-028): a session could read a
  sibling's `config.worktree`, as it could its helper before.
- The goal's acceptance also asks for a day of dl360 node.log with no
  `could not lock config file`; that is read on the nodes after the
  release, not here. The goal's note asks for the choice in mesh-specs
  too; this ADR is mesh-cli's, and mesh-specs ADR 017 is unchanged.
