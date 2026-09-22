# Mesh CLI - Go Implementation

The operator tool for Mesh: this machine becomes a **node** of the runsheet
platform (`mesh-specs/specs/adr-002-control-plane-on-runsheet`), and goals
are set and watched from the same token. The node speaks to the platform's
gateway (`https://api.runsheet.dev/graphql`) as the machine credential
Latchkey issued it — or, until the estate has moved over, as the person
whose console token it holds. No gRPC, no relay, no shared secret.

## Installation

Build from source:

```bash
GO111MODULE=on go build -o mesh ./cmd/mesh/
```

## Node

### Credential

The node holds one credential in `~/.mesh/token` (mode 0600; `MESH_TOKEN`
overrides it for one process). Two kinds work, and the CLI tells them
apart by what is in the file:

**A Latchkey machine credential (preferred).** `mesh node login` with no
argument runs Latchkey's device flow (RFC 8628): it prints a code, opens
the browser, and waits while you sign in and approve. Latchkey registers
a **machine** for this computer — its own principal, bound to one org,
with you as its **operator** — and the CLI stores the machine's tokens as
JSON:

```bash
mesh node login --client-id <the org's mesh-cli client>
# Open https://auth.latchkey.id/oauth/device and enter the code:
#
#     WXYZ-1234
#
# Machine "mini" (3f1c9a2e-…) registered in runsheet for operator 9b2e6d41-…
```

The public client the flow starts from is registered per estate (a
`mesh-cli` client tagged to the org, device-enabled). Give it with
`--client-id` or `MESH_LATCHKEY_CLIENT_ID`; the login writes it (and the
issuer, `--latchkey` / `MESH_LATCHKEY`, default
`https://auth.latchkey.id`) into `~/.mesh/node.yaml` — before the
machine is registered too, since registering is itself done on the
credential — and `mesh node register` keeps them, so later logins need
no flags.

Its access token lasts an hour and is refreshed from the stored refresh
token a minute before it expires — by `mesh node start` and by the goal
commands alike, under a lock on `~/.mesh/token` so two mesh processes
never burn one refresh token (Latchkey rotates it on every use). Each
`claude` session is started with the bearer taken at that moment, so a
session never begins on a token about to die. Revoke the machine in the
Latchkey console (Machines) or `mesh node logout` to drop it here; either
way your own sign-in is untouched.

**An `rs_*` console token (the fallback).** Mint one under **Settings →
Tokens** on the runsheet console and save it:

```bash
mesh auth token rs_…        # or: mesh node login rs_…
```

The token's scope is the node's scope: whatever orgs the person reaches,
the node reaches — revoking it revokes the person's own bearer, which is
why the machine credential exists. It keeps working while the estate
moves over (latchkey `adr-machine-identity`, rollout phases 1–3); the
gateway accepts a machine token only once its own phase has landed, and
`mesh node login` says so when it does not.

### Models

Each kind of session runs on its own model, so the expensive work and
the cheap work need not share one:

```bash
mesh node model                                  # what this node uses
mesh node model --default opus                   # everything on Opus
mesh node model --plan sonnet --evaluate sonnet  # cheaper planning and review
mesh node model --build ""                       # back to the CLI's default
```

Saved in `~/.mesh/node.yaml`; restart the node (or its service) for a
change to take. `--model`, `--model-build`, `--model-evaluate`,
`--model-plan` and `--model-channel` on `mesh node start` win over the
file for that run (`--model-research` too). A value is an alias the CLI
knows (`opus`, `sonnet`, `haiku`) or a full id (`claude-opus-5`); unset
leaves the CLI's own default.

### The director

A project on the platform can carry a line of **direction** — what it is
for — and the **hours** it may act on it in (runsheet trd §6c). Inside
those hours a node runs a research session against the direction, the
project's repos and its context, and proposes the goals worth doing
next; the platform turns as many of them into goals as the project has
room for and asks the planner for each. On an `experimental` project
they need no approval, so the work runs and the integrator merges it.

Nothing is set here: the direction, the hours, the gap between sessions
and the ceiling on live goals all live on the project, and a node simply
offers itself every tick. Being refused — outside the hours, inside the
gap, another node holding it — is the ordinary answer.

```bash
mesh node model --research opus   # the session that decides what to build
mesh node start --research-budget-usd 5
```

### Register

```bash
mesh node register --org acme --name "Chris's laptop" [--slug chris-mbp] [--capacity 2]
```

Signs in, finds the org among yours, derives the node id from
`(org, slug)` — registering twice converges on the same node — and writes
`~/.mesh/node.yaml` (org login and id, slug, node id, capacity).
`mesh node status` heartbeats once and lists the org's nodes, above a
line naming the credential this machine runs on — the machine, its org
and operator, and when its access token is next refreshed, or the rs_*
console token it is still on.

### Start

```bash
mesh node start [--mode channel|poll] [--interval 30s] [--unattended] [--max-budget-usd 10]
```

Every interval the node heartbeats (`HeartbeatNode`), lists the org's
`ready` work items and claims up to its free capacity (`ClaimWorkItem`; the
aggregate refuses a second claimant, so two nodes never run one item).
What happens to a claimed item depends on the mode.

**Channel mode (default).** One persistent `claude` session runs in
`~/.mesh/work/<node-slug>` with the Mesh channel attached:

```
claude -p --input-format stream-json --output-format stream-json --verbose --dangerously-load-development-channels server:mesh --append-system-prompt …
```

A print-mode session ends with its turn and channel events only arrive
while a session is open, so the session runs in streaming-input mode with
its stdin held open until the node stops: it waits for user messages, and
each event is delivered twice — as the channel notification and as a
`[mesh event …]` user message on stdin, so the item is worked even if the
notification is missed. A `result` event on the stream is a turn's end
(logged for cost), not the session's. The daemon restarts the session only
when the process exits; on its own start and after every session restart
it takes up every item this node still holds in `claimed`/`running` (those
with no progress in the last interval) and pushes them again.

The daemon serves the channel to Claude Code as a stdio MCP server
(`mesh channel serve`, registered for that folder in `~/.claude.json`) and
pushes events into the session:

| Event | When |
| --- | --- |
| `work_item.ready` | the daemon claimed an item for this node (attributes: `item_id`, `seq`, `goal_slug`, `repo`, `base_ref`, `branch`, `workspace`; the brief and acceptance as content) |
| `goal.paused` / `goal.cancelled` / `goal.resumed` | the goal's status changed |
| `checkpoint` | the goal awaits a person's approval |

The session reports back through the channel's tools, each a thin call on
the gateway: `mesh_progress {item_id, note, cost_cents}`, `mesh_finish
{item_id, branch, head_sha}`, `mesh_fail {item_id, error}`, `mesh_evaluate
{item_id, passed, notes}`, `mesh_context {goal_slug}` (the project's
Prologue documents: instructions, ADRs, PRD, TRD), `mesh_items {status}`,
`mesh_record {item_id, summary}` (see below) and `reply`. Its system
prompt tells it to clone or fetch the repo into
`~/.mesh/work/<goal-slug>/<seq>`, branch `mesh/<goal-slug>-<seq>` from the
base ref, read `mesh_context` first, do each item as a fresh subagent,
run the acceptance, commit, push, then `mesh_finish` — or `mesh_fail`. The
daemon restarts the session if it exits; the claim stays the daemon's, so
the lock is held before the event is pushed.

Channels are a Claude Code **research preview**: custom channels load only
with `--dangerously-load-development-channels`; they need Anthropic
authentication through claude.ai (Max) or a Console API key and are not
available on Bedrock, Vertex or Foundry; Team/Enterprise orgs must enable
`channelsEnabled`. The flag syntax may change.

**Poll mode** (`--mode poll`). Each claimed item runs its own
`claude -p "<prompt>" --output-format stream-json --verbose --max-budget-usd <budget>`
in `~/.mesh/work/<goal-slug>/<seq>` (cloned with your `gh`, branch
`mesh/<goal-slug>-<seq>` from the base ref), with the goal's Prologue
context written under `.mesh/context/` (excluded from git). The budget is
the goal's remaining budget, capped by `--max-budget-usd`. The node
reports progress every minute (`ReportWorkItemProgress` with the cents
since the last report), pushes the branch with your credentials and
`FinishWorkItem`s — or `FailWorkItem`s. `--once` runs one tick.

In both modes `--unattended` adds `--dangerously-skip-permissions` so a
session never waits on a permission prompt — only on a machine you trust
with that. On SIGTERM the node kills its sessions and leaves claimed items
for a retry; it never fails an item on its own shutdown.

### The goal's record in the repo

Prologue is read-only over git, so a goal writes itself into the repo:
`specs/goal-<goal-slug>.md` (frontmatter `title: Goal — …`, `kind: plan`,
`status: draft` while running / `approved` when done / `superseded` when
cancelled, `parent:` the project's PRD when Prologue lists one), with the
statement, the acceptance, an `## Items` table regenerated from the
gateway's rows and a `## Log` that appends one dated line per call. The
session calls `mesh_record` after each milestone and before `mesh_finish`
(poll mode does it for the session); it is committed on the item's branch
(`docs(mesh): goal <slug> — item <seq>`) and lands with the merge, where
Prologue indexes it.

## Goals

```bash
mesh goal set goal.yaml            # slug, title, statement, acceptance, budget_cents, daily_budget_cents, checkpoints, project
mesh goal plan ship-it plan.yaml   # items: seq, title, brief, acceptance, depends_on: [seq…], repo: owner/name, base_ref
mesh goal approve ship-it          # the graph checkpoint
mesh goal pause ship-it --reason "…" / resume / cancel
mesh goal list
mesh goal watch ship-it            # polls every 5 s, prints item status changes
```

`--org <login>` picks the org (default: the registered node's);
`--gateway` or `MESH_GATEWAY` picks the gateway.

## Legacy commands (mesh-api)

### Authentication Commands

- `mesh auth login [baseUrl]` - Browser-based portal handoff login. Opens the Mesh portal login page, waits for the local callback, validates the returned session token with `mesh-api`, and saves the authenticated session locally.
- `mesh auth login --no-browser [baseUrl]` - Prints the browser login URL instead of opening it automatically.
- `mesh auth token-login [baseUrl] <accessToken>` - Explicit fallback. Validates the supplied bearer token against `mesh-api`, then saves the session locally.
- `mesh auth whoami` - Shows the authenticated identity for the current saved session.
- `mesh auth logout` - Removes the saved local session.

Sessions are stored in `~/.mesh-session.json`.

## Browser Login Architecture

`mesh auth login` now follows the auth model Mesh already uses:

- the portal owns end-user Supabase sign-in
- the CLI starts a local callback listener
- the CLI opens the Mesh portal login page with `cli_redirect=<local callback>`
- after successful sign-in, the portal redirects the browser back to the CLI with the signed-in access token
- the CLI validates that token with `GET /auth/session` on `mesh-api` before persisting it

This replaces the earlier attempt to force a generic OIDC Authorization Code + PKCE flow onto a Supabase setup that is not acting as the desired provider boundary for Mesh.

## Backend Expectations

`mesh-api` should expose:

- `GET /auth/login-start` returning browser-login metadata for the CLI
- `GET /auth/session` validating a bearer token and returning the current user identity

`mesh-portal` should support `GET /login?cli_redirect=<local callback>` and, after successful sign-in, redirect the browser to that local callback with the signed-in session token.

## Ralph Loop Work-Item Commands

After `mesh auth login`, the work-item commands use the saved signed-in user session and call `mesh-api` with the user bearer token.

Core flow:

1. create the work item
2. move it through the Ralph Loop state machine
3. hand it off between roles
4. record review
5. record learning

### Command shapes

- `mesh work-item create <title> [scope] [projectId]`
- `mesh work-item update <workItemId> [status] [currentOwner]`
- `mesh work-item handoff <workItemId> <fromRole> <toRole> [summary]`
- `mesh work-item review <workItemId> <reviewerRole> <decision> [notes]`
- `mesh work-item learning <workItemId> [outcome]`

For richer payloads, the same commands also support flags:

- `update`: `--title`, `--status`, `--priority`, `--owner-role`, `--current-owner`, `--scope`, `--project-id`, `--requester`, `--note`
- `handoff`: `--summary`, `--required-outputs`, `--repo-path`, `--system-target`, `--constraints`, `--test-expectations`, `--linked-spec-version`, `--note`
- `review`: `--notes`, `--criteria-checklist`
- `learning`: `--outcome`, `--what-worked`, `--what-failed`, `--follow-up-needed`, `--follow-up-item-id`, `--note`

### Friendly defaults and aliases

- `mesh work-item update <id> ready` maps to `ready_for_devlead`
- `mesh work-item update <id> review` maps to `in_review`
- `mesh work-item review <id> devlead approve` maps to decision `accept`
- `mesh work-item review <id> devlead changes` maps to decision `change_request`
- `mesh work-item move ...` is an alias for `mesh work-item update ...`
- `mesh work-item learn ...` is an alias for `mesh work-item learning ...`
- `mesh work ...` and `mesh wi ...` are aliases for `mesh work-item ...`

### Full-loop example

```bash
mesh auth login
mesh work-item create "Stabilize release flow" project <project-id>
mesh work-item update <work-item-id> ready
mesh work-item handoff <work-item-id> pm devlead "Spec approved and ready"
mesh work-item review <work-item-id> devlead approve "Looks good"
mesh work-item learning <work-item-id> "Release flow is stable now"
```

`mesh work-item update --status` is validated against the live Ralph Loop state machine values:
`inbox`, `clarifying`, `spec_ready`, `awaiting_approval`, `ready_for_devlead`, `in_execution`, `blocked`, `in_review`, `changes_requested`, `done`, `archived`.

## Release

Multi-platform binaries are published via GitHub Releases triggered by tags matching `v*.*.*`.

## Known Limitations

- Live browser-login and work-item testing still depends on the deployed `mesh-portal` and `mesh-api` carrying the matching session-auth changes.
- `mesh node login`'s device flow needs the org's public `mesh-cli` client id; there is no built-in default, and the runsheet gateway accepts machine tokens only from its own rollout phase on.

## Contribution

Contributions and fixes are welcome.
