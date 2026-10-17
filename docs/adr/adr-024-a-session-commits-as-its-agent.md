---
summary: A build or rebase session commits as `Mesh <role> (<slug>) <mesh-<role>+<slug>@runsheet.dev>`, author and committer, with one Co-Authored-By per paired person added by a node-written hook that also drops any Claude trailer, its Claude Code settings hiding Claude's own attribution — never as the node's owner, never with a Claude trailer (amended 2026-10-15).
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

## Amendment 2026-10-15: no Claude trailer

Goal `a-sessions-commit-carries-no-claude-trailer`. Commits on runsheet
still carried `Co-Authored-By: Claude … <noreply@anthropic.com>`: Claude
Code adds its own attribution unless its settings say not to, and a
model types the trailer from habit. The pairer's trailer is meant to be
a commit's only Co-Authored-By; two layers now make it so.

- **The session's settings hide Claude's attribution** (item 1).
  Every Claude Code session — host or sandbox, every role — is started
  with `--settings`, the job's `Settings` merged by
  `harness.SessionSettings` with `"attribution":
  harness.NoAttributionSettings()`, that is `{"commit": "", "pr": "",
  "sessionUrl": false}` (`internal/harness/claudecode.go`): no commit
  trailer, no `Generated with Claude Code` PR text, no session link.
  The empty-string form, per Claude Code's settings reference
  (code.claude.com/docs/en/settings-reference, "Git and attribution"),
  because `attribution: false` is taken only by CLI v2.1.281+ and an
  older one rejects the whole settings, the tool-trim hook with them;
  `includeCoAuthoredBy` is deprecated and ignored once `attribution` is
  set. A job with no `Settings` still gets `--settings`.
- **The hook drops a Claude trailer.** `prepareCommitMsg` first removes
  every line matching `claudeTrailer`,
  `^co-authored-by:[[:space:]]*claude.*<noreply@anthropic\.com>[[:space:]]*$`
  read case-insensitively (`grep -v -i -E`; GitHub's squash merges write
  `Co-authored-by:`), in POSIX sh and grep alone, since it runs in the
  pod's image too; then adds the pairers' trailers as before. A pairer's
  trailer stays — one named Claude too, whose email is not Anthropic's
  noreply. git runs prepare-commit-msg for `git commit -m` and
  `--no-verify` alike, and for the node's own `Git.Commit` in an
  identified checkout.
- **A rebase.** git's sequencer runs prepare-commit-msg (source
  `message`) for every commit it re-creates, so a rebase session's picks
  pass the same hook: a picked commit carrying a Claude trailer loses
  it, the pairer's is added, and a rebase never adds a Claude trailer. A
  pick git fast-forwards (the commit already on its new base) is not
  re-created and keeps its message as it was.
- Tests: `TestNilSettingsStillCarryNoAttribution`
  (`internal/harness/attribution_test.go`);
  `TestSessionSettingsCarryNoCoAuthor`,
  `TestPullRequestBodyCarriesNoClaudeAttribution`
  (`internal/node/attribution_test.go`);
  `TestBuildCommitDropsClaudeCoAuthorTrailer`,
  `TestBuildCommitKeepsPairerTrailerBesideDroppedClaude`,
  `TestBuildSessionCommitCarriesNoCoAuthorClaude`,
  `TestNodeCommitDropsClaudeCoAuthorTrailer`,
  `TestRebaseDropsClaudeCoAuthorTrailer`
  (`internal/node/identity_test.go`).
