---
title: "ADR-036: A host session's scratch dies with the session"
summary: An unsandboxed session or validation never writes into the node process's /tmp — each gets a scratch dir of its own under the scratch root `<work root>/.scratch` (`<kind>-<subject>-<rand>`, `validate-<item|batch>-<rand>`) as its TMPDIR and GOTMPDIR, every one shares the node's Go build cache `<user cache dir>/mesh/cache/go` as GOCACHE, held with the cache root under node.yaml's `validation_cache_max_bytes` (default 20 GiB, trimmed hourly below 80% of it), and the scratch is removed when the session or validation ends, however it ends, by the node that ends it; a start-up sweep removes what a killed node left — every dir under the scratch root and the node's own `mesh-session-`/`mesh-gh-`/`mesh-claude-`/`mesh-node-*`/(host only) `mesh-home-` dirs under /tmp — keeping what an adopted session's state names, a path held live, the process's HOME, another user's dir and symlinks; `mesh node status` prints `cache: <size> of <cap> (<root>); scratch <size> (<root>); measured <t>`, and `scripts/host-tmp-reading.sh` takes the before/after reading.
status: accepted
---

# ADR-036: A host session's scratch dies with the session

Goal `a-host-sessions-scratch-dies-with-the-session`. Amends
[ADR-007](adr-007-the-sandbox-on-the-node.md) (the start-up sweep) and
[ADR-009](adr-009-sessions-outlive-the-node.md) (a session's temp dirs),
beside [ADR-034](adr-034-a-node-validates-from-a-served-diff-with-a-shared-cache.md)'s
shared cache and [ADR-034](adr-034-the-quartermaster-keeps-the-node-fit.md)'s
quartermaster.

## Context

On the dl360, 2026-10-15 09:00–10:20 local, `du -xsh /tmp/*` by prefix
read runsheet-* 67G, latchkey-* 42G, mesh-home-* 18G, go-build* 13G and
mesh-eval* 13G, and both node logs said `no space left on device` from
09:38 to 09:45 (items latchkey-signing-keys/42 and a-retry-is-judged/6,
whose builders diagnosed it). A sandboxed session's pod has a TMPDIR of
its own that goes with the pod (ADR-007). An unsandboxed one did not:
a host build, evaluation, plan, rebase or research session, and every
host validation step, inherited the node process's TMPDIR. So what the
agent's `go test`, `t.TempDir`, `mktemp` and go-build left — and a
project's own `runsheet-*`/`latchkey-*` prefixes — stayed in /tmp. The
Go build cache was rebuilt into it each time and then dropped. A node
killed mid-session never ran the session's end, so that session's dirs
stayed too.

## Decision

### The scratch root

The node's scratch root is `<work root>/.scratch` (`ScratchRoot`,
`ScratchDirName`; `~/.mesh/work/.scratch` by default). It is on the work
root's filesystem, where the node's disk floor (ADR-035) already looks,
and it is a dot directory like `.claims` and `.locks`, so `FindSessionsIn`
and the checkout walks skip it. Everything a host session or validation
writes as temporary goes there, never to `os.TempDir()`.

### Per-session and per-validation TMPDIR and GOTMPDIR

- **A host validation** (item 1, `internal/node/scratch.go`,
  `validation.go`): each unsandboxed run — a build's validation, its
  retry, a rebase's re-attest, a merge batch candidate, a main
  validation — gets `MakeScratch(root, "validate", <item|batch>)`,
  `<root>/validate-<subject>-<rand>`. `hostValidationEnv` sets it once
  as TMPDIR and GOTMPDIR, and GOCACHE once as the shared cache
  (`scratchEnv` replaces an inherited value rather than appending a
  second one). `buildValidation.close` removes it with `RemoveScratch`,
  and every caller defers it. A served-diff validation's scratch
  (ADR-034) is a dir under the same root.
- **A host agent session** (item 8, `sessionScratch`): a build,
  evaluation, plan, rebase or research session gets
  `<root>/<kind>-<subject>-<rand>` under its org's scratch root
  (`scratchRootFor`) as TMPDIR and GOTMPDIR, with GOCACHE the shared
  cache. `sessionTempDirs` carries the TMPDIR into
  `SessionMeta.TempDirs`, so `state.json` names it. A sandboxed session
  gets none, because its pod has its own.
- **The harness** (item 7): a no-workspace Claude Code session's
  `mesh-session-*` dir goes under the job's own TMPDIR, else
  `ClaudeCode.ScratchRoot` (the node's scratch root), never the process's
  /tmp. A Codex session's `CODEX_HOME` is removed on every end: ended,
  failed or cancelled.

No mesh-cli code makes `mesh-eval*`. That name comes from an agent's
own `mktemp`, which the session's TMPDIR now covers.

### The shared GOCACHE and its cap

Every host session and validation shares one Go build cache,
`BuildCacheDir()` = `<user cache dir>/mesh/cache/go` (`$XDG_CACHE_HOME`
or `~/.cache` on Linux). It sits under the shared cache root
`SharedCacheRoot()`, so it is reused across runs instead of being
rebuilt into a scratch that is then thrown away. The cache root, the Go
cache included, is held under node.yaml's **`validation_cache_max_bytes`**
(`Config.ValidationCacheCap`, default 20 GiB,
`DefaultValidationCacheMaxBytes`). No new key was added; the served-diff
cache already used this cap. A served-diff validation's `Enforce` holds
the cap after every materialisation (ADR-034). The node also runs
`CacheTrim` (item 4, `internal/node/cachetrim.go`) from the tick, after
the quartermaster: at the first tick, then at most once an hour
(`DefaultCacheTrimEvery`). It measures the root and, when the root is
over the cap, trims it below `TrimFraction` (0.8) of the cap by
`SharedCache.Trim`: least recently used entries first, then `go clean
-cache`. Each trim is logged `cache: trimmed <before> to <after>`.

### Removal at session end

A scratch dir goes when its session or validation ends, by the node
that ends it. A session handled in-process goes on its end. A detached
`Outlive` session goes when the adopting node has handled its end
(`harness.RemoveSession` over `state.json`'s `TempDirs`). A session the
node gives up on (claim lost, starved) goes on its stop.
`RemoveScratch` and the harness's `removeTree` remove a tree that holds
read-only dirs, such as a Go module cache: a failed `RemoveAll` is
followed by `chmod u+w` on every dir and a retry. Once nothing runs, the
scratch root holds nothing.

### The start-up sweep and its keep rules

At `mesh node start`, after `FindSessions` and `SweepSandbox` and before
adoption, `SweepScratch` runs `ScratchSweep` (item 3,
`internal/node/scratchsweep.go`) on every node, sandboxed or not. It
removes:

- every dir directly under the scratch root, and
- every dir directly under `os.TempDir()` named with one of
  `HostScratchPrefixes` — `mesh-session-`, `mesh-gh-`, `mesh-claude-`,
  `mesh-node-pull-`, `mesh-node-image-`, `mesh-node-push-` — plus
  `mesh-home-` on a host node only. On a sandboxed node, `mesh-home-*`
  is left to the sandbox's sweep, which knows which HOME a running
  container mounts.

It **keeps**:

- a dir that a found session (one about to be adopted) names, or one
  that holds such a path: its session dir, workspace, HOME, every
  `TempDirs` entry, and its token file's dir;
- a path held live in this process (`WorkspaceLive`);
- the process's own HOME (`os.UserHomeDir`), and any dir that holds it.
  A session whose HOME is a `mesh-home-*` dir under /tmp (a pod's HOME
  mounted at its host path), when it runs `go test ./...` and the
  package's `mesh node start` tests with it, would otherwise sweep its
  own HOME and Go build cache. `internal/commands`' `TestMain` also gives
  those tests a TMPDIR of their own;
- a dir not owned by the node's uid;
- a symlink, which is never followed or removed;
- every other prefix (`runsheet-*`, `latchkey-*`, `go-build*`), which is
  left to the quartermaster's age-bounded `scratch` check.

It logs `scratch: swept <n> dirs, <bytes> bytes at start (<k> kept)`. A
removal that fails is logged on one line before it, and the sweep goes on.

### The status line

Each `CacheTrim` reading is written to `~/.mesh/node.cachesize`
(`RunState.WriteCacheSize`/`CacheSize`, kept across restarts): the cache
root and its bytes, the cap, and the scratch root and its bytes.
`mesh node status` prints it beside the `validation cache:` hit-rate line:

```
cache: 4.2 GiB of 20.0 GiB (/home/u/.cache/mesh/cache); scratch 1.0 GiB (/home/u/.mesh/work/.scratch); measured 2026-10-17T18:00:00Z
```

Before the first reading it prints `cache: size unknown (no reading yet)`.

### The measurement

`scripts/host-tmp-reading.sh [--baseline FILE] <user>...` (item 5) sums
each named user's top-level /tmp entries (`TMPDIR_ROOT` overridable) by
prefix: `runsheet-*`, `latchkey-*`, `mesh-home-*`, `mesh-eval*`,
`mesh-session-*`, `go-build*`, other. With `--baseline` it prints each
row's change and an `added` total. The goal's after-release check — an
hour of host validations and evaluations on dl360 adds nothing under
/tmp owned by the node users — is two readings an hour apart (users
`anythink grapevine`), the second taken with `--baseline` naming the
first. Like every `Measure:` (ADR-020), it reports a reading, not a
verdict.

## Tests

- `internal/node/host_scratch_e2e_test.go`'s `TestHostScratchE2E`
  (`go test ./internal/node -run HostScratch`) runs an unsandboxed item
  through build, a host validation whose step runs `mktemp` and a tiny
  `go test`, and evaluation, against the fake gateway, with the process
  TMPDIR pointed at a test dir. It checks that the TMPDIR gains no
  entry, the scratch root is empty after, the shared build cache holds
  what `go test` built, and the trim's `node.cachesize` reading names the
  cache root against its cap. Its second case kills the node
  mid-session and restarts it. The sweep must remove the dead session's
  scratch and its `mesh-gh-`/`mesh-claude-` dirs, keep the detached live
  session's, and the live session's scratch must go once the adopted end
  is handled.
- `session_scratch_test.go`, `scratchsweep_test.go`, `cachetrim_test.go`,
  `internal/harness/scratch_test.go`, `internal/commands`'
  `TestNodeStatusShowsCacheSize`, and `scripts/host_tmp_reading_test.go`
  cover each part.

## Limits

- Only `ScratchRoot(workRoot)` is swept at start. Per-org scratch roots
  (ADR-034, each org has its own roots) are not yet passed to the sweep.
- A sibling node under the same uid shares /tmp's prefixes, so its live
  `mesh-gh-*` dir could be swept by the other's start. The keep rules
  are about the node's own user, not its own process tree.
- `CacheTrim` is wired where the served-diff cache is built (channel
  mode, the default). A node whose served-diff cache cannot be opened
  trims nothing and has no `cache:` reading.
- Channel-mode sessions and the operator's own `mesh mcp` sessions are
  not given a scratch.
