---
summary: A build, a rework and a rebase work in the item's checkout ~/.mesh/work/<goal>/<seq> on the item's write token; an evaluation prepares a clone of its own at <goal>/<seq>-eval (EvalWorkspaceSpec), so its Authorize never rewrites the credential helper the build's push reads.
status: accepted
---

# ADR-027: An evaluation has its own checkout

2026-10-10, goal `a-build-pushes-before-it-finishes` (items 6 and 7).
Amends adr-001-item-scoped-github-token's "the evaluator is the same
node, on the same claim" and ADR-023's layout, which put every session
of an item in `~/.mesh/work/<goal>/<seq>`.

## Context

On the dl360 nodes (2026-10-10, sessions in pods) most builds failed at
the push: anythink 61 refusals against 11 finishes, grapevine 7 against
1, each `git push -u --force-with-lease origin mesh/<goal>-<seq>:
remote: Write access to repository not granted`, then `fail:
failWorkItem: not yours`. One item's activity shows the order:

- 04:35:56 a write token issued to the node (the build's claim);
- 04:39:17 `item_finished` — the finish reported, the item `evaluating`;
- 04:39:22 a read token issued to the same node, for the evaluation;
- 04:40:06 the build's push, refused.

The build and its evaluation shared one checkout. `Workspacer.Authorize`
writes the token into that checkout's own `.git/config` as its
`credential.https://github.com.helper` (adr-001), and a second
`Authorize` replaces the first: that is how a renewed token takes over.
So the evaluation's `Authorize`, preparing the checkout to read the
branch, replaced the build's write token with its own read token while
the build's push was still to come. Validation in a pod takes longer
than on the host (the unit suite about 40 s), so the race the Mini mostly
won was lost here.

## Decision

Which checkout each session of an item uses, and with which token:

| Session | Checkout | Token on its helper |
|---|---|---|
| build (`Runner.RunItem`) | `~/.mesh/work/<goal>/<seq>` (`WorkspaceSpec`) | the item's write token, issued at claim and renewed near expiry |
| rework (a later `RunItem` after a failed evaluation) | the same `<goal>/<seq>`; `Prepare` continues the pushed branch | the item's write token |
| rebase (`Rebaser.Rebase`) | the same `<goal>/<seq>` | the item's token, issued to the rebase's holder |
| evaluation (`Evaluator.Evaluate`) | `~/.mesh/work/<goal>/<seq>-eval` (`EvalWorkspaceSpec`) | the token issued for the evaluation |

- The sessions that push — build, rework, rebase — own the item's
  checkout. Each is one claim at a time on the item, so they never
  share it concurrently, and the branch they continue is the one the
  last of them pushed.
- The evaluation pushes nothing, so it needs no write token and no
  local branch the build left. `EvalWorkspaceSpec(root, goal, seq,
  repo, branch, base)` names a clone of its own beside the build's, at
  `<root>/<goal>/<seq>-eval` (`WorkspaceSpec.Path`). Its `Authorize`
  and `Prepare` touch only that directory: **an evaluation never writes
  the build's credentials**, whenever it starts relative to the build's
  push. It reads the branch from origin, as an evaluator on another node
  would (ADR-025's evaluation claim).
- `EvalWorkspaceSpec` keeps `Root` the work root, so the evaluation's
  clone and fetch take the same per-repo fetch lock as the build's
  (`fetchLocked`, `<root>/.locks/<owner>--<name>.lock`, ADR-023).
- The `-eval` clone is never removed, like the build's; a later
  evaluation of the item fetches into it.

`TestEvaluationLeavesTheBuildsCredentialHelper`
(`internal/node/workspace_test.go`) is the proof: on one node, against
a local stand-in for GitHub and the fake gateway issuing the tokens, a
build prepares on its write token and commits, an evaluation of the same
goal/seq then prepares on a read token, and `git credential fill` in the
build's checkout still answers the write token, the evaluation's
directory is another, and the build's push lands. With the evaluation
in the build's directory it fails exactly as the dl360 did: the build's
helper answers the read token.

## Consequences

- One more clone per evaluated item on disk.
- Separate checkouts remove the shared-credentials race. They do not
  enforce the order "push, then finish": that is the build's own
  contract (the rest of the goal: the node pushes the head with the
  token it holds and only then records the finish).
- A future session kind that only reads an item's branch takes a
  checkout of its own the same way, never the item's.
