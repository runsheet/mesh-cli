---
summary: A build or rebase session commits as `Mesh <role> (<slug>) <mesh-<role>+<slug>@runsheet.dev>`, author and committer, with one Co-Authored-By per paired person added by a node-written hook — never as the node's owner.
status: accepted
---

# ADR-024: A session commits as its agent

2026-10-03, goal `every-actor-has-a-name` (item 2).

## Context

A session's commits took whatever identity the checkout found: on the
host the operator's global `user.name`/`user.email`, in the sandbox the
same pair copied into the pod's `.gitconfig` by `nodecmd.go`'s
`gitIdentity` (ADR-007, Git identity). Every commit a node made read as
its owner's, whichever role made it and whoever was paired with it.

## Decision

- **The agent.** `AgentIdentity(role, slug)` (`internal/node/identity.go`)
  is `Mesh <role> (<slug>)` and `mesh-<role>+<slug>@runsheet.dev`: the
  role `tools.RoleFor` gives the session's kind (`builder`, `rebase`),
  the slug node.yaml's `slug`. Build and rebase sessions commit as it;
  an evaluation's checkout is read-only and gets none.
- **The pairers.** `Pairers(item, goal, configured)` are the people
  paired with the session, one `Co-Authored-By: <name> <email>` each.
  The platform serves no paired field on `WorkItem` or `Goal` yet
  (`internal/gateway/schema.json`; runsheet goal
  `mesh-work-tools-on-the-hosted-mcp-server` adds it), so the interim
  source is node.yaml's `pair: [{name, email}]` (`Config.Pair`). When
  the schema serves the field it goes into `fields/<Type>.txt` as
  optional and `Pairers` reads it first. A pairer missing a name or an
  email, or with a line break or angle bracket in either, is dropped;
  one email is one pairer.
- **Where it is set.** `Workspacer.Identify(ctx, dir, Identity)`, called
  by the Runner and the Rebaser after `Prepare` and before the session
  starts:
  - `git config --local user.name/user.email` to the agent (as
    `ghcred.go` writes the credential helper: the checkout's own config,
    never the global one);
  - `core.hooksPath` to `<workdir>.hooks` (`HooksDir`), whose
    `prepare-commit-msg` runs `git interpret-trailers --in-place
    --if-exists addIfDifferent --trailer 'Co-Authored-By: …'` per
    pairer. `addIfDifferent`, not `doNotAdd`: `doNotAdd` matches on the
    key alone, so a second pairer would never be added; `addIfDifferent`
    skips only the same trailer, so a message that carries it gains no
    duplicate. No pairer, no trailer. The directory sits beside the
    checkout, not in `<workdir>.session`, which the harness clears at
    every session's start.
  - `GIT_AUTHOR_NAME/EMAIL` and `GIT_COMMITTER_NAME/EMAIL` on `Job.Env`
    (`Identity.Apply`), so a session that commits past the checkout's
    config (`git -C` elsewhere, `-c` of its own) is still the agent.
- **In the sandbox.** `Sandbox.Open(label, kind, GitIdentity)` writes
  the identity it is given into the pod's `.gitconfig`; the node opens
  the pod with the session's (`Identity.Sandbox`), the harness — for a
  session with no pod of the node's — with the one on the job's env
  (`GitIdentityFromEnv`). The hooks directory is mounted read-only at
  its own path beside the workspace, and the `GIT_*` identity variables
  are on `AllowedEnv`. `sandbox.Config` no longer carries `GitName`/
  `GitEmail`, and `gitIdentity` is gone: nothing reads the operator's
  identity for a session.

## Consequences

- A repo's own `.git/hooks` do not run in a session's checkout:
  `core.hooksPath` points at the node's. A repo that relies on a
  committed hooks path is unaffected only through its CI.
- AGENTS.md's rule changes from "no Co-Authored-By trailer" to "a
  commit's only Co-Authored-By trailer is the paired person the node
  sets; a session adds none of its own".
- Tests: `internal/node/identity_test.go` commits through the Runner on
  a real checkout (`TestBuildCommitsAsTheAgentWithThePairer`,
  `TestNoSessionCommitsAsTheOwner`); `internal/sandbox`'s
  `TestOpenWritesTheAgentIdentity`.
