---
summary: A session reaches GitHub with its item's own installation token, fetched from the platform with issueItemRepoToken; an old platform means the operator's login, with a warning.
status: accepted
---

# ADR-001: A session touches GitHub only with its item's token

## Context

A node cloned and pushed with its operator's own `gh` login, so a session
could reach every repo its operator can. The platform (runsheet goal
`item-scoped-github-token`) mints a GitHub App installation token per
item: through the installation of the org that owns the item's repo,
restricted to that one repository, contents write and pull requests read,
living an hour.

## Decision

- The node asks for it with one gateway mutation, only while it holds the
  item's claim:
  `mutation($in: IssueItemRepoTokenInput!) { issueItemRepoToken(input: $in) { token expiresAt repository } }`
  with `in = { aggregateId: <item id>, namespace: "runsheet", nodeId }`.
  `expiresAt` is RFC 3339; `repository` is `owner/name`. The client
  returns it as `gateway.GitHubToken{Token, ExpiresAt, Repository}`
  (`Client.IssueItemRepoToken`, on the `node.Gateway` seam) — callers
  depend on that type, not on the wire shape, which follows the platform
  if it lands differently.
- Anything the platform says no to (not this node's claim, no
  installation reaches the repo) is a refusal (`gateway.IsRefusal`).
- A gateway that does not know the mutation at all — the schema's
  "Cannot query field" / "Unknown type" naming it — is an older platform:
  `gateway.IsUnsupported` reports it (the `Error` carries
  `Unsupported`), and the node falls back to the operator's login and
  says so once. The rollout is not a flag day.
- "Once" is once per run of the node, not once per item: `mesh node
  start` makes one `node.TokenFallback` (a `sync.Once` around the log
  line) and hands it to everything that asks for an item's token — the
  poll-mode `Runner`, every `Evaluator`, and channel mode's
  `readyChannelItem` — so a node claiming many items against an old
  platform logs the fallback a single time and runs every item on the
  operator's credentials as before.

- The builder (`Runner.RunItem`) and the evaluator (`Evaluator.Evaluate`)
  ask for it right after resolving the item and before anything clones,
  fetches, diffs or pushes. So the platform must issue it to the node
  holding the item while it is `claimed`, `running` **or `evaluating`**
  (the evaluator is the same node, on the same claim).
- A session of kind rebase (`Rebaser.Rebase`, runsheet ADR 008) asks
  for it the same way, after `ClaimWorkItemRebase` and before its
  checkout is prepared: it pushes the rebased branch, so it needs
  contents write on the item's repo. The item is then in `review`, and
  the node holding it is the one its `rebase_node_id` names, which need
  not be the node that built it. **So the platform must also issue the
  token to the node holding an item's rebase while `rebase_state` is
  `claimed`** (amended 2026-09-24, goal `node-runs-rebase-sessions`;
  the platform half is runsheet's `gateway/itemtoken.go`). Until it
  does, the refusal fails the rebase with the reason — the item goes to
  its builder, as the platform's own stall timer would have sent it —
  and the operator's login is not used instead.
- The workspace uses the token through `Workspacer.Authorize(spec, tok)`
  (`internal/node/ghcred.go`, `workspace.go`). This sets `origin` to
  `https://github.com/<owner>/<name>.git`, since a `gh repo clone` may
  have left an ssh URL on the operator's key. It then writes, in the
  checkout's own `.git/config` and never the operator's global one, an
  empty `credential.https://github.com.helper` and then a helper that
  answers with the token. The empty entry clears every helper set in a
  lower config, `gh auth git-credential` among them. A checkout with no
  clone yet is created empty, and `Prepare` fetches into it.
- The session gets `GH_TOKEN=<token>` and `GH_CONFIG_DIR=<a fresh empty
  temp dir>` in `harness.Job.Env`. The directory is outside the
  workspace and is removed when the session ends, so `gh` never reads
  the operator's `~/.config/gh`.
- A token within five minutes of expiry (`GitHubTokenNeedsRefresh`) is
  renewed, and `Authorize` rewrites the helper. The runner checks on its
  progress ticker and again before it pushes. `GH_TOKEN`, fixed when the
  session started, is not renewed; git's helper is.
- A refusal fails the item (the evaluator returns the error and retries
  on the next tick). An item the platform will not give a token to is
  not run on the operator's login instead. Only `IsUnsupported` takes
  the fallback, and that leaves the workspace and environment as they
  were before this ADR.
- A token GitHub refuses while the workspace is prepared — `Prepare`'s
  clone, fetch or checkout answering `Repository not found`, 401 / 403,
  `Authentication failed` or `Invalid username or password`
  (`IsCredentialRefusal`) — is re-issued and tried again. A freshly minted
  installation token can be used before GitHub honours it: on 2026-09-24
  an item failed at `git fetch` with "Repository not found" minutes after
  others fetched the same repo on their own tokens. `PrepareWithRetry`
  (`ghcred.go`), which the builder, the evaluator and channel mode's
  `readyChannelItem` all prepare through, backs off 1 s, 2 s and 4 s. Before
  each retry it calls `ItemCred.Reissue`, which, unlike `Refresh`, always
  asks `issueItemRepoToken` and re-`Authorize`s. After three retries it
  answers a `*CredentialError{Repo, Expiry, Retries, Cause}`. Every refused
  attempt is logged with the token's repository, its expiry (RFC 3339)
  and git's error. The token itself is never logged: it is scrubbed from
  that error, and from any URL git echoes, before the error is logged or
  wrapped. Any other failure, and the operator login's fallback, is not
  retried. For now the item still fails on a `CredentialError`. mesh-specs trd §3
  has the node release it instead, as its own condition, the way it
  releases an item when it is out of credit. The command for that
  credential release is `ReleaseWorkItem { reason }` (trd §1, §3;
  `Client.ReleaseWorkItem(ctx, itemID, reason)`, the `releaseWorkItem`
  mutation): the platform takes it only for a `claimed` or `running` item
  the calling node holds, and puts the item back to `ready` with its
  node, harness and session cleared, `attempts` unchanged, and the
  reason — repo and expiry, never the token — in `last_note`, not
  `last_error`. The fake gateway (`gatewaytest.Fake`) honours it so;
  `RefuseReleases` makes it refuse, as a platform that does not take a
  node's release.
- Channel mode's one `claude` process runs many items, so its
  environment carries no item's token. `Session.once` starts it with
  `GH_TOKEN`, `GITHUB_TOKEN` and the enterprise tokens removed from the
  daemon's environment, and `GH_CONFIG_DIR` set to a fresh empty
  directory for the life of the process (`channel.SessionEnv`), so `gh`
  in the session reaches nothing. The daemon's `loop.Dispatch`
  (`readyChannelItem`) resolves the item, gets its token, `Authorize`s
  the workspace and `Prepare`s it before it pushes `work_item.ready`.
  The session's system prompt no longer tells it to clone; a missing
  workspace is a `mesh_fail`. Each item's git runs on that checkout's own
  helper. The channel's `repush` bookkeeping renews a held item's token
  near expiry every tick and rewrites the helper. A refusal fails the
  item, and so does a failed `Prepare`.
- On the old platform the shared session's empty `GH_CONFIG_DIR` would
  also hide the operator's global `gh auth git-credential` from its
  push. So the daemon gives such a checkout its own helper,
  `!GH_CONFIG_DIR='<the operator's gh config dir>' gh auth
  git-credential` (`Git.AuthorizeOperator`). That keeps the fallback's
  reach to that checkout, which is where it was before this ADR.
- The planner's and the director's `Fetch` read many repos for a whole
  project, not one item, and stay on the node's own credentials.

## Consequences

The platform side lands first; until it does, every node takes the
fallback. Once every platform serves the mutation the fallback can go.
