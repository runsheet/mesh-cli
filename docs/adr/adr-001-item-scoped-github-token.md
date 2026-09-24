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
- A refusal fails the item (the evaluator returns the error and retries
  on the next tick). An item the platform will not give a token to is
  not run on the operator's login instead. Only `IsUnsupported` takes
  the fallback, and that leaves the workspace and environment as they
  were before this ADR.
- The planner's and the director's `Fetch` read many repos for a whole
  project, not one item, and stay on the node's own credentials.

## Consequences

The platform side lands first; until it does, every node takes the
fallback. Once every platform serves the mutation the fallback can go.
