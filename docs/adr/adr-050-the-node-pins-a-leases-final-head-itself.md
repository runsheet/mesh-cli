---
summary: When a v1 lease's brief grants exactly one exact, create-only push rule (the pin, meshapi.Git.Pin) and names a push_to, the node itself pushes push_to's final head to the pin once the session has ended with a finish, before the lease's grant closes, through the push tool's own relay paths (nodetools.NodePush); a pin the session already pushed is never moved, and the finish result's "pin" key names what happened — {ref, sha, head_sha, pushed}, plus mismatch and reason when the pin the session set is not the final head.
status: accepted
---

# ADR-050: The node pins a lease's final head itself

2026-11-11. Mesh v1. Applies to `internal/nodev1` (push.go, lease.go),
`internal/nodetools` (push.go) and `internal/meshapi` (types.go). Ports
the legacy node's pin (`gateway.JobBrief.Pin`) to the v1 wire shape.

## Context

A build's brief grants its push_to branch and one create-only ref the
platform reads the changeset from: the pin. The session was asked to
push the same final commit to both. Sessions forgot the pin, or pushed
it early and then committed again, so the pin named a commit that was
not the branch's head. The create-only rule then refused the session's
later push to fix it.

## Decision

**The pin is the one exact create-only rule.** `meshapi.Git.Pin()` is
the `ref` of the brief's only push rule that is `create_only` and not a
prefix (`…/*`). It is "" when there is none, or more than one.

**The node pushes it.** In `work()`, after `settle` and only when the
session ended with a finish that fits (`noFinish == ""`), and before the
lease's grant closes, `Runtime.finalPin` reads push_to and the pin
through the checkout's `origin`, which is the relay's loopback
(`git ls-remote origin`):

- When the pin does not exist, the node pushes push_to's head to it
  with `nodetools.NodePush`. That is the push tool's own sidecar or
  grant path, so the relay's rules judge the push and record it. The
  result is `{ref, sha, head_sha, pushed: true}`. A push that fails
  gives `pushed: false` and a `reason`.
- When the session already pushed the pin, the node never moves it and
  attempts no push. The result is `{ref, sha: <pin>, head_sha, pushed:
  false}`. If the pin is not the head, the result also has `mismatch:
  true` and a `reason` naming both shas.

The result rides the finish result as its `pin` key. A brief with no
pin, no push_to, or a push_to that does not exist yet is finished
unchanged.

## Consequences

The session does not need to push the pin. A pin that does not match the
head is reported to the platform, which decides what to do with it. The
node does not overwrite it. Only `internal/nodetools/push.go` runs `git
push` (`TestNoWritePathToAVendorButTheRelay`).
