---
summary: A plan or research session's fetch runs on a token the platform issues for it — planGitHubToken(goalId) / researchGitHubToken(directionId), one row per repo of the project — never the operator's own gh login; a repo with no installation is a refusal naming it, and an old platform means the fallback ADR-001 already has.
status: accepted
---

# ADR-022: A plan clones on the platform's token

## Context

Chris, 2026-10-03: "grapevine is still stuck in planning." Read on the
Mini at 05:11Z, in the grapevine node's log (user `grapevine`, node.yaml
`contract: per-org`, orgs `[grapevine]`): every plan claim ended at the
fetch — `gh repo clone GrapevineNetwork/mobile … exit status 4` and
gh's own line, "populate the GH_TOKEN environment variable with a
GitHub API authentication token" — because that user had never run `gh
auth login`. Five goals (four handbooks and the QR-code goal) were
claimed, failed at the clone, held, and re-claimed every 30 minutes for
two hours; the platform saw only `planning`, and the node reported
nothing, since a fetch failure before the session opens is neither an
attempt nor a usage row.

ADR-001 gave a build this immunity already: the node asks the platform
for the item's own GitHub token, scoped to the item's one repo through
the App installation, and the operator's `gh` is never consulted. A
plan and a research session did not: `Planner.Plan` and the director's
research fetch the project's repos with `Workspacer`'s `gh repo clone`,
on whatever `gh` the node's own user happens to be logged into — so a
node set up for an org whose user never logged into GitHub cannot plan
at all, and a node that *can* plan does so on a person's own token with
every scope that person has, not the project's.

## Decision

**The read.** The node asks for a plan's or a direction's own GitHub
tokens, the way `IssueItemRepoToken` asks for an item's:

```
query($id: UUID!) { planGitHubToken(goalId: $id) }
query($id: UUID!) { researchGitHubToken(directionId: $id) }
```

each answering a JSON list — a Map scalar, as `itemCredentials` and the
rest of the claim-time read are (ADR-006) — one row per repo enrolled
in the project the goal or the direction belongs to:
`{token, expiresAt, repository}`. Every row is minted from that repo's
own account's GitHub App installation (an org links several GitHub
accounts: the owner of a row's repo, never the project's or the org's
own account) and scoped read-only to it — a plan or a research session
only fetches, it never pushes, so contents:write is not asked for.
`Client.PlanGitHubTokens(ctx, goalID, nodeID)` and
`Client.ResearchGitHubTokens(ctx, directionID, nodeID)` decode every row
into the existing `gateway.GitHubToken{Token, ExpiresAt, Repository}` —
callers depend on that type, not the wire shape, which follows the
platform if it lands differently (ADR-001's rule); `nodeID` is kept for
the interface, since the platform knows the node from the bearer, as
`Session` and `IssueItemRepoToken` already do. Both are on the
`node.Gateway` seam beside `IssueItemRepoToken`.

**Only the claim holder is answered.** `planGitHubToken` is refused to
any node but the one `ClaimPlan` recorded as the goal's planner, and
`researchGitHubToken` to any but the one holding the direction's
research claim — the same rule `issueItemRepoToken` applies to an
item's builder. Refused either way, the error is `gateway.IsRefusal`,
never `IsUnsupported`.

**A repo with no installation is a refusal, by name, not a partial
list.** When any repo enrolled in the project has no token to give —
the App is not installed on its account — the whole read refuses
rather than silently answering fewer rows than the project has repos,
naming the repo: `no GitHub installation reaches <owner/name>`. A
partial list would let the fetch silently skip a repo the plan needs
instead of surfacing the gap where a person can fix it.

**An old platform.** A platform without `planGitHubToken` or
`researchGitHubToken` answers the schema's "Cannot query field", which
`IsUnsupported` reports — `gatewaytest.Fake`'s `Unsupported["..."]`
mirrors it exactly as it does `itemCredentials`/`itemTools` in
`sessionRead`. The rollout is not a flag day: the caller falls back to
the operator's own `gh` login and says so once per run, ADR-001's
existing rule (`node.TokenFallback`) — not a new one for planning.

**On the session.** The fetch and the session both get `GH_TOKEN` and a
private `GH_CONFIG_DIR`, the way `ghcred.go` puts them on every item
session: the node's git credential helper on the plan's or the
research's own checkout, never the operator's global one or their `gh`
login. A project whose repos span more than one GitHub account has more
than one row; since one checkout's `origin` reaches one repo, the
session's `GH_TOKEN` is the first repo's row by sorted `repository`
name when the fetch itself needs a single ambient token (a tool call,
not a `git` remote already carrying its own helper) — each checkout
still reaches its *own* repo through its *own* row's helper, sorted
order only decides which row is "the" token where exactly one is
asked for.

**Contract fixture.** `specs/contracts/planGitHubToken.json` is the
shape, read-only is enough (runsheet serves `researchGitHubToken` the
same shape, named in the description) — the platform side of the
contract, the way `heartbeatNode.json` and `registerNode.json` are.

## Consequences

- A node whose operator never ran `gh auth login` can plan and research
  once the platform's App installations cover the project's repos — the
  grapevine failure (`gh repo clone … exit status 4`) cannot recur from
  a missing ambient login.
- A plan or a research session's git access is read-only and scoped to
  the project's own repos, never a person's full GitHub scope.
- This item lands the gateway client, the fake, the contracts fixture
  and this ADR; wiring the planner's and the director's fetch and
  session onto the read, and reporting a fetch failure as `planError`
  with a Needs-you row on the second identical failure, are later items
  of goal `a-plan-clones-on-the-platforms-token` (acceptance points 2
  and 3). Until they land, the planner's and the director's own
  project-wide fetches stay on the operator's `gh` and `git`, unchanged.
- A platform that does not yet serve either field costs nothing new: the
  existing fallback and its once-per-run log line carry planning and
  research exactly as they carry a build.

## Amendment 2026-10-03: the planner fetches on the read

Item 2 of the goal wires the planner. `Planner.plan` reads
`PlanGitHubTokens` after the project's repos and before any fetch; for
each repo, in sorted order, `Workspacer.Authorize(PlanRepoSpec(repo,
<plan dir>/repos/<name>), row)` makes the checkout — empty, origin the
https URL, the checkout's own credential helper on the row — and then
`Fetch` takes its `git fetch` branch, never `gh repo clone`. `Git.Fetch`
fetches `origin HEAD` by name, since an `Authorize`d checkout has no
branch tracking the default one. A refusal, or a repo the answer has no
row for, is the plan's error naming the repo, returned before any
session opens (item 4 of the goal reports it). The session's `job.Env`
is `SessionEnv(<first sorted repo's row>, NewGHConfigDir())`, the dir
removed when the session ends unless it detached. An old platform
(`IsUnsupported`) fetches on the operator's `gh` as before, said once
per run by the planner's own `TokenFallback` (`NotePlan`): `goal <slug>:
platform issues no plan token; fetching on the operator's gh`. The
director's research is item 3.

## A fetch that fails is reported

Item 4 of the goal. A plan that fails before its session opens — a plan
token refused or missing for a repo, a checkout's `Authorize`, or its
`Fetch` (`gh repo clone … exit status 4` on the grapevine node) — is a
`*PlanFetchError{Repo, Err}` from `Planner.plan`, and `Planner.Plan`
hands the claim back with it: mutation `releasePlan` on the goal id,
`{nodeId, note}`, the shape of `releaseWorkItem`, the note the error's
own text (`fetch acme/widgets: …`, `plan github token: … acme/gadgets`).
The node logs `goal <slug>: fetch failed: <err>; plan claim released`
and counts no try against `MaxPlanAttempts`: the platform holds the
record now, and the loop's `planRequested` claims again on its tick as
before, so a failure that repeats is repeated to the platform each time
instead of a silent claim that lapses every 30 minutes.

A platform without `releasePlan` (`IsUnsupported`) is said once per run
— `goal <slug>: the platform takes no plan release; the claim lapses
and the try counts` — and the try counted as before; a release the
platform refuses is logged with its error (`plan release refused: …`)
and the try counted. The no-credential and no-headroom holds
(`NoCredentialOf`, `NoHeadroomOf`) are unchanged: they come after the
fetch and are holds, not failures.

**The platform half is runsheet's.** `releasePlan` clears the goal's
plan claim (`planClaimedAt`, the claiming node), keeps it in
`planning`, and records the note as the goal's `planError`, which
`GoalIndex` shows; on the second identical `planError` it raises a
Needs-you row for the org's admins naming the repo and the error. The
gatewaytest fake mirrors the first part (`releasePlan` in `apply`,
`Unsupported["releasePlan"]` refusing the field); the Needs-you row and
its e2e are runsheet's to land, first, under the contract rule.

## Amendment 2026-10-03: the director fetches on the read

The director now does what the planner does. `Director.Research` reads
`ResearchGitHubTokens(direction, node)` after `Credentials.Open` and
`ProjectRepos` and before any fetch. For each repo, in sorted
`owner/name` order, it calls `Workspacer.Authorize(ResearchRepoSpec(repo,
<research dir>/repos/<name>), row)` and then `Fetch`. A refusal, or a
repo with no row in the answer, returns an error naming the repo
(`research github token: … <owner/name>`) before any session opens.
The session's `job.Env` is `SessionEnv(<first sorted repo's row>,
NewGHConfigDir())`. The dir is removed when the session ends, unless the
session detached.

An old platform (`IsUnsupported`) still fetches on the operator's `gh`.
The Director's own `TokenFallback` (`NoteResearch`) says so once:
`direction <project>/<slug>: platform issues no research token;
fetching on the operator's gh`.

## Amendment 2026-10-03: a refused research token hands in an empty research

A refused read — the platform naming a repo it will not clone for
(`no GitHub installation reaches <owner/name>`), or a project repo the
answer has no row for — no longer fails `Director.Research` outright:
`refusedToken` logs `<project>/<direction>: <err>; handing in an empty
research` and hands in an empty one through
`RecordDirectionResearch(direction.ID, node, gateway.Research{}, 0,
note, nil)` — `note` the error's own text, which names the repo — the
same way `refused` does for a refused credential (both now go through
the shared `emptyResearch`), then returns nil: no session ran, nothing
to cost or report, and the direction's claim is let go rather than held
for the platform's 45-minute timer. An old platform's `IsUnsupported` is
unaffected — `researchTokens` answers `nil, nil` for it, before
`refusedToken` is ever reached, and the fetch runs on the operator's
`gh` as before.
