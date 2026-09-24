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

- The builder (`Runner.RunItem`) and the evaluator (`Evaluator.Evaluate`)
  ask for it right after resolving the item and before anything clones,
  fetches, diffs or pushes. So the platform must issue it to the node
  holding the item while it is `claimed`, `running` **or `evaluating`**
  (the evaluator is the same node, on the same claim).
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
- The platform refusing to issue the token fails the item (the evaluator
  returns the error and retries on the next tick). An item the platform will not give a token to is
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
  retried.
- A `CredentialError` is the node's condition, not the item's (mesh-specs
  trd §1, §3): GitHub refusing the item's token on the first try and on all
  three re-issues fails the item on neither path. The reason is
  `CredentialError.Reason()`: `github credential: <repo> token expiring
  <RFC 3339> refused 4 times: <git's error>`. It names the repo and the
  expiry and never the token, and the node logs it on both paths.
  - The builder holds a `claimed` or `running` item, so it hands it to
    `ReleaseOnCredential` (`ghcred.go`), which releases it with
    `releaseWorkItem { aggregateId, nodeId, note }`
    (`Client.ReleaseWorkItem`) instead of calling `FailWorkItem`. The item
    goes back to `ready` with its attempts unchanged and the reason in
    `last_note`, so its goal spends no re-plan.
  - **The platform as deployed refuses that call from a node**: runsheet's
    `decideMutation` has no case for `releaseWorkItem`, which only the
    platform's own offline-node process issues. A refused release must not
    leave the item `claimed`/`running` on a node that has stopped working
    on it, so `ReleaseOnCredential` then fails it with `FailWorkItem`
    and the same reason, as before this ADR. The release takes effect
    once runsheet lets a node release an item it holds (a platform change
    that lands first, per this repo's rules); the node needs no change
    then. The fake gateway (`gatewaytest.Fake`) refuses a node's release
    unless a test sets `NodeReleases`, so the tests run against both.
  - The evaluator's item is in `evaluating`, which the platform's
    `ReleaseWorkItem` does not accept (only `claimed`/`running`, only from
    the holding node), and sending a built, pushed branch back to `ready`
    would build it again. So the evaluator neither releases nor fails it:
    `Evaluator.Evaluate` logs the reason and returns an error, no verdict
    is recorded, and the item stays in `evaluating` for a later tick to
    evaluate on a freshly issued token. A way to hand an evaluation back
    would be a platform command first.

  One repo's credential does not stop the node claiming other work:
  unlike out-of-credit, it is not a node-wide pause, and the heartbeat
  carries no field for it.
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
  item, and so does a failed `Prepare` — for now even one that ends in a
  `CredentialError` after the retries: channel mode does not release it
  as the builder does.
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
