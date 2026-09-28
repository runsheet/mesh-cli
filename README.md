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

### What a session can reach on GitHub

Not your GitHub login. A session touches GitHub only with its item's own
token: when the node claims an item it asks the platform for a GitHub
App installation token (`issueItemRepoToken`), minted through the
installation of the org that owns the item's repo, restricted to that
one repository, contents write and pull requests read, living an hour.
Your personal GitHub access — every org and repo your `gh` login reaches
— is no longer what a session can reach.

The node puts the token on the checkout's own credential helper (in its
`.git/config`, clearing the operator's `gh auth git-credential` for
github.com), so clone, fetch and push use it alone, and a push from
inside a session to any other repo fails. A poll-mode session gets
`GH_TOKEN` set to it and `GH_CONFIG_DIR` pointing at a fresh, empty
directory, so `gh` never reads your `~/.config/gh`; channel mode's shared
session gets the empty `GH_CONFIG_DIR` and no token at all, since its
items' checkouts carry their own. A token near its expiry is renewed and
the helper rewritten, so a long session does not lose its push. A token
the platform refuses fails the item — it is not run on your login
instead. The planner and the director, which read a whole project's
repos rather than one item's, still fetch on the node's own `gh` login.

**The fallback.** A platform that has not yet rolled out the mutation
(its schema does not know `issueItemRepoToken`) gets the old behaviour:
the item runs on the operator's own `gh` and `git` credentials, exactly
as before, and the node logs once per run — not once per item — that
the platform issues no item GitHub tokens. The rollout is not a flag
day; once every platform serves the mutation the fallback goes.

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
file for that run (`--model-research` and `--model-rebase` too). A value is an alias the CLI
knows (`opus`, `sonnet`, `haiku`) or a full id (`claude-opus-5`); unset
leaves the CLI's own default.

These are the node's own models. An item's build and rebase sessions
run on the first model that names one, in this order:

1. the **item**'s own `model`, the one its plan chose;
2. its **agent**'s model, when the item names only an agent;
3. **node.yaml**'s `build` (or `default`); none leaves the CLI's own.

The evaluator runs on the stronger of the builder's model and `sonnet`,
so an item is never judged by a weaker model than the one that built
it (a node.yaml `evaluate` stronger than both wins). An item from an
older platform that carries no model builds and evaluates on node.yaml
as before.

The planner chooses every item's model, so without an explicit `plan`
it runs on the strongest model configured for any kind, and on `opus`
when none is; `mesh node model` shows the one it will use. Its graph
names a `model` per item with a one-line `model_reason`, by this rubric:

- `haiku` — mechanical items: docs-only, a goal-doc section, a version
  bump, a rename.
- `sonnet` — a well-specified change in one package with tests to copy.
- `opus` — anything cross-cutting, concurrency, migrations, security, a
  failing item's retry, or when unsure.

The model a session ran on is in `node.log` (`item <goal>/<seq>: build
on haiku`, `evaluate on sonnet`, `rebase on …`) and on the item's events
(`session started: <branch> on haiku`, and the evaluator's notes end
`(evaluated on sonnet)`); node.log also records each model the planner
chose when the plan is handed in.

### The director

A project on the platform carries **directions** — what it is for, in
words — each with the **hours** it may be acted on in (runsheet trd
§6c). A project may have several: "features overnight", "chores on
weekday mornings". Inside its hours a node runs one research session
against that direction, the project's repos and its context, and
proposes the goals worth doing next; the platform turns as many of them
into goals as the direction and its project have room for and asks the
planner for each. On an `experimental` project they need no approval, so
the work runs and the integrator merges it.

Nothing is set here: the words, the hours, the timezone, the gap between
sessions and the ceiling on live goals all live on the direction, and a
node simply offers each one every tick. Being refused — outside the
hours, inside the gap, the direction switched off, another node holding
it — is the ordinary answer. Two directions of one project take their
turns on their own hours, without waiting for each other.

A direction's **mode** is its own (runsheet ADR 022). A `set` direction
turns its proposals into goals up to the room, so a node does not offer
it while its project or its share is at the ceiling. A `propose`
direction sets nothing — its pitches wait for a person and take no
live-goal slot — so it is offered whatever the room, and its session is
told the room only as information: a pitch that would wait for room says
so. Its session hands in pitches — the problem, the evidence, the
strategy bet, the goal or initiative it would set, an estimate, risks,
alternatives and the signal it expects to move — and its answers to
open questions on earlier pitches; a `set` direction's session hands in
goal proposals. A platform older than pitches is handed them as
proposals instead, and the node logs that once.

```bash
mesh node model --research opus   # the session that decides what to build
mesh node start --research-budget-usd 5
```

### The rebase session

A session of kind **rebase** (runsheet ADR 008, trd §6a) takes a pull
request the integrator could not rebase itself. When a conflict is more
than mechanical, the integrator marks the item `rebase_state: requested`
and leaves it for a node.

- **Listing.** Every tick the node lists the requested rebases in each
  org it serves (`RequestedRebases`) and orders them with the rest of
  its work: by their goal's priority — urgent, high, normal, low; a goal
  with none is normal — then oldest seq first.
- **Claim.** It claims each with `ClaimWorkItemRebase`. The aggregate
  refuses a second claimant, so two nodes never run one rebase; a
  refusal is the ordinary answer and is not logged as an error. A
  draining node claims none.
- **The session.** It works in the item's own checkout on the item's
  token: it checks out the branch, rebases it onto its base, resolves
  each conflict keeping both sides' intent, re-runs the item's
  acceptance and pushes with `--force-with-lease`. The runbook is the
  mesh-branch-rebase skill in runsheet/runsheet
  (`skill-mesh-branch-rebase`, read through Prologue); the prompt
  carries its steps for when it cannot be fetched. The session ends with
  `REBASED: <sha>` and `RESOLVED: <files>`, or `REBASE FAILED: <why>`.
- **Finish.** The node reads the pushed head back from the remote and
  hands it in with the resolved files through `FinishWorkItemRebase`.
- **Fail.** Anything else — no token, a failed checkout, a session that
  gives up, a head that was not pushed — is `FailWorkItemRebase` with
  the reason, which hands the item to its builder with those words.
- **Capacity.** Like evaluations, rebases do not count against the
  node's capacity. `--rebase-budget-usd` caps each one; its heartbeat
  and its cost go in with `ReportWorkItemProgress`.
- **Model.** The `rebase` key under `models` in `~/.mesh/node.yaml`,
  `mesh node model --rebase`, or `--model-rebase` on `mesh node start`;
  unset, it takes `default`. That is the fallback: a rebase runs on the
  model its builder did — the item's, else its agent's — like the build.

```bash
mesh node model --rebase sonnet
mesh node start --rebase-budget-usd 3
```

Until the platform issues an item's token to the node holding its
rebase (ADR-001 asks for it), every rebase fails at `github token:` and
the item goes to its builder.

### Several Claude accounts

By default every session runs on the operator's own `claude` login.
Once the platform issues credentials (the claim-time read, adr-005),
each session runs on the Claude accounts the org's admins configured
for its project — handed to the node for that session only — and
node.yaml's `accounts:` are only the fallback, for a platform that
does not issue them yet (the node logs `credentials: the platform
issues no session credentials; sessions run on node.yaml's accounts`
once per run). Until then, to run on other accounts — several Max
subscriptions, or an API key — list them in `~/.mesh/node.yaml`:

```yaml
accounts:
  - label: max-a
    kind: subscription
    token_file: ~/.mesh/accounts/max-a.token
  - label: max-b
    kind: subscription
    token_file: ~/.mesh/accounts/max-b.token
  - label: api
    kind: api
    api_key_env: MESH_ANTHROPIC_KEY
```

**Adding a Max account.** Signed in to that account (in a browser the
CLI's login opens), run `claude setup-token`; it prints a long-lived
OAuth token. Save it to a file only you can read, and list the file as
the account's `token_file`:

```sh
mkdir -p ~/.mesh/accounts
claude setup-token                          # signed in to the Max account
cat > ~/.mesh/accounts/max-b.token          # paste the token, then Ctrl-D
chmod 600 ~/.mesh/accounts/max-b.token
```

**Adding an API key.** Put the key in a variable and name the variable
as the account's `api_key_env` — never the key itself. The service does
not read your shell's profile, so export it in the service's
environment: on Linux with `systemctl --user edit
io.projectmesh.mesh-node` and

```ini
[Service]
Environment=MESH_ANTHROPIC_KEY=sk-ant-...
```

(on macOS, an `EnvironmentVariables` entry in
`~/Library/LaunchAgents/io.projectmesh.mesh-node.plist`), then `mesh
node drain` so the service starts again with it.

**The spending order.** Before every claim — an item, a plan, research,
a rebase — and before an evaluation, the node picks the account the
session runs on, by the latest plan limits each account's sessions
reported (see Near the plan's limits). The accounts are the union of
node.yaml's `accounts:` and any credentials the platform issues; a
platform that issues none uses node.yaml only:

1. Subscriptions first: of those with headroom, the one with the most —
   the lowest use of its five-hour and seven-day windows. An account
   past `limits.stop`, or whose status is not `allowed`, takes nothing
   until its window resets; one past `limits.pace_five_hour` takes only
   urgent and high work. An account no session has run on yet has its
   full headroom.
2. An API key only for urgent or high work (before a paced
   subscription), or when every subscription is stopped — or, on a
   ranged node, has no slot free (see Capacity across accounts) — and
   work waits.
   Research never runs on an API key. Evaluations and rebases finish
   work already built, so a paced subscription takes them at any
   priority (a stopped one still does not).
3. **No account with headroom means the node waits**: the item is not
   claimed — not failed, not released; it stays ready for the next tick
   or another node — and the log says so once per tick:

```
accounts: no account has headroom for 2 item(s), 1 plan(s); left unclaimed until one has
```

Capacity still bounds how many items run at once. Each session gets its
account's secret in its environment only (`CLAUDE_CODE_OAUTH_TOKEN` or
`ANTHROPIC_API_KEY`), its own temporary `CLAUDE_CONFIG_DIR`, removed
after, and none of the operator's own Claude variables; the plan limits
are kept per label, so the next choice sees what this session read.
An API key the platform issues is never chosen once its daily or
monthly cap ($50 and $1,000 unless an admin set others) is spent, and a
session on one is held to what the cap leaves.

**Usage.** Every session — build, evaluation, plan, research, rebase —
reports its usage on the command that ends it (`usage` on
`finishWorkItem`, `evaluateWorkItem`, `planGoal`,
`finishWorkItemRebase` or `recordDirectionResearch`): the platform's
credential id and its kind, input, output and cache tokens, tool calls,
wall time, the list cost in cents (and the actual cost, on an API key),
and the plan's windows before and after. A platform whose finishing
command takes no `usage`, and a session that ends in a failure, leave it
in the node's log instead, one `usage: {…}` JSON line per session.
`mesh node start` refuses an account whose token file or variable
cannot be read.

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
console token it is still on — and a line for the node process on this
machine: running (its pid), draining, or none (see Drain below).

### Start

```bash
mesh node start [--mode channel|poll] [--interval 30s] [--unattended] [--max-budget-usd 10]
```

Every interval the node heartbeats (`HeartbeatNode`), lists the `ready`
work items of every org it serves and claims up to its free capacity
(`ClaimWorkItem`; the aggregate refuses a second claimant, so two nodes
never run one item). It claims them in one order across the orgs: by
their goal's priority — urgent, high, normal, low; a goal with none is
normal — then, within a priority, items with no evaluation before
retries, then oldest seq first. Each org's goals are read once a tick to
join the priorities; if that read fails, the org's items count as normal.
Planning and evaluation follow the same order across the orgs: the goals
in planning are claimed (`ClaimPlan`) and planned urgent, high, normal,
low (empty is normal), and the items this node holds for evaluation are
evaluated by their goal's priority, then seq — on the same one goals read
per org per tick.
There is no starvation guard beyond this order: a steady stream of urgent
work keeps a low goal's items, plans and evaluations waiting.
Rebases follow it too (see The rebase session below).
Capacity and the orgs served are re-read at the top of every tick, so
neither needs a restart: `capacity` and `orgs` come from
`~/.mesh/node.yaml` as it stands (a file that cannot be read is logged and
the last good capacity kept), and the orgs from the platform's `MyOrgs`
— an org seen for the first time is registered in and served that tick,
one no longer listed (or outside `orgs`) is dropped. A change is logged
(`capacity 3 → 6`, `orgs acme → acme, go-apis`). In channel mode the
goal watch of an org added this way waits for the next start; its
planning, items and evaluations do not.
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
{item_id, branch, head_sha}` — or `mesh_finish {item_id, outcome:
already_done, evidence}` with no branch, when main already holds the
item's work (the merged PRs or commits that satisfy its acceptance; the
evaluator checks them, mesh-specs ADR 003) — `mesh_fail {item_id, error}`, `mesh_evaluate
{item_id, passed, notes}`, `mesh_context {goal_slug}` (the project's
Prologue documents: instructions, ADRs, PRD, TRD), `mesh_items {status}`,
`mesh_record {item_id, summary}` (a milestone note to the platform; see below) and `reply`. The daemon
has already cloned or fetched the repo into
`~/.mesh/work/<goal-slug>/<seq>` and checked out `mesh/<goal-slug>-<seq>`
by the time the event arrives. It did that on the item's own GitHub token,
which is also the only credential the checkout's git uses. The session
itself runs with an empty `GH_CONFIG_DIR` and no `GH_TOKEN`, so the
operator's gh login is out of its reach. Its system prompt tells it to
read `mesh_context` first, do each item as a fresh subagent,
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
with that. On SIGTERM the node exits leaving its host `claude -p`
sessions running in their own units, for the next `mesh node start` to
adopt; a channel-mode or sandboxed session ends with it and its claimed
item is left for a retry. It never fails an item on its own shutdown.
SIGHUP does the same and execs the binary again at once.

### Drain: take a machine out of service

A node's host `claude -p` sessions run in their own units and outlive a
restart — the next process adopts them (see [Upgrading a node without
ending sessions](#upgrading-a-node-without-ending-sessions), which is how
a new binary goes in). A channel-mode session and a sandboxed one still
end with the node. To take a machine out of service — or to restart a
node whose sessions would end with it — drain it:

```bash
mesh node drain          # or: kill -USR1 "$(cat ~/.mesh/node.pid)"
```

`mesh node start` writes its pid to `~/.mesh/node.pid` (removed on exit);
`mesh node drain` sends that process `SIGUSR1`, and says so if no node is
running. The draining node claims nothing new — no work item, no plan,
no research, no rebase, and it starts no evaluation — while every
session it has running finishes: builds, evaluations, plans, research
and rebases alike.
Planners and evaluators do not count against capacity, which is why
waiting for zero running items never sees an idle node; the drain waits
for all of them. When the last one ends the node exits 0, and the service
(`Restart=always` under systemd, `KeepAlive` under launchd) starts it
again on the binary and config now on disk. An item it held for
evaluation waits in `evaluating` and is taken up by the node that comes
up.

While it drains the node leaves `~/.mesh/node.draining`, and `mesh node
status` shows it (`This machine: node running (pid …), draining`, and
`draining` beside its row). Its heartbeat carries `draining: true` as
well, a field sent only while draining; until the platform's node record
keeps it (runsheet `update-nodes-from-console`), the drain is known on
the machine alone.

Swapping the binary on the mini by a drain — for a sandboxed or a
channel-mode node, whose sessions do not outlive it (anything else:
`mesh node upgrade`, next section; a released version: `mesh node
update`, under Release):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh
scp mesh-linux mini:bin/mesh.new
ssh mini 'mv ~/bin/mesh.new ~/bin/mesh && ~/bin/mesh node drain'
ssh mini 'systemctl --user status io.projectmesh.mesh-node.service'   # a new pid once it has drained
```

Replacing the file under a running process is safe (the old one keeps
its inode); the drain is what makes it take. Capacity and the orgs served
need neither: they are re-read every tick (see Start).

### Upgrading a node without ending sessions

`mesh node upgrade` installs a new binary and restarts the node **at
once, without a drain**: every session the node runs — builds,
evaluations, plans, research, rebases — is its own unit (a transient
`systemd --user` scope named `mesh-session-<kind>-<subject>-<rand>`, or on
macOS a detached process group), so the node's exit does not end it,
and the new process adopts each one on start: it heartbeats the item at
once (the platform never sees it stall), follows the session's
`output.jsonl` from its saved offset and finishes the item as if nothing
had happened. The heartbeat lists them as `adopted` while they run.

```sh
mesh node upgrade                          # the latest release: verify, pull its image, swap, restart
mesh node upgrade --version v0.5.0         # that release
mesh node upgrade --binary ./mesh-linux    # a local build, once `./mesh-linux version` answers
```

A release is verified exactly as by `mesh node update` (signed
`SHA256SUMS`, its node image pulled); a local build must answer
`<path> version` with `mesh <version>`. Either replaces the executable
keeping the old one as `<path>.prev`, and a new binary that fails to
start twice is rolled back to it. Then the node is restarted:

- under systemd: `systemctl --user restart io.projectmesh.mesh-node.service`.
  The unit's `KillMode=process` signals the node alone; a unit installed
  before it has the default, which still spares the scopes (another
  cgroup) — `mesh node service install` writes the new one;
- under launchd: `launchctl kickstart -k gui/<uid>/io.projectmesh.mesh-node`
  (the agent has `AbandonProcessGroup`);
- with no service: `SIGHUP` to the pid in `~/.mesh/node.pid`, and the node
  execs the new binary in place (same pid). `kill -HUP "$(cat
  ~/.mesh/node.pid)"` does the same by hand, for a binary already in place.

A node whose sandbox is on is refused (its sessions' pods end with it;
`--force` restarts it anyway): use `mesh node update`, which drains
first. A platform-driven self-update takes the same path as `upgrade`:
verify, swap, exit at once, adopt.

**On the mini.** Build, copy, upgrade, and watch the sessions carry on:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh
scp mesh-linux mini:bin/mesh.new

# before
ssh mini 'mesh node status; systemctl --user list-units "mesh-session-*"'
#   This machine: node running (pid 41210).
#   UNIT                                              LOAD   ACTIVE SUB     DESCRIPTION
#   mesh-session-build-<item id>-3fa1c2d0.scope       loaded active running /home/chris/.local/bin/claude …
#   mesh-session-evaluate-<item id>-9b27e4aa.scope    loaded active running /home/chris/.local/bin/claude …

ssh mini 'mesh node upgrade --binary ~/bin/mesh.new'
#   /home/chris/bin/mesh.new answers `version` with v0.5.0.
#   v0.5.0 is in place at /home/chris/bin/mesh; the previous binary is kept at /home/chris/bin/mesh.prev.
#   Restarted io.projectmesh.mesh-node.service: the new process adopts the sessions the old one left running …

# after: a new pid, the same scopes still running
ssh mini 'mesh node status; systemctl --user list-units "mesh-session-*"'
#   This machine: node running (pid 41877).
#   mesh-session-build-<item id>-3fa1c2d0.scope       loaded active running …
#   mesh-session-evaluate-<item id>-9b27e4aa.scope    loaded active running …
ssh mini 'grep -E "adopted|restarting" ~/.mesh/node.log | tail'
#   adopted a build session of <item id> (pid …, running)
#   item <goal>/<seq>: adopted its running session (pid …, from byte …)
```

The scopes are the same units before and after — no session is started
again — and each item goes on to `evaluating` as usual when its session
ends; `mesh node status` in the console's Nodes page shows the new
version and the adopted sessions.

**When the node's contract changes.** A change to what the node reports
on the finishing command — usage records, tool call costs, or window
standings — reaches the mini only when its binary is upgraded by hand.
mesh-cli has no deploy: releases are signed GitHub artifacts that the
node pulls only when you ask it to `upgrade`. So if a goal lands that
changes the node's output format or semantics, the mini is not
automatically on the new version until you follow the steps above —
`mesh node upgrade --binary ~/bin/mesh.new` — and the first new rows on
the console show it worked. Until then, every session's report is only in
the node's local log, a fallback line that names the tokens and windows
but tells the platform it keeps no ledger.

<!-- RECORDED MANUAL RUN: replace this block with the run on the mini —
     date, the versions from → to, the `mesh node status` and
     `systemctl --user list-units 'mesh-session-*'` output before and
     after, and the node.log lines of the adoption. -->
> **Recorded manual run on the mini:** _not yet recorded — to be filled
> in from the first `mesh node upgrade` there._

### Near the plan's limits

Every `claude -p` session — build, evaluation, plan, research, rebase and
the channel session — reports its Claude plan's standing in a
`rate_limit_event`: a status (`allowed` while sessions may run), and how
much of the five-hour and the seven-day window is used, with when each
resets. The node keeps the latest reading per account (its label from
`accounts:`, else the session's `CLAUDE_CONFIG_DIR`, or `default`) and
paces each account by its own, so it slows down before the plan runs
out rather than failing items once it has. With several accounts, the
modes below are each account's, and every session goes to an account
that may take it (see Several Claude accounts); the node as a whole is
paced or stopped — and logs it — only when every subscription is:

- **Paced** — the five-hour window at `limits.pace_five_hour` (80%) or
  past it: the node claims only items of urgent and high goals, and runs
  at most half its capacity (never less than one) at once; it plans only
  urgent and high goals and starts no research. Evaluations and rebases
  go on.
- **Stopped** — either window at `limits.stop` (95%) or past it, or a
  status other than `allowed` (`allowed_warning`, `rejected`): nothing
  new starts — no item, plan, research or rebase, and no evaluation —
  as in a drain, but the sessions running finish and the process stays
  up. It lasts until the window that stopped it resets (the event's own
  `resetsAt` for the status; 15 minutes when it gives none).

Once a window's reset has passed, its reading is stale and the node goes
back to normal on its next tick. The thresholds are node.yaml's, read
again every tick like the capacity; left out, they are the defaults (a
value over 1 is read as a percent):

```yaml
limits:
  pace_five_hour: 0.80
  stop: 0.95
```

Every heartbeat carries the latest reading of each account as `limits`
— account, status, rate-limit type, `resetsAt`, overage status, the
`fiveHour` and `sevenDay` windows (utilization, `resetsAt`) and
`observedAt` — for the console to show (runsheet
`limits-on-the-console`); it is left out until a session has reported
one.

The log says each crossing once, not every tick:

```
limits: paced until 2026-09-25T15:00:00Z: five-hour window at 85% (pace 80%) — only urgent and high goals are claimed and planned, at most 2 item(s) at once, and no research
limits: stopped until 2026-09-26T14:00:00Z: seven-day window at 96% (stop 95%) — nothing new is started; 1 session(s) to finish
limits: back to normal (was stopped)
```

and a change to the thresholds in node.yaml as `limits: pace five-hour
80% → 50%, stop 95% → 95%`.

### Capacity that scales with headroom

`capacity` in `~/.mesh/node.yaml` is a plain number or a range:

```yaml
capacity: 3              # fixed: always 3 at once, as before
capacity: {min: 1, max: 4}   # scaling: between 1 and 4 by the plan's windows
```

Like the rest of node.yaml it is read again every tick: a new number
or range takes effect without a restart, and a range clamps the
capacity in force into it at once. A plain `capacity: N` runs none of
what follows. A ranged node starts at `min` and, once a minute, works
out a target from the same windows the node reads for its limits:

1. **Burn per slot**, per window: how much that window's utilization
   rose over the last 30 minutes, divided by the slot-minutes that ran
   in them (a cautious default before there is data, and a floor so a
   quiet half hour does not read as endless room).
2. **Affordable rate**, per window: (target − utilization) / the time
   to that window's reset. The targets are **0.95 for the seven-day
   window** — so the week is spread, not spent on day one — and **0.90
   for the five-hour window**.
3. **Slots** per window: the affordable rate / the burn per slot.
4. **The target** is the smaller of the two, clamped to `[min, max]`,
   and capped by **machine load**: when the 1-minute load average is at
   or above the CPU count, or free memory is 2 GB or less (CI runs on
   the same machine), the node grows no further than the sessions it
   already runs.

The capacity in force moves toward the target **one step at a time, at
most once every 5 minutes**. A shrink only stops claiming — a running
session is never stopped; the node claims again once it is under the
new capacity. **At `min`, only items of urgent and high goals are
claimed.** Each move is logged once with its reason:

| reason | the target is set by |
| --- | --- |
| `max` | both windows afford `max` or more (or nothing has been read yet) |
| `weekly pace` | the seven-day window |
| `five-hour pace` | the five-hour window |
| `machine load` | the machine's load or memory |

```
capacity 1 → 2: max (target 4, 1 running)
capacity 3 → 2: weekly pace (target 1, 3 running)
```

This sits under the hard stops of [Near the plan's
limits](#near-the-plans-limits), not beside them: *stopped* still means
nothing new whatever the scaler says, and *paced* still halves the
capacity in force and claims only urgent and high items. The scaler
only decides how many sessions run while the plan has room.

A ranged node's heartbeat carries `scaling` — `capacity` (in force),
`min`, `max` and `reason` — for the console's Nodes page (runsheet
`limits-on-the-console`); a fixed node's carries none. `mesh node
status` shows this machine's node as `2/1–4 (weekly pace)` in the
CAPACITY column when it scales, and the plain number when it does not.

Every heartbeat also carries `runtimes` — which of `npx`, `uvx` and
`docker` are on the node's `PATH` — and `adapters` — `claude-code`
and `codex`, by their CLIs — so the platform knows which local tools
the node can start (runsheet `tools-in-the-console`). Both are probed
every tick, so a runtime installed while the node runs is reported
without a restart; neither is sent when nothing is found.

### Capacity across accounts

A ranged node that lists `accounts:` scales by what **all** of them
afford together rather than by one plan's windows (adr-005, capacity
across accounts). node.yaml names the accounts and their caps — never a
secret — and, like the capacity, is read again every tick:

```yaml
capacity: {min: 1, max: 8}
accounts:
  - {label: max-a, kind: subscription, token_file: ~/.mesh/accounts/max-a.token}
  - {label: max-b, kind: subscription, token_file: ~/.mesh/accounts/max-b.token}
  - label: api
    kind: api
    api_key_env: MESH_ANTHROPIC_KEY
    daily_cap: 50        # list-price dollars; the default
    monthly_cap: 1000    # the default
    project: mesh-cli    # optional: spends that project's budget
project_budgets:
  mesh-cli: 30           # dollars a day, shared by the keys naming it
```

`kind` is `subscription` or `api`; anything else is refused, as is a
negative cap or a cap on a subscription. With no `accounts:` the node
runs on the operator's own login and scales as in [Capacity that scales
with headroom](#capacity-that-scales-with-headroom) — nothing changes.

**The rule, every minute:**

- **A subscription** affords the smaller over its windows of (target −
  utilization) ÷ minutes to reset ÷ its burn per slot-minute — the
  targets 0.90 five-hour and 0.95 seven-day, the burn measured over the
  last 30 minutes of *its own* sessions.
- **An API key** affords its money left today ÷ hours left today ÷ its
  dollars per slot-hour (measured the same way; $10 before there is
  data). The money left is the least of the daily cap less today's
  spend, the monthly cap less this month's, and the project's daily
  budget less what the project's keys spent today. **The last 10% of a
  daily cap is for urgent and high work only**; a key at either cap
  affords nothing — the hard stop.
- **Capacity** is the sum of every account's slots, clamped to `[min,
  max]` and held at the sessions running when the machine is busy;
  one step per 5 minutes, a shrink never stops a running session, and
  at `min` only urgent and high items are claimed.

**Spend** is counted in list-price dollars — each session's
`total_cost_usd` — per account, today and this month in the node's
local time, in `~/.mesh/ledger.json`, so a restart does not reset it.
The ledger also keeps each account's slot-minutes today.

**The spending order**, before every item is claimed: a subscription
with a slot free first (the most headroom wins); an API key only for
urgent or high work, or when every subscription is stopped or has no
slot free and work waits — and then only its normal slots; the reserve
slots only for urgent and high work; never a key at its cap. The
chosen account's environment is the session's. Work no account can
take stays ready, and the tick logs one `accounts: no account has
headroom …` line.

**Reasons.** The heartbeat's `scaling.reason` (and `mesh node status`)
says what set the capacity:

| reason | set by |
| --- | --- |
| `max` / `min` | the range clamped the sum |
| `machine load` | the machine's load or memory |
| `five-hour pace` / `weekly pace` | every account, by that window |
| `daily cap` / `monthly cap` / `project budget` | every account, by that money |
| `accounts` | the sum of several different bounds |

Each account's own slots (`Loop.AccountSlots`) are kept for the
heartbeat's per-account slots, which it carries once the platform
accepts them.

### Tools

A session gets the tools its project and role have, configured in the
console and handed to the node that claimed the session (ADR-006, trd
§3 Tools). The Prologue MCP server is not one of them: the node attaches
it to every session itself, and a console tool named `prologue` is
refused and noted.

**What the node reports.** Every tick the node looks on its `PATH` for
the tool runtimes it can start — `npx`, `uvx`, `docker` — and the agent
CLIs it has — `claude` (claude-code) and `codex` — and its heartbeat
carries them as `runtimes` and `adapters`, so one installed while the
node runs is reported without a restart.

**How tools reach the session.** At claim the node reads the session's
credentials and its `tools` (name, remote `url`/`headers` or local
`runtime`/`command`/`args`/`env`, `required`, `roles`) in one read, and
keeps the tools whose `roles` include the session's (`build`,
`evaluate`; plan, research and rebase sessions keep Prologue alone for
now). Each agent gets them in its own format:

- **Claude Code**: `--mcp-config` with `mcpServers` — Prologue plus one
  entry per tool, `{type: http, url, headers}` remote or `{command, args,
  env}` local (`npx -y …`, `uvx …`, `docker run -i --rm -e KEY… image
  …`) — and `--allowedTools mcp__<name>__*` for each, so a print-mode
  session can call them.
- **Codex**: one `[mcp_servers.<name>]` table per tool in a 0600
  `config.toml` in the session's temporary `CODEX_HOME`, removed with it
  (`tools.WriteCodexConfig`, from `Job.Tools`).

**Secrets in the environment only.** A secret header or env value is
written into the config as a variable — `${MESH_TOOL_<NAME>_<KEY>}` for
Claude Code (a bearer as `Bearer ${…}`), `bearer_token_env_var`,
`env_http_headers` or `env_vars` for Codex — and its value travels only
in the session's environment. It is never on the command line, in a
config file, in the node's log or in anything reported on the item:
log lines name the tools, and every text the node logs or reports about
the session is redacted of the values.

**Omit or decline.** A local tool whose runtime the node lacks:

- **nice-to-have** (`required: false`): the session runs without it,
  and the item gets a note — `tools: tool git-tools omitted: this node
  lacks uvx` (an evaluation says it in its verdict's notes);
- **required**: the node declines — the item is released
  (`ReleaseWorkItem`, attempts unchanged) with the reason `tools:
  required tool git-tools needs uvx, which this node lacks`, and this
  node does not claim it again while its runtimes are unchanged (an
  hour at most), so a node that has the runtime takes it. An item in
  evaluation cannot be released: it records no verdict and waits the
  same way.

**Priced calls are usage.** A tool with a `pricePerCallCents` is
reported by what the session used of it: the node counts the session's
calls per MCP server from its stream (Claude Code's `tool_use` blocks
named `mcp__<name>__…`, Codex's `mcp_tool_call` items) and, after a
build or evaluation, hands the platform one `recordToolUsage` per call
of a priced tool — the item, the call's id, the tool, and the session's
usage report at the tool's price per call — beside the session's own
report on its finish. Unpriced tools and Prologue report nothing.
A platform without `recordToolUsage` gets none: the node says so once
per run, logs each call instead, and the item goes on.

A platform that does not hand out `tools` yet costs nothing: sessions
run on Prologue alone, as before.

### Tool results

One `go test ./...` log or file dump can put 100 KB into a session's
context, and every later turn pays for it again. node.yaml's
`tool_results.max_kb` caps what one tool result may put there:

```yaml
tool_results:
  max_kb: 16   # the default; 0 turns the cap off
```

A build session on the host gets a Claude Code `PreToolUse` hook on
`Bash` in its `--settings`, `mesh tool-trim --max-kb 16 --hook`, which
rewrites each command to `( <cmd>\n) 2>&1 | mesh tool-trim --max-kb 16`
(a command that already pipes to `tool-trim` or runs in the background
is left as it is). Output over the cap comes back as its first 60% and
last 40%, cut at line boundaries, with
`[mesh: trimmed N KB of M KB; run the command again with a narrower filter if the middle matters]`
between. The pipeline's exit status is the trim's, so a failure shows in
the output, not the status. The session's environment also carries
`BASH_MAX_OUTPUT_LENGTH` (the cap in characters) and
`MAX_MCP_OUTPUT_TOKENS` (the cap at four bytes a token), so MCP results
and whatever the hook misses are bounded by the CLI itself. A sandboxed
session gets the environment but not the hook (the pod has no `mesh`),
and channel-mode sessions get neither. `mesh tool-trim --max-kb N`
works by hand too: it trims stdin to stdout.

### Sandbox

Every one-shot session — a poll-mode build, an evaluation, a plan, a
research or a rebase session — runs in its own rootless **Podman** pod
(mesh-specs ADR 007; `internal/sandbox/`), created when the session
starts and removed when it ends. Build, evaluate and rebase sessions
get their repository's services beside them in the pod (see
[Services](#services)). Channel-mode sessions and Codex do not run in
one yet.

- **Runtime.** `podman` on the node's `PATH`, rootless (the operator
  needs a subuid/subgid range, and the `cpu`, `memory` and `pids` cgroup
  controllers delegated to their systemd user slice for the limits to
  bite). The network is pasta. No daemon, no socket.
- **Node image.** `claude` runs from the node image,
  `ghcr.io/runsheet/mesh-node:<node version>` (`:dev` on a dev
  build), which carries the toolchains and the agent CLIs;
  `sandbox.image` names another or `build`s it here (see
  [Node image](#node-image)). Its `PATH` is
  `/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`
  and it must carry `nft` for the network rules.
- **Mounts.** Exactly two: the item's workspace, read-write at its own
  path, and a fresh temporary HOME (removed after the session) holding
  the `--mcp-config` file, `CLAUDE_CONFIG_DIR` (`$HOME/.claude`), an
  empty `GH_CONFIG_DIR` (`$HOME/.config/gh`) and a `.gitconfig` with the
  operator's commit identity. Never the operator's home, `~/.mesh`, a
  runner's directory or a container socket.
- **Environment.** Built from an allow-list, never inherited:
  `PATH`, `HOME`, `LANG`, `TZ`, `TERM`, `CLAUDE_CONFIG_DIR`,
  `MESH_ITEM`, `MESH_SESSION_KIND`, `DISABLE_AUTOUPDATER=1`, and of what the node gave the
  session: `GH_TOKEN`, `GH_CONFIG_DIR`, `CLAUDE_CODE_OAUTH_TOKEN` or
  `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `CODEX_API_KEY`,
  `MESH_ACCOUNT` and the tool secrets `MESH_TOOL_*`. Values reach
  podman through its own environment (`--env NAME`), never its command
  line. A sandboxed session has no operator login to fall back on: it
  runs on the account the platform or node.yaml gives it.
- **Network.** Outbound TCP 443 and 80 and DNS through pasta's
  forwarder; `192.168.0.0/16`, `10.0.0.0/8`, `172.16.0.0/12`, loopback
  beyond the pod's own, link-local, CGNAT, IPv6 ULA and link-local, and
  the host's public addresses are dropped by an nftables table an init
  container loads into the pod's own network namespace; the agent runs
  with every capability dropped and `no-new-privileges`.
- **Limits.** `--cpus`, `--memory` (swap the same) and `--pids-limit`
  on the agent's container, and the session timeout (the runtime's own
  `--timeout` is a backstop 30 s after the node's). A session killed by
  a limit — OOM, the pids cap, the runtime's timeout — is the node's
  condition, not the item's failure: the item is released
  (`ReleaseWorkItem`, attempt not counted) with the reason, the
  heartbeat's `reason` names the limit and the item for an hour, and
  the node does not claim the item again for that hour.
- **The heartbeat** carries `sandbox {mode, runtime, image}`.

node.yaml's `sandbox:` — every key optional:

```yaml
sandbox:
  mode: on             # on | off; left out: on when `podman --version` answers, else off
  cpus: 2              # the agent's --cpus
  memory: 6g           # the agent's --memory (and --memory-swap)
  pids: 4096           # the agent's --pids-limit
  services_cpus: 2     # the services' budget together
  services_memory: 4g
  registries: [docker.io, ghcr.io, "*.pkg.dev", quay.io]   # where service images may come from
  image: build         # default (empty): ghcr.io/runsheet/mesh-node:<version>; build: that tag built here; else as given
  service_timeout: 120s  # how long the services' health checks are waited for
```

| key | default | what |
|-----|---------|------|
| `mode` | on if podman answers, else off | `sandbox: off` (or `on`) alone is the mode |
| `cpus` | `2` | the agent container's CPUs |
| `memory` | `6g` | the agent container's memory, swap included |
| `pids` | `4096` | the agent container's process cap |
| `services_cpus` | `2` | the service sidecars' CPUs, together |
| `services_memory` | `4g` | the service sidecars' memory, together |
| `registries` | `docker.io`, `ghcr.io`, `*.pkg.dev`, `quay.io` | registries service images may come from |
| `image` | `ghcr.io/runsheet/mesh-node:<version>` | the node image sessions run in; `build` builds that tag here from the embedded Containerfile, any other value is used as given |
| `service_timeout` | `120s` | the bound on the services' health checks before the agent starts |

**`sandbox: off`** runs every session exactly as before: `claude` on
the host as the node's user, same arguments, same environment. It is
for a machine that cannot run rootless containers. With the mode left
out and no podman the node says `sandbox: off — the runtime does not
answer …` once at start and runs as before; with an explicit `mode:
on` and no podman `mesh node start` refuses to start — it never runs a
session unsandboxed on its own.

The probe test runs a real pod: `MESH_SANDBOX_E2E=1
MESH_SANDBOX_IMAGE=<image> go test ./internal/sandbox -run E2E`.
`scripts/e2e-sandbox.sh` runs the whole sandbox e2e — the node image
through `EnsureImage`, then a build and its evaluation on the node's own
path against runsheet's Postgres and a Redis fixture — and checks
whether anything still uses `latchkey-e2e-pg`: see
[docs/e2e-sandbox.md](docs/e2e-sandbox.md) for running it, the live run
on the mini and retiring that container.

### Node image

Every sandboxed session runs one image, versioned with the node:

```
ghcr.io/runsheet/mesh-node:<version>     # e.g. ghcr.io/runsheet/mesh-node:v0.4.0
```

It is built from [`images/node/Containerfile`](images/node/Containerfile),
which the binary embeds: Debian with `git`, Go at go.mod's version, Node
LTS with `npm` and corepack, Python 3 with `pip` and `uv`, `make` and a C
toolchain, `gh`, `nftables`, `ca-certificates`, and the `claude` and
`codex` CLIs, each at a version pinned in the file. The agents do not
update themselves (`DISABLE_AUTOUPDATER=1`, codex's
`check_for_update_on_startup = false`, and a root-owned install), the
user is the non-root `mesh`, and no secret is in it: a session is given
its token and secrets in its environment when it starts. A toolchain
bump is an edit to the file's `ARG`s, shipped with the next release.

`release-tag.yml`'s `image` job builds it for linux/amd64 and
linux/arm64 on every `v*` tag, pushes it and signs its digest keyless
with `cosign`; `publish` waits for it, so no release exists without its
image (ADR-002 amendment).

**It is pulled before the drain.** `mesh node update` and the node's own
self-update pull the *new* version's image once the release is verified
and before anything is drained, then remove every older `mesh-node` tag
but the running version's:

```
Ensuring the node image of v0.4.0…
Node image: ghcr.io/runsheet/mesh-node:v0.4.0 (pulled); removed ghcr.io/runsheet/mesh-node:v0.3.1
```

A pull that fails stops the update there: no drain, no swap, the node
stays on its binary — the command exits non-zero, a self-update puts the
reason on the heartbeat (`updateError`) and waits an hour on that
version. Nothing is built in its place unless node.yaml says so.
`sandbox: off` (or no podman and no mode set) pulls nothing.

**`sandbox.image`** in node.yaml:

| value | image |
|-------|-------|
| left out | `ghcr.io/runsheet/mesh-node:<version>`, pulled |
| `build` | that same tag, built on this machine from the embedded Containerfile instead of pulled |
| anything else | used as given, pulled |

`mesh node update <the running version>` ensures the running version's
image and nothing else — how a node set to `build` makes its first one.
While the repository is private so is the package: `podman login
ghcr.io` with a token that can read it, or `sandbox.image: build`. To
build it by hand:

```sh
podman build -t ghcr.io/runsheet/mesh-node:dev -f images/node/Containerfile images/node
```

### Acceptance the evaluator can see

The evaluator is a fresh session that runs before any pull request exists:
the integrator opens the PR only after `EvaluateWorkItem` passes. So
(ADR 004 in mesh-specs, `internal/harness/prompt.go`):

- **The planner** writes an item's acceptance only from what the evaluator
  can see at evaluation time, on that item's branch, in that item's
  repository: tests, commands and their output, file contents. Never a CI
  check on the item's own pull request, a deploy, or a file in another
  repository. Work like that becomes its own item in the right repo, or
  goes in the goal's acceptance. The node checks every plan for such
  clauses before `PlanGoal` (`internal/node/plan_check.go`) and re-asks the
  planner once with what it flagged; a second plan still flagged is handed in
  as it stands, with its flags logged.
- **The evaluator** reports a clause it cannot check at evaluation time — a
  CI check on the item's PR, a deploy, another repository's file — in its
  review as unverifiable, not failed, and does not fail the item on that
  clause alone. It passes when every clause it can check holds; a change
  against an ADR still fails. The verdict lines are unchanged.

The case that made the rule, runsheet's `update-nodes-from-console`: item
#6 required the `deploy / verify` check to succeed on its own pull
request, which opens only after evaluation passes. It failed evaluation 3
times, ran out of attempts, and left its goal with nothing to run. Item #4
asked a console item for a `specs/console-trd.md` diff, in a repository
its builder does not hold.

### Dependencies: only what an item uses

The node runs up to 6 items at once, and the scheduler readies every item
whose dependencies have merged. A dependency the item does not need only
makes it wait. So the planner (and the replanner) follows one rule:

- **An item depends on another only when it uses something that item
  creates or changes** — a type, command, field, function, file or
  endpoint — and could not be built against `main` without it.
- **Sharing a repository or touching the same file is not a dependency.**
  The merge queue rebases each pull request, and the scheduler's overlap
  hold covers conflicts.
- **Order by importance is priority, not dependency.** An item that
  matters more is not a reason for another to wait on it.
- **Prefer a wide, shallow graph**, so that as many items as possible are
  ready at once.
- **A final docs or goal-doc item depends only on the items whose results
  it records**, not on every item in the goal.

Each dependency the planner declares carries a one-line reason naming what
is used:

```json
"depends_on": [{"seq": 2, "because": "uses the PlannedItem.Because field item 2 adds"}]
```

The node's plan validation drops any dependency without a `because`, and
logs it, before `PlanGoal`. The reason stays on the node: it is added to
the dependent item's brief, and is not sent to the platform, which still
receives `depends_on` as seqs.

The case that made the rule: in goal `node-plans-from-the-truth`, item #29
waited on four items it did not use, so it ran only after all of them had
merged instead of alongside them.

### The goal's record in the repo

Status lives on the platform, not in git (ADR 003, decision 4). No item's
branch writes a goal status table: in channel mode `mesh_record` hands its
summary to the platform as a progress note on the item
(`ReportWorkItemProgress`, like `mesh_progress`) and writes no file, and
poll mode writes nothing either — a build's branch carries only its own
work. A session reads live status from `.mesh/context/INDEX.md` or the
platform.

Prologue is read-only over git, so a goal still writes itself into the
repo once, when it ends: `specs/goal-<goal-slug>.md` (frontmatter
`title: Goal — …`, `kind: plan`, `status: approved` when done /
`superseded` when cancelled, `summary:`, `parent:` the project's PRD when
Prologue lists one), with the statement, the acceptance and the final
`## Items` table. `internal/node/goaldoc.go` is its only writer. The node
writes it after an item finishes: it re-reads the goal, and if the goal
has ended (`done` or `cancelled`) it checks out the project's primary repo
— the first of its `repoIds` — fresh at its default branch, writes the
final doc and pushes it straight to that branch (`docs(mesh): goal <slug>
— finished`). No other repo the goal touched gets a copy, and a second
finish that finds the doc already final pushes nothing. A failure there is
logged; the item has already finished.

## Services

A sandboxed session (mesh-specs ADR 007) gets the services its
repository declares as sidecars in its pod, reachable on `127.0.0.1` at
their usual ports and removed with the session.
`sandbox.Resolve(repoDir, allowed)` (`internal/sandbox/services.go`)
reads the declaration; **the first found wins**:

1. **`.mesh/services.yml`** in the repository.
2. Else the **`services:` of the CI workflows that run tests**, from
   `.github/workflows/*.yml`: a job runs tests when a `run:` step runs
   `go test`, `npm|pnpm|yarn test`, `pytest`, `make test`, `cargo test`,
   `vitest` or `jest`, or when the job's id or name, or the workflow's
   name, says `test` or `ci`. The services of every such job are merged
   by name, first file (in name order) then first job winning. In a
   value, `${{ matrix.x }}` takes the matrix's first value, `${{ env.X }}`
   the job's or workflow's literal, `${{ a || 'default' }}` the default;
   any other expression (`${{ secrets.X }}`) drops the value with a
   note, and a service whose image cannot be resolved is skipped. These
   services are not required: reading a workflow is a best effort.
3. Else **none**.

`.mesh/services.yml` has the shape of an Actions `services:` map — at
the top level or under a `services:` key, so a block can be copied from
a workflow as it is:

```yaml
services:
  postgres:
    image: postgres:16            # a bare name is docker.io
    env:
      POSTGRES_PASSWORD: postgres
    ports: ["5432:5432"]
    options: >-
      --health-cmd "pg_isready -U postgres"
      --health-interval 2s --health-timeout 5s --health-retries 30
    command: []                   # optional: arguments to the image
    required: true                # default true; a required service that
                                  # cannot start returns the claim
    tuning: true                  # default true; see below
  redis:
    image: redis:7
    ports: ["6379:6379"]
```

- `options`: only `--health-cmd`, `--health-interval`,
  `--health-timeout`, `--health-retries` and `--health-start-period`
  are honoured, as the service's health check; any other flag is
  ignored and noted. `volumes` and `credentials` are not supported.
- **Test tuning**: a known database with no `command` (and no
  `tuning: false`) runs with durability off — Postgres (`postgres`,
  `postgis`) with `-c fsync=off -c synchronous_commit=off -c
  full_page_writes=off`, MySQL/MariaDB with
  `--innodb-flush-log-at-trx-commit=0 --sync-binlog=0 --skip-log-bin`.
- **Registries**: an image's registry (a bare `postgres:16` is
  `docker.io`) must match the allow-list — by default `docker.io`,
  `ghcr.io`, `*.pkg.dev`, `quay.io`. A disallowed image is returned as a
  refused service with a note naming its registry, never pulled.

### How a session gets them

Before a build, evaluate or rebase session's agent starts, the node
(`OpenSessionPod`, `internal/node/services.go`) resolves the checkout's
services and `Session.Start` (`internal/sandbox/sidecars.go`) creates
the session's pod and, for each allowed service, runs it as a sidecar
there — `podman run -d --pod … --pull missing`, so an image is pulled
once and kept in the node user's store — sharing the pod's network, so
it listens on `127.0.0.1:<its container port>` (the right side of
`"5432:5432"`; nothing is published on the host). The sidecars share
`services_cpus`/`services_memory` evenly and get 1024 processes each.
The node then waits for every service's health check at once — the
declared `--health-cmd` run in the sidecar (`podman exec … sh -c`),
else a TCP connect to its first port from a short-lived container of
the node image, else nothing — each check retried every
`--health-interval` (1 s by default), failing after `--health-retries`
failures past `--health-start-period`, and all within
`sandbox.service_timeout`. Then:

- **All ready**: the agent starts in the same pod. Its prompt gets a
  `# Services` section before the rules — each service with its image
  and `127.0.0.1` address — and `$HOME/.mesh-services` lists them, one
  per line: `name<TAB>image<TAB>127.0.0.1:port<TAB>ready`.
- **One that is not required fails or is refused**: the item gets a
  progress note (`services missing (not required; the session goes on
  without them): redis (redis:7): <why>`), the prompt and
  `.mesh-services` say it is missing and why, and the session goes on.
  An evaluation cannot report on its item, so the note goes in its
  verdict's notes instead.
- **A required one fails or is refused** (a disallowed registry
  included — then no pod is made and nothing pulled): no agent starts.
  A build is released (`ReleaseWorkItem`, the attempt not counted) with
  `services: required service postgres (postgres:16) did not come up:
  <why>`, and the node does not claim it again while that holds; an
  evaluation is left unevaluated and held off the same way; a rebase is
  given up (`FailWorkItemRebase`) with that reason.

On every way out — success, failure, the timeout, a limit kill, the
node stopping — the pod and its sidecars are removed (`podman pod rm
--force`) with the session's HOME. With `sandbox: off` no service is
started, as before.

The sidecar test runs real pods: `MESH_SANDBOX_E2E=1
go test ./internal/sandbox -run ServicesSidecarsE2E` reaches the Postgres
and Redis fixtures on `127.0.0.1:5432` and `:6379` from the agent's
container and finds no pod left after.

## Codex on a node

A node can run OpenAI's Codex CLI beside Claude Code. The adapter
(`internal/harness/codex.go`) runs `codex exec --json --sandbox
workspace-write` in the item's workspace, with the prompt on stdin.
Evaluations run on it: `mesh node start` builds it whenever `codex` is
on `PATH`, and an evaluation runs on Codex when a Codex account (one
the console issues, or node.yaml's `codex:` account) has headroom for
the goal's priority, else on Claude. A `gpt-…` evaluate or agent model
pins Codex, a `claude-…` one Claude. An urgent or high pass gets a
second opinion on the other family
(docs/adr/adr-011-evaluations-run-on-the-family-with-headroom.md).

**The node's family choice** (ADR-011): before an evaluation starts, the node
judges headroom across all Codex accounts and all Claude accounts and
prefers Codex when a Codex account has headroom for the item's priority.
A planner may pin an item's evaluator to a family through the agent model;
a pinned family with no headroom waits (not claimed, not failed). An urgent or
high goal's first evaluation on one family gets a second opinion on the other.

### Install the CLI

Verified with **codex-cli 0.157.0**. With Node.js:

```bash
npm i -g @openai/codex
codex --version
```

Without Node.js, take the `codex-package-<arch>-unknown-linux-musl`
tarball from the [releases](https://github.com/openai/codex/releases)
and put its binaries on the node's `PATH`, or name the path as
`codex.binary`. Keep `codex-code-mode-host` beside `codex`. Without it,
every session starts with a `Code Mode is unavailable` warning.

The workspace-write sandbox uses bubblewrap. On Ubuntu with
`kernel.apparmor_restrict_unprivileged_userns = 1` it fails
(`bwrap: loopback: Failed RTM_NEWADDR`), and every write and command in
the session is blocked. The session still "succeeds", and its message
says what it could not do. Give Codex's bwrap an AppArmor profile
before a Codex session builds anything on such a host.

### Add an account

Each session gets a fresh temporary `CODEX_HOME` under the user cache
dir (`~/.cache/mesh/codex/home-*`), removed when the session ends. The
operator's own `CODEX_HOME`, `CODEX_API_KEY` and `OPENAI_API_KEY` never
reach it. There are two kinds of account.

**A ChatGPT plan** (Plus, Pro, Business). Log it in to its own home,
once per account:

```bash
CODEX_HOME=~/.mesh/codex/plus codex login
```

Each session gets a copy of that home's `auth.json`. When Codex
refreshes the tokens during the session, the refreshed file is written
back.

**An API key.** Name the environment variable that holds it, or a file
(0600) that holds it. The key reaches the session only in its
environment, as `CODEX_API_KEY` and `OPENAI_API_KEY`. It is never
logged or written to the workspace or the temporary home. codex-cli
0.157.0's `exec` reads only `CODEX_API_KEY` and gets a 401 with just
`OPENAI_API_KEY`, so the node sets both.

Codex has no budget flag, so a job's `MaxBudgetUSD` is **not enforced**
for a Codex session. The cost is known only at the end, from the token
usage at list price. Use the session's timeout and the key's own caps
to bound it.

### node.yaml

The `codex:` block is **optional**. If present, the node's own Codex account
lives in it; if absent, the node uses only the accounts the platform issues
through the claim-time read. With neither, evaluations run on Claude.

```yaml
codex:
  binary: codex                 # default: codex on PATH
  accounts:
    - name: plus
      home: ~/.mesh/codex/plus  # a ChatGPT-plan login
    - name: api
      api_key_env: OPENAI_API_KEY
    - name: api-file
      api_key_file: ~/.mesh/codex/api.key
  models:                       # the Codex model for each Claude tier
    opus: gpt-6-astra           # default for Opus
    sonnet: gpt-6-sol           # default for Sonnet
    haiku: gpt-6-luna           # default for Haiku
```

Each account has a `name` and exactly one of `home`, `api_key_env` and
`api_key_file`. A load that finds anything else is refused.

**Default model mappings:** when a planner or agent model names a bare tier
(`opus`, `sonnet`, `haiku`) for a Codex evaluator, the node maps it using the
defaults above. Full model ids (`gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`) pass
through unchanged. Defaults may be overridden in the `models:` block.

**Issued credentials:** An OpenAI credential added in the console and serving
`codex` is handed to the claimed session by the claim-time read. A ChatGPT
login (the `auth.json` from `codex login`, pasted as the secret) is written
into the session's own temporary `CODEX_HOME/auth.json` and removed with it.
A login Codex refreshed is kept under `<user cache dir>/mesh/codex/logins/`
for the next session on the same secret version. An OpenAI key goes in the
environment only (docs/adr/adr-005-accounts-on-the-node.md).

`models` maps the Claude tiers to Codex models. A session asked for
`opus`, `sonnet` or `haiku` runs on that tier's Codex model, and so does
a full id of that family (`claude-opus-5` → `gpt-6-astra`). A tier left
out of `models` takes its default: `gpt-6-astra`, `gpt-6-sol`,
`gpt-6-luna`. An empty model, or one of no tier, runs on Codex's own
default.

The session's MCP servers (Prologue) are written to `config.toml` in
its temporary home. Secrets are named there by variable, never by value
(mesh-specs ADR 006). What Codex's event stream reports is documented
in [docs/codex-events.md](docs/codex-events.md): the final message,
token usage and cost at list price, and no plan rate-limit windows.

## Goals

```bash
mesh goal set goal.yaml            # slug, title, statement, acceptance, budget_cents, daily_budget_cents, checkpoints, project
mesh goal plan ship-it plan.yaml   # items: seq, title, brief, acceptance, depends_on: [seq…], repo: owner/name, base_ref, model
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

## Gateway fields are generated

What the node selects on each platform entity — `WorkItemIndex`,
`GoalIndex`, `ProjectIndex`, `DirectionIndex`, `NodeIndex` — is not
written by hand in `internal/gateway/client.go`. It comes from two
committed inputs (docs/adr/adr-010-generated-gateway-fields.md):

- `internal/gateway/schema.json`: the gateway's schema, every object
  and input type with its fields, sorted, one field per line.
- `internal/gateway/fields/<Type>.txt`: the allow-list of what the node
  selects on that type, one name per line, sorted (`#` starts a
  comment). A trailing `?` marks a field an older platform may lack:
  the client asks for the optional fields first and, once the gateway
  refuses one, reads the required ones alone (`itemsWithoutModel`,
  `nodesWithoutDesired`).

`go generate ./internal/gateway/...` runs `internal/gateway/internal/genfields`,
which reads only those files and writes `internal/gateway/fields_gen.go`:
per entity a `<entity>Required` and `<entity>Optional` list, one quoted
field per line, and the selections `<entity>Fields` and
`<entity>OptionalFields` the client uses. It fails, naming the field,
when an allow-listed field is not in `schema.json`, and when a list is
out of order. **`fields_gen.go` is never edited by hand**; the PR check
`generated gateway fields are current` runs `scripts/check-generated.sh`,
which regenerates and fails on any diff.

To add a field: add its line, in order, to `fields/<Type>.txt` (with
`?` if a platform the node still supports may not have it), add it to
the Go struct, and run `go generate ./internal/gateway/...`. If the
generator says the field is not in `schema.json`, the platform half has
not landed yet, or `schema.json` is stale — refresh it:

```sh
cd internal/gateway && go run ./internal/genfields -refresh && go generate .
```

`-refresh` introspects `https://api.runsheet.dev/graphql` (the schema is
public: no token) and rewrites `schema.json`; commit it with the
regenerated file. CI never reads the live gateway; the daily
`schema-refresh` workflow (`.github/workflows/schema-refresh.yml`, also
run by hand) does, and opens or updates the PR on `bot/schema-refresh`.

## Release

Nodes update themselves from the GitHub releases of
`runsheet/mesh-cli`, which `.github/workflows/release-tag.yml` cuts
from a tag (docs/adr/adr-002-signed-tag-releases.md). To cut one:

```sh
git tag vX.Y.Z && git push origin vX.Y.Z
```

Any tag starting `v` runs it; a tag with a `-` (`v0.4.0-test.1`) is
published as a prerelease. The version is stamped from the tag
(`internal/version.Version`), so `mesh --version` and the node's
heartbeat report exactly the tag. There is no manual trigger — tags only.

Test tags carry a `-` (`v0.0.0-selfupdate-test.<n>`), so they are
prereleases, and `mesh node update` with no version never picks one up as
the latest: a node runs one only when the platform or an operator names it.
The reference release is **`v0.0.0-selfupdate-test.1`**, cut from main to
prove the workflow end to end: its `SHA256SUMS` and
`SHA256SUMS.sigstore.json` are committed under
`internal/selfupdate/testdata/v0.0.0-selfupdate-test.1/`, and
`go test ./internal/selfupdate/...` verifies them offline against the real
`release-tag.yml` identity and refuses them under another tag's identity or
with an edited `SHA256SUMS`.

Each release carries five files, and the names are the contract the
updater reads:

| Asset | What |
|---|---|
| `mesh-linux-amd64` | the node for linux/amd64 (the mini) |
| `mesh-linux-arm64` | linux/arm64 |
| `mesh-darwin-arm64` | macOS on Apple silicon |
| `SHA256SUMS` | `sha256sum mesh-*` over the three binaries |
| `SHA256SUMS.sigstore.json` | the Sigstore bundle signing `SHA256SUMS` |

`SHA256SUMS.sigstore.json` is cosign 2.x's `--bundle` JSON (its legacy
form, not the protobuf Sigstore bundle); the node reads either.
`SHA256SUMS` is signed keyless with `cosign sign-blob`: no key exists,
the signature is bound to the workflow's GitHub OIDC identity and logged
in Rekor. The node accepts a release only when the bundle verifies
against exactly

- certificate identity
  `https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/<tag>`
- OIDC issuer `https://token.actions.githubusercontent.com`

and the binary's hash matches its line in `SHA256SUMS`. To verify a
release by hand:

```sh
TAG=vX.Y.Z
gh release download "$TAG" -R runsheet/mesh-cli \
  -p SHA256SUMS -p SHA256SUMS.sigstore.json -p mesh-linux-amd64
cosign verify-blob \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
```

There is no other release channel: `cli-release.yml` (main builds,
published unsigned to another repo) was dropped when the repo moved to
the runsheet org. `release.yml` is the Shipper's `workflow_dispatch` stub
and publishes nothing.

### Updating a node

**From the platform.** A node follows the version the platform asks it
for: after every heartbeat it reads its desired version (the console
sets it; empty, the version already running, or a `dev` build does
nothing). For a different one the node downloads that release's binary
for its OS/arch with `SHA256SUMS` and the Sigstore bundle, and verifies
them as above, and pulls its [node image](#node-image), *before*
anything else — a refused release or a failed pull costs no work,
its reason goes on every heartbeat (`updateError`) and the version is
left for an hour. A verified one swaps the executable atomically,
keeping the one it replaced as `<path>.prev`, and exits 0 at once — no
drain: its sessions run on and the new process adopts them (see
[Upgrading a node without ending sessions](#upgrading-a-node-without-ending-sessions);
a sandboxed node drains first). systemd (`Restart=always`) or launchd
(`KeepAlive`) starts the new binary, whose first heartbeat reports the
new version and confirms the update. `~/.mesh/update.json` counts the starts of a new binary: one
that fails to start twice is rolled back — the third start puts `.prev`
back, reports the failure on the heartbeat as the old version and exits
0 for the old binary to start — and that version is not tried again
until the platform asks for another.

The new process's first log lines — before it loads `node.yaml`, builds
a platform client or signs in — name its version, its pid, its config
path and whether an update is pending, so a start that dies before it
can do anything else is never silent. If it does die there, or panics,
while an update is pending, `update.json`'s `lastError` (after the third
start) says why instead of the bare "did not start": `'<to> did not
start: <the error>'`. `update.confirm_within` in `node.yaml` (below) is
how long a new process is meant to keep trying to confirm before it
gives up — default 5 minutes — so a platform blip does not by itself
roll an upgrade back; when it is exhausted, `update.json` names it
`'<to> did not confirm within <window>: <the heartbeat error>'`.

```yaml
update:
  confirm_within: 5m   # how long a new process keeps trying to confirm; default 5m
```

**By hand.** `mesh node upgrade` installs a release (or a local build)
and restarts at once, as the platform's update does (see [Upgrading a
node without ending sessions](#upgrading-a-node-without-ending-sessions)).
`mesh node update [version]` is the draining variant, for a node whose
sessions would end with it (the latest release that is not a prerelease
when no version is given):

```sh
mesh node update v0.4.0                  # verify, pull mesh-node:v0.4.0, drain the node, swap
mesh node update                         # the latest release
mesh node update v0.4.0 --verify-only    # download and verify into a temp dir; swap nothing
mesh node update v0.4.0 --verify-only --from-dir ./rel   # the same, from files already downloaded
```

It verifies first and refuses anything not signed by `release-tag.yml` on
`refs/tags/<version>` with issuer `https://token.actions.githubusercontent.com`,
or whose hash does not match, printing the reason and exiting non-zero
with the executable and `.prev` untouched. It then pulls the release's
[node image](#node-image) — a failed pull is refused the same way. A
verified release drains the
node running here (`~/.mesh/node.pid`, as `mesh node drain`; with none
running there is nothing to drain), swaps the executable keeping
`<path>.prev`, and says how the service will start it: the node exits
once its sessions end and the service starts the new binary, under the
same two-failed-starts rollback. It needs no platform, so it works when
the console cannot reach the node — or `mesh node update --verify-only`
checks a release from any machine.

**The repo is private.** GitHub answers 404 on its release download URLs
without (and even with) a token, so today neither the node nor `mesh node
update` can fetch a release by itself (ADR-002). Download it with `gh`
and verify or install from the directory:

```sh
TAG=v0.0.0-selfupdate-test.1
gh release download "$TAG" -R runsheet/mesh-cli -D rel
mesh node update --verify-only --from-dir rel "$TAG"
```

## Known Limitations

- Live browser-login and work-item testing still depends on the deployed `mesh-portal` and `mesh-api` carrying the matching session-auth changes.
- `mesh node login`'s device flow needs the org's public `mesh-cli` client id; there is no built-in default, and the runsheet gateway accepts machine tokens only from its own rollout phase on.

## Contribution

Contributions and fixes are welcome.
