---
title: "ADR-034: A session cannot prune the shared clone's worktrees"
summary: A worktree session's pod mounts the shared clone's common dir read-write but `<common>/worktrees` read-only over it and only its own admin entry read-write over that, so a `git worktree prune` in the pod — where every other item's work tree is absent and its entry's gitdir pointer dangles — can no longer delete live entries on the host; pruning stays the node's, on the host under the repo lock.
status: accepted
---

# ADR-034: A session cannot prune the shared clone's worktrees

2026-10-17, goal `a-sandboxed-session-cannot-prune-the-shared-clones-worktrees`
(items 1–2). Amends [ADR-028](adr-028-one-clone-per-repo-one-worktree-per-item.md)'s
"The sandbox mounts the clone's git dir" and
[ADR-007](adr-007-the-sandbox-on-the-node.md)'s mounts; follows
mesh-specs ADR 017.

## Context

ADR-028 made each item's checkout a `git worktree add` of one shared
clone per repo, and mounted that clone's whole git common dir
read-write into a sandboxed session's pod, so the worktree's `.git`
file resolves there and its commits land in the shared object store.
The common dir holds `.git/worktrees/<id>/` for **every** item of the
repo on the node, each with a `gitdir` file pointing at its work tree
under `~/.mesh/work/<goal>/<seq>`. Only the session's own work tree is
mounted in the pod; every other one is absent there.

So inside the pod every other entry's `gitdir` pointer dangles, and
`git worktree prune` — run by an agent tidying up, or by a tool that
calls it — judges those entries stale and deletes them. The mount is
read-write and the same directory as the host's, so the deletion lands
on the host: the other items' live worktrees lose their admin entries,
and every git command in them fails with

    fatal: not a git repository: /home/<user>/.mesh/repos/runsheet/runsheet/.git/worktrees/<n>

That happened twice on dl360: sessions of other runsheet/runsheet
items, mid-build, found their checkouts broken by a sibling session's
prune. `--expire` does not help: a dangling pointer is pruned whatever
its age once `--expire now` is given, and the default expiry only
delays it.

## Decision

**`.git/worktrees` is read-only in the pod, and only the session's own
entry is writable.** `Session.Mounts` (`internal/sandbox/sandbox.go`),
for a worktree, adds after the common dir

- `<common>/worktrees` read-only, and
- `<common>/worktrees/<id>` — the workspace's own admin entry,
  `sandbox.GitDir` (`git rev-parse --absolute-git-dir`) — read-write
  over it,

through `worktreeMounts`. `agentArgs` hands podman the mounts
shallowest target first, so the deeper one wins, and `mountsHold`
judges a path's writability by its deepest mount. The rest of the
common dir — objects, refs, logs, packed-refs, config — stays
read-write, as ADR-028 needs for commits and ADR-030 already keeps a
session's own config writes off the shared `config`. A prune in the pod
now reports `failed to delete '<entry>': Read-only file system` for
every sibling and deletes nothing; the session's own HEAD, index and
logs are still writable, so its commits work.

An own entry that cannot be read, or that is not directly under
`<common>/worktrees`, leaves the whole of `.git/worktrees` read-only,
logged: the session's commits then fail, rather than a sibling's entry
being pruned.

**Pruning stays the node's, on the host, under the repo lock.** The
prunes the node itself runs — `addWorktree` in `workspace.go` before
each `git worktree add`, `RemoveWorkspace`'s `git worktree remove
--force` + `git worktree prune`, and `addDetachedWorktree` in
`mainvalidation.go` for a merge batch's candidate or a merged commit —
run on the host, where every live work tree is present, so they remove
only what is really gone. They are unchanged.

## Consequences

- A session cannot break another item's checkout through
  `.git/worktrees`. It can still write the shared refs and objects, as
  any worktree can: a branch another item holds is protected by git's
  own checks, not by the mount.
- A session cannot `git worktree add` or `remove` in the shared clone
  from the pod; nothing a session does needs to, and the node owns the
  layout (ADR-028).
- Two more bind mounts per worktree session; a full clone's mounts are
  unchanged.
- A sibling's `config.worktree` (its token, ADR-028) is still readable
  in the pod, read-only now; the exposure ADR-028 accepts is unchanged.

## Proof

`internal/sandbox/mounts_test.go` checks the mount list and its order.
`TestPodCannotPruneWorktrees` (`internal/node/prune_e2e_test.go`, `-tags
e2e`) prepares two items of one repo on the shared clone, asserts the
session's mounts hold no read-write mount under `.git/worktrees` but
its own entry, runs `git worktree prune` and `git worktree prune
--expire now` and a commit in item 1's pod, and sees both entries still
in `<clone>/.git/worktrees`, item 2's `git status` working and item 1's
commit landed. It skips without podman or an image with git
(`MESH_SANDBOX_IMAGE`, else mesh-node at dev).
