---
title: "ADR-034: A node validates from a served diff with a shared cache"
summary: A main validation — a merge batch candidate or a merged item's merge commit — is no longer a worktree of the shared clone; the node reads the commit and the compare from GitHub's Git Data API on the minted token, materialises the merge base from one shared, content-addressed, capped cache into a scratch dir of its own under <work root>/.scratch, applies the diff, checks the tree and runs validate.yml there, removes the scratch on every path, falls back to the worktree when the served diff cannot give the commit, and reports the cache's hit rate in `mesh node status`.
status: accepted
---

# ADR-034: A node validates from a served diff with a shared cache

2026-10-17, goal `a-node-validates-from-a-served-diff-with-a-shared-cache`
(items 1–6). Amends
[ADR-028](adr-028-one-clone-per-repo-one-worktree-per-item.md):
validations of pushed commits no longer take worktrees. Amends
[ADR-032](adr-032-a-node-validates-a-merge-batch-candidate.md)'s checkout
of a candidate and a merged item's commit, and
[ADR-007](adr-007-the-sandbox-on-the-node.md)'s mounts for a validation's
pod. Follows mesh-specs ADR 017
(`adr-017-one-clone-per-repo-and-one-store-per-node`), which this
narrows; the amendment it needs is recorded on item 6, not made here.
The number repeats
[ADR-034](adr-034-a-session-cannot-prune-the-shared-clones-worktrees.md);
the two are separate decisions.

## Context

ADR-032 made a node validate a merge batch candidate, and runsheet ADR
054 a merged item's merge commit, by checking each out as a detached
worktree of the repo's shared clone (ADR-028): fetch the candidate or
the commit into the clone under the repo lock, `git worktree add` at
`<work root>/merge-batches/<id>-<attempt>` or
`<work root>/main-validations/<item>-<attempt>`, run the suites, remove
the worktree. Every validation fetched, held the repo lock, wrote an
entry in the clone's `.git/worktrees` and wrote out the whole tree
again, though consecutive candidates of one repo share almost all of
it. And what the steps wrote outside the checkout — `mktemp`, `go
test`'s temp dirs, go-build — went to the node's TMPDIR: on dl360 on
2026-10-15, 67G of `runsheet-*`, 42G of `latchkey-*` and 13G of
`go-build*` under `/tmp`, none of it ever removed.

A pushed commit needs none of the clone. GitHub already serves what a
validation reads: the commit and its tree, the compare `base...head` —
its merge base and changed files with their blob shas — and each blob by
sha (`platform/github`'s trees, blobs and compare, which the settled
merge reads too). The base is the same for every candidate of a batch
run and for most merges of a day.

## Decision

### The served diff

A main validation reads, on the run's minted token
(`gitdata.WithToken`; a batch's `issueItemRepoToken(mergeBatchId:)`,
ADR-032's amendment), from GitHub's Git Data API (`internal/gitdata`):

1. the candidate branch's head — not the batch's sha is
   `*CandidateHeadError`, nothing run — or, for a merged item's commit,
   `compare <default>...<sha>`, whose merge base must be the sha
   (`*CommitNotOnBranchError` otherwise);
2. the commit (its tree and parents);
3. `compare <base>...<sha>` — the batch's default branch, or the merge
   commit's first parent for a request (`validationTarget.diffFrom`) —
   its merge base and changed files: status, blob sha, previous name,
   mode (read from the merge-base and head trees, which the compare does
   not carry).

The merge base is materialised from the shared cache (below), the
diff's files applied — removed and renamed-from paths deleted, every
other file's blob written through the cache at its mode, a submodule an
empty dir — and what is on disk checked file by file against the
commit's tree (`checkMaterialized`: path, mode, blob id; nothing extra,
nothing missing). `validate.yml` runs there, its suites selected by the
compare's paths (a rename's old path beside its new), and the
attestation names the commit's sha and tree as before. node.log:
`<prefix>: materialised <sha7> from <base7> and <n> changed file(s) of
the served diff`. No worktree is added, nothing is fetched and the repo
lock is not taken (`internal/node/serveddiff.go`, `checkoutServed`).

### The node-owned scratch

Each validation's checkout and its steps' temp dirs are a scratch dir
of the node's own, `<work root>/.scratch/<kind>-<subject>-<rand>`
(`ScratchRoot`, `MakeScratch`): on the work root's filesystem, a dot
directory nothing that walks the work root takes for a checkout. The
steps' `TMPDIR` and `GOTMPDIR` point into it, `GOCACHE` at the node's
one Go build cache, `<cache root>/go` (`BuildCacheDir`). The scratch is
removed on every path — green, red, an error, a cancel — by
`RemoveScratch` (u+w before RemoveAll, so a read-only module cache goes
too); a sandboxed run's after its pod, by `Sandbox.RemoveTree` (RemoveAll,
u+w, then `podman unshare rm -rf`). A validation leaves nothing under
`/tmp` or the work root but an empty `.scratch`.

A sandboxed validation's pod (`sandbox.ValidationCache`,
`RunSpec.Cache`) mounts its scratch read-write, the Go build cache
read-write and the shared cache's `blobs/` read-only — nothing else of
the cache root — beside ADR-007's workspace and HOME;
`TMPDIR`/`GOTMPDIR`/`GOCACHE` are in its `BaseEnv`.

### The shared cache

One cache per node, `<user cache dir>/mesh/cache` (`SharedCacheRoot`;
`~/.cache/mesh/cache` on Linux), owned by the node's user, never a
session's, content-addressed:

    blobs/<sha[:2]>/<sha>          a blob's bytes, read-only
    trees/<sha[:2]>/<sha>.json     a commit's tree, flattened
    commits/<sha[:2]>/<sha>        a commit's tree sha

A miss is filled from the repo's shared clone when it holds the object,
else from the `BlobSource` — GitHub's Git Data reads
(`GitDataSource`) — once however many validations want it at once, each
blob checked against its sha before it is written. A materialisation
copies out of the cache, so the scratch is writable and the cache is
not. Every tree and blob lookup is counted a hit or a miss
(`CacheCounts`), persisted to `~/.mesh/node.cache`.

### Its cap and its finding

Everything under the cache root, the Go build cache included, is held
under node.yaml's `validation_cache_max_bytes` (default 20 GiB,
`Config.ValidationCacheCap`) by `SharedCache.Enforce`, after every
materialisation and every applied diff, a hit too: the least recently
used blobs, trees and commits go first, logged once per call that
evicts — `cache: evicted <n> least recently used entries (<size>) to
hold the cap of <cap>; <size> now` — then `go clean -cache` on `go/`.
Still over after both is the finding, as the quartermaster names one:
`cache over cap: <size> of <cap>`, logged once until the cache is back
under. Eviction never runs from under a copy (the cache's lock is held
shared by a materialisation, exclusively by `Enforce`).

### The fallback

The served diff cannot give every commit; then the validation takes
ADR-032's worktree as before, said once per run per reason —
`<prefix>: served diff: <reason>; validating from a worktree (said once
per run)`:

- `diff too large` — the compare holds GitHub's cap of 300 files
  (`gitdata.DiffTooLargeError`);
- `tree truncated` — a recursive tree GitHub truncated;
- `no minted token` — the run has no token of its own to read on: a
  merged item's request, which runs on the operator's credentials until
  the platform mints one for it;
- `no shared cache` — a Runner never readied for it
  (`Runner.ServedDiff`);
- `no parent to diff from` — a request's commit with no parent;
- `tree mismatch` — the materialised tree is not the commit's.

A token that cannot be renewed, a commit not on its branch or a head
that moved fails as it did; nothing else falls back.

### The hit rate in node status

`mesh node status` prints the cache's line — `validation cache: 75%
hits (30/40), 1.2 GiB of 20.0 GiB`: the persisted hits over lookups,
the cache root's size on disk and the cap in force — or `validation
cache: no lookups yet`.

## Consequences

- A batch run's candidates, and the merges of a day, cost GitHub only
  their own changed blobs and two trees after the first: the base is a
  cache hit. The node's e2e,
  `internal/node/serveddiff_e2e_test.go`'s
  `TestServedDiffValidationsShareOneCacheAndLeaveNoResidueE2E`
  (`go test ./internal/node -run 'ServedDiff|SharedCache'`), runs a
  merged item's validation and three candidates on one base against a
  bare origin and a Git Data fake and sees every one attest green, the
  hits rise with blob reads only for each diff's new blobs, a 32 KiB cap
  held by eviction, logged, and nothing left in TMPDIR or the work root
  but an empty `.scratch`.
- Validation no longer touches the shared clone: no fetch, no repo lock,
  no `.git/worktrees` entry, so it neither waits on nor races a build of
  the same repo, and ADR-034 (prune)'s worktree mounts do not apply to
  it.
- The cache is one more thing on the node's disk. Its cap bounds it,
  the Go build cache inside it; the finding says when the cap cannot
  hold.
- GitHub's rate limit is spent on Git Data reads in place of a fetch: a
  tree and a compare per validation, and a blob per new file.
- An evaluation's validation still runs on the item's own checkout
  (ADR-027): it is a checkout the evaluator needs anyway. A served diff
  for it is a later goal's.
- After release, an hour of validations on dl360 should add nothing
  under `/tmp` or `~/.mesh/work` owned by the node users;
  `scripts/validation-residue-reading.sh` (item 7) reads it.
