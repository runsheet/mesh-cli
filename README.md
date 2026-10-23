# Mesh CLI - Go Implementation

The operator tool for Mesh: this machine becomes a **node** of the runsheet
platform (`mesh-specs/specs/adr-002-control-plane-on-runsheet`), and goals
are set and watched from the same token. The node speaks to the platform's
gateway (`https://api.runsheet.dev/graphql`) as the machine credential
Latchkey issued it — or, until the estate has moved over, as the person
whose console token it holds. No gRPC, no relay, no shared secret.

## Work a goal from Claude Code, Codex or Cursor

`mesh mcp` is Mesh inside your agent of choice: a stdio MCP server on
your own login (the credential in `~/.mesh/token` — `mesh node
login`'s Latchkey credential or an `rs_*` token). Install it, with its
instructions, into whichever client you use:

```bash
mesh mcp install --client claude
mesh mcp install --client codex
mesh mcp install --client cursor
```

With no `--client` it installs into every client it finds on the
machine (config dir present, or its binary on `PATH`); `--client all`
is every client regardless. Each writes two things from one source
(`skills/mesh/SKILL.md`, embedded): the `runsheet` server entry
(`<this binary> mcp` over stdio, its initialize naming itself
`runsheet`) in the client's own config file and
format, and the same instructions rendered for how that client reads
them —

- **claude**: `~/.claude.json`'s `mcpServers.runsheet` (or
  `$CLAUDE_CONFIG_DIR/.claude.json`), and the skill itself at
  `$CLAUDE_CONFIG_DIR/skills/mesh/SKILL.md`, or
  `~/.claude/skills/mesh/SKILL.md` when that is unset.
- **codex**: `[mcp_servers.runsheet]` in `$CODEX_HOME/config.toml` (or
  `~/.codex/config.toml`), a custom prompt at
  `$CODEX_HOME/prompts/mesh.md` (`/prompts:mesh <form> $ARGUMENTS`),
  and a `mesh`-marked section spliced into `$CODEX_HOME/AGENTS.md` —
  the rest of that file, yours, is untouched.
- **cursor**: `~/.cursor/mcp.json`'s `mcpServers.runsheet`, a user skill at
  `~/.cursor/skills/mesh/SKILL.md` and a command at
  `~/.cursor/commands/mesh.md`, both keeping the `/mesh` form Claude
  Code uses. Nothing is written under the directory you run it from.

Every other entry, and every byte outside a marked section, is left
byte-for-byte; a config it cannot parse is reported and left alone;
re-running changes nothing. `--dry-run` prints every file's diff and
writes nothing. It refuses to overwrite a whole instruction file that
differs from the one mesh-cli ships unless you pass `--force`
(docs/adr/adr-025-mesh-mcp-install-is-one-table-per-client.md).

Runsheet's server is one entry per client, named `runsheet`. An earlier
install kept it under a legacy name — `mesh`, `mesh-context` or
`prologue` — and an install now removes every other entry of the
client's servers map that is runsheet's (it runs a `mesh` binary's
`mcp` or `mcp-relay`, or its URL is `/mcp` on `api.runsheet.dev` or the
configured gateway), saying `<client>: removed mesh from <path>`. A
legacy name pointing at something else is not runsheet's and is kept:
`<client>: kept mesh (not runsheet's)`.

`mesh mcp doctor` checks it afterwards, reading only. It opens with who
the runsheet server acts as and what it reaches — the person, by the
identity rule of runsheet ADR 056 and Latchkey ADR 013: an `rs_*`
token's user, or a machine credential's operator via the machine — and
the projects of the person's orgs; then, for each client it finds, a
line saying whether runsheet is configured and what it points at, and
one line per instruction file, `current`, `stale` or `missing` —

```text
identity: you@example.com (person, via machine 4f1c…)
projects: runsheet/mesh, runsheet/runsheet
codex: configured /usr/local/bin/mesh (/home/you/.codex/config.toml)
  /home/you/.codex/prompts/mesh.md: current
  /home/you/.codex/AGENTS.md: stale
cursor: duplicate runsheet entries: runsheet, mesh (/home/you/.cursor/mcp.json); run mesh mcp install
```

With no credential the first line is `identity: not signed in; run
mesh auth login`; a gateway that cannot answer is `identity: <email>
(projects unavailable: <err>)` and the client lines still follow. A
client whose runsheet entry sits beside a legacy one is `duplicate`;
one with only a legacy entry is `legacy`; with neither it is `missing
in <path>`.

A whole file is current when it is byte-for-byte what this binary
renders; the AGENTS.md section when the text between its markers is —
what you wrote outside them never counts. `--json` prints the same as
`{"identity": {"person", "via", "projects", "error"}, "clients": [...]}`,
each client a report (`client`, `status`, `command`, `url`, `path`,
`error`, `duplicates`, `instructions`). It exits 1 when no credential
loads, or when any client found is not configured and current — an
entry missing, unparseable, duplicate or legacy, an instruction stale or
missing — so `mesh mcp install` again is the fix; 0 otherwise.

With no local binary to run, `mesh mcp install --remote` writes each
client's remote-server entry for the hosted tools at the gateway's
`/mcp` (`https://api.runsheet.dev/mcp`, or `--url`). It asks the
server's OAuth metadata once and prints the answer: `auth: oauth` when
the issuer offers dynamic registration — the entry is the URL alone and
the client signs you in — else `auth: bearer from $MESH_TOKEN`, the
entry naming the variable (`${MESH_TOKEN}` for Claude Code,
`${env:MESH_TOKEN}` for Cursor, `bearer_token_env_var` for Codex) and
never holding a token; export `MESH_TOKEN` where the client runs.

With it in place, Claude Code, Codex and Cursor alike have
`mesh_goals`, `mesh_join`, `mesh_switch`, `mesh_status`,
`mesh_progress`, `mesh_ask`, `mesh_finish` and `mesh_release`
(docs/adr/adr-023-mesh-mcp-claude-code-is-a-personal-node.md), and the
same five everyday forms — `/mesh <form>` in Claude Code and Cursor,
`/prompts:mesh <form> $ARGUMENTS` in Codex:

- **`/mesh goals`** — `mesh_goals`: the live goals of the orgs you
  reach and their open items.
  > `/mesh goals` → `runsheet/widgets-goal (running): 3 ready, 1 review`
- **`/mesh join <goal> [seq] [--pair]`** — `mesh_join`: claims the item
  (or the goal's next ready one) and prepares its checkout. The
  checkout lands at `~/.mesh/work/<goal>/<seq>`, or the current
  directory when it is already that item's repo; `cd` there, read the
  brief and acceptance it answers, and work on the branch it checked
  out. `--pair` joins the branch another holder is already working on
  (`pair: true`) with no claim of your own.
  > `/mesh join widgets-goal 3` → `cd ~/.mesh/work/widgets-goal/3; on mesh/widgets-goal-3`
- **`/mesh switch <goal> [seq]`** — `mesh_switch`: pushes the held
  item's commits, notes why, releases it, and joins the target —
  answering like a join.
  > `/mesh switch other-goal` → `left widgets-goal/3: switching to other-goal; cd ~/.mesh/work/other-goal/1`
- **`/mesh status`** — `mesh_status`: each held item's goal, seq, path
  and branch, and the checkout's uncommitted changes and commits past
  base.
  > `/mesh status` → `widgets-goal/3 at ~/.mesh/work/widgets-goal/3 (mesh/widgets-goal-3), uncommitted`
- **`/mesh finish`** — commit first, then `mesh_finish`: pushes the
  branch and ends the item (or, with `outcome: already_done`, the
  evidence and no push). A reply that starts `push refused:` — the
  branch moved under you, usually a pair's holder pushing first — is
  relayed to you verbatim; `git pull --rebase` and finish again.
  > `/mesh finish` → `finished widgets-goal/3: pushed mesh/widgets-goal-3 at a1b2c3d`

`mesh node status` names the personal node these tools claim through
(`ADR-015`'s owner form, kind `personal`, capacity 1) once it is
registered.

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

**Registering on a machine credential.** On a machine credential the
node sends its machine id — the token file's `machineId`, else the
access token's `sub` after `machine:` — as `credentialId` on every
`registerNode`; an `rs_*` token sends none. The owner row then names the
machine, and heartbeats and claims pass only from it (the gateway's
`machineIsNode` rule). This needs the gateway at runsheet#2290 or later:
an older gateway's `RegisterNodeInput` has no `credentialId`, so the
node probes the schema once, leaves the field out and logs
`credential: machine <id>; credentialId left out: …` — a machine
registered that way is refused `not yours` by a gateway that does check.
Registration logs which credential it used: `credential: machine <id>
sent as credentialId`, or `credential: rs_* console token, no
credentialId`. To move a node onto its machine credential:

1. `mesh node login` on the node (it writes `~/.mesh/token` with
   `kind: "machine"` and its `machineId`), and say who owns it in
   `~/.mesh/node.yaml` — for a company's own box, the org (see Who owns
   a node below):

   ```yaml
   owner: org
   org: grapevine
   ```

2. restart it — `mesh node start`, or `systemctl --user restart` /
   `launchctl kickstart -k` of its service;
3. confirm in `~/.mesh/node.log` the registration line, `node <name>
   (<slug>) owned by …, serving <org>` — with `owner: org`, `node mini
   (runsheet-mini) owned by Grapevine (org), serving grapevine` — the
   `sent as credentialId` line and no `not yours`; node.yaml now holds
   `node_id`, `node_id_source` and, for `owner: org`, `org_id`;
4. clear the version pin (`desiredVersion`) on the console so the node
   follows releases again.

`owner: org` needs a gateway that takes `ownerKind` `org` (runsheet goal
a-node-is-owned-by-an-org, `gateway.OwnerOrgGateway`); on one that does
not, step 2's start exits non-zero with one line and registers nothing:
`owner: org needs a gateway that takes ownerKind org (runsheet with
a-node-is-owned-by-an-org); this gateway does not`.

A `register (owner person): registerNode: not yours — sent credentialId
<id> …` line means the gateway refused the machine on that row: the
owner row names another credential, or the operator who ran
`mesh node login` does not keep the row. Compare the id it names with
the row's `credential_id` on the console (Nodes → the node's slug). A
row naming another machine's credential needs `mesh node login` on that
machine, or the row's credential cleared by its keeper (runsheet
ADR 039, the clear); `sent no credentialId` means the token file has no
`machineId` and its access token's `sub` is not `machine:<id>` — log in
again.

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
inside a session to any other repo fails. A session gets `GH_TOKEN`
set to it and `GH_CONFIG_DIR` pointing at a fresh, empty directory, so
`gh` never reads your `~/.config/gh`. A token near its expiry is renewed and
the helper rewritten, so a long session does not lose its push. A token
the platform refuses fails the item — it is not run on your login
instead.

**A sandboxed build's job container holds no GitHub credential**
(docs/adr/adr-038-a-session-runs-as-ordered-steps.md). There the token
is never written into the checkout the pod mounts: the node's own
clone, fetch and push carry it on each git command's environment alone,
the checkout step fails if any git config of the workspace names it,
and neither `GH_TOKEN` nor `GH_CONFIG_DIR` reaches the pod
(`sandbox.HeldOutEnv`), whose `gh` is signed out. A host session keeps
the helper and `GH_TOKEN` above.

The push is the node's, after the session, and it goes out on the
build's own write token: just before it the node rewrites the
checkout's helper with the token it was issued at claim (or that
token's refresh), so nothing written there since can be the one used.
An evaluation of the item works in a clone of its own in the item's
job, `~/.mesh/jobs/<item id>/eval`, and never touches the build's helper
(ADR-027). A build session finishes on the node's own `mesh-node`
server, whose `mesh_finish` records nothing (the hosted one is
disallowed for builds): the order is **push → attest → finish**, and
node.log states it, `item <goal>/<seq>: pushed <sha>; finished <branch>
($x)`. A push GitHub refuses for its credential — `remote: Write access
to repository not granted` — is re-issued once and tried again; still
refused it fails the build, the branch left for a retry and no finish
recorded, logged with the token's access as issued, never the token:

```
item ship-it/3: push refused (access write, issued 2026-10-10T04:35:56Z, expires 2026-10-10T05:35:56Z): remote: Write access to repository not granted.; not finished
```

ADR-014 (its 2026-10-10 amendment) and ADR-023 (Decision 2) have the
detail.

The planner reads a whole project's repos rather than one item's, so it
asks for the goal's own tokens (`planGitHubToken`, adr-022): one per
repo of the project, minted through each repo's own account's
installation, read-only. Each repo's checkout under
`~/.mesh/jobs/<goal id>/plan/repos/` gets its own row on its own
credential helper before it is fetched — with `git`, never `gh repo
clone` — and the session gets `GH_TOKEN` (the first repo's by sorted
name) and a fresh, empty `GH_CONFIG_DIR`. A node whose user never
logged `gh` into GitHub plans; the operator's `gh` login is not
consulted. A
repo the platform issues no token for — the App is not installed on its
account — is the plan's error, naming the repo, before any session
opens.

The director's research fetches the same way, on the direction's own
tokens (`researchGitHubToken`, adr-022): one per repo of the project,
each checkout authorized on its own row before it is fetched, and the
session's `GH_TOKEN` (the first repo's by sorted name) and its own
`GH_CONFIG_DIR` — the operator's `gh` login is never consulted. A repo
the platform issues no token for, or a refused read, hands in an empty
research naming the repo rather than failing the claim, so the
direction is not left held; an old platform falls back to the
operator's `gh`, said once per run.

**The fallback.** A platform that has not yet rolled out the mutation
(its schema does not know `issueItemRepoToken`) gets the old behaviour:
the item runs on the operator's own `gh` and `git` credentials, exactly
as before, and the node logs once per run — not once per item — that
the platform issues no item GitHub tokens. A platform without
`planGitHubToken` plans on the operator's `gh` as before, said once per
run: `goal <slug>: platform issues no plan token; fetching on the
operator's gh`. The rollout is not a flag
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
`--model-plan`, `--model-research` and `--model-rebase` on `mesh node
start` win over the file for that run. A value is an alias the CLI
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
on haiku, budget $2.00 (the default for build/haiku)`, `evaluate on
sonnet`, `rebase on …`) and on the item's events (`session started:
<branch> on haiku`, and the evaluator's notes end `(evaluated on
sonnet)`); node.log also records each model the planner chose when the
plan is handed in.

### Session budgets

A session's budget is capped by kind — build, evaluate, plan, rebase,
research — and then by the model tier it runs on: `haiku`, `sonnet` or
`opus` (a full `claude-*` id resolves to its tier). `~/.mesh/node.yaml`'s
`budgets:` names the figures; a kind or tier it leaves out takes its
default — only `build` has one (sonnet build sessions are where spend
runs away): `haiku` $2, `sonnet` $4, `opus` $10. The other kinds keep
their historical flat cap (`--eval-budget-usd` 3, `--plan-budget-usd`,
`--research-budget-usd` and `--rebase-budget-usd` 5) until `budgets:`
names one for them too.

```yaml
budgets:
  build:
    haiku: 2
    sonnet: 4
    opus: 10
```

The session's actual cap is the smallest of the goal's own remaining
budget, this figure and the kind's `--*-budget-usd` flag (a session with
no goal, such as research, or a flag left at 0, drops out of the
comparison). Read live like the accounts and project budgets — a change
takes on the next tick, no restart needed. The platform is meant to
override this per claimed item or project once its schema carries a
`budgets` field (ADR 012); it does not yet, so node.yaml is the only
source.

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

Its fetch runs on the platform's own token, not the operator's `gh`
login (`researchGitHubToken`, adr-022): one row per repo of the
project, each checkout authorized before it is fetched, and the session
on `GH_TOKEN` with a fresh `GH_CONFIG_DIR`. A repo the platform issues
no token for hands in an empty research naming it rather than failing
the claim, and an old platform falls back to the operator's `gh` once
per run.

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
- **Regenerated first.** Before any session, account or model, a
  repository whose `.runsheet/validate.yml` declares `generated:` gets
  the mechanical path (ADR-038, a generated-only conflict is
  regenerated): the node rebases the branch onto its base itself, and
  at each conflict confined to the declared `paths` takes the base's
  side of each file, runs `generate`, then, once the rebase is done,
  every `check` — on the host, or in a pod of the org's sandbox, never
  the host on a sandboxed node. Only when every check passes is the
  head pushed with `--force-with-lease=<branch>:origin/<branch>`,
  attested and finished, the progress note reading `rebased <branch>
  onto <base> at <sha> by regenerating <paths> (no session)`. A
  conflict in any other path, a failing generate or check, or no
  conflict at all says why in one line (`regenerate: <why>; a rebase
  session instead`), puts the checkout back at `origin/<branch>` and
  runs the session below unchanged.

  ```yaml
  generated:
    paths: [platform/loomgen/**, platform/registry_gen.go]
    generate: ["cd platform && go tool loom generate"]
    check: ["cd platform && go tool loom generate --check", "go build ./...", "go vet ./..."]
    timeout: 10m   # each command; 10m when left out
  ```
- **The session.** It works in the item's own checkout on the item's
  token: it checks out the branch, rebases it onto its base, resolves
  each conflict keeping both sides' intent, re-runs the item's
  acceptance and pushes with `--force-with-lease`. The runbook is the
  mesh-branch-rebase skill in runsheet/runsheet
  (`skill-mesh-branch-rebase`, read through runsheet); the prompt
  carries its steps for when it cannot be fetched. The session hands the
  node its result through the node's `mesh_result` tool —
  `{rebasedHead: {headSha, files}}` — or gives up through `mesh_fail`
  with why (ADR-041).
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

### Credentials come from the platform

A node runs a session only on a credential the platform issues for the
session's org or its enterprise — never on the machine's own login.
Admins add the org's Claude subscriptions and API keys, and OpenAI
credentials for Codex, on the console's Credentials page
(`/orgs/<org>/settings/credentials`, console-trd §3) and attach them to
projects; an enterprise's credentials serve every project of its orgs
unless they name some. At claim time the node asks the platform for the
claimed session's credentials (`itemCredentials`, `planCredentials`,
`researchCredentials`; the claim-time read, adr-005) and the session
gets one of them for that session only. A read that answers with no
credential for the session's adapter starts no session and leaves the
work unclaimed (4, below); it never falls to node.yaml or the
operator's own login.

**node.yaml's `accounts:` and `codex.accounts` are retired.** A
node.yaml that lists either is refused — `mesh node start` exits
non-zero before it registers, and a running node that reads one on its
per-tick reload keeps its last good config and logs the refusal:

```
node.yaml: accounts: is retired — a node runs sessions only on credentials the platform issues; add them on the console's Credentials page and attach them to the project (see README, 'Credentials come from the platform')
```

Delete the list (an empty one loads) and add the accounts in the
console instead.

**Which projects have none.** Once the first heartbeat has answered the
orgs the node serves, `mesh node start` reads each org's credentials
and projects and logs one line for each project with no credential
open to this node — active, serving the adapter, attached to the
project (an enterprise's, to it or to none) and not limited to other
nodes — for `claude-code` and, when the `codex` CLI is on PATH, for
`codex`:

```
credentials: org acme's project latchkey has no claude-code credential open to this node
```

and, for each adapter every project has one for, `credentials: every
project has a claude-code credential open to this node` (the same for
`codex`) — said for claude-code whatever codex's coverage. A platform that does not
serve `credentialIndexs` to the node says so once (`credentials: the
platform does not serve credentialIndexs to this node; which projects
have none is known at claim time`) and the start goes on.

**The spending order.** Before every claim — an item, a plan, research,
a rebase — and before an evaluation, the node picks the account the
session runs on, by the latest plan limits each account's sessions
reported (see Near the plan's limits). The accounts are the
credentials the platform issues for the session — a read that answers
with none for the session's adapter is a hold (4, below), never
node.yaml or the machine's own login:

1. Subscriptions first: of those with headroom, the one furthest
   behind its target line — the window's elapsed fraction less
   `limits.reserve`, read on whichever of its five-hour and seven-day
   windows has a reading (the seven-day one when both do); an account
   ahead of its line is chosen last, and then the least ahead of them
   (goal every-subscription-lands-full-at-its-reset). An account past
   `limits.stop`, or whose status is not `allowed`, takes nothing until
   its window resets; one past `limits.pace_five_hour` takes only
   urgent and high work. An account no session has run on yet is the
   furthest behind its line, so it still has full headroom. **The
   holdback:** once every subscription but one is within 10% of
   `limits.stop`, the one with the latest reset is kept for urgent work
   — normal, low and high work wait rather than spend it, and never
   fall to an API key through it — so one subscription always has
   headroom; the log says `accounts: <label> held back for urgent work
   (the others within 10% of stop); <n> item(s) left unclaimed until
   <earliest reset>`.
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

4. **No credential issued means the node waits too.** A node runs
   sessions only on credentials the platform issues for the org or its
   enterprise, never on the machine's own login: when the claim-time
   read answers with none for the session's adapter (`claude-code` or
   `codex`), no session starts — not on node.yaml's `accounts:`, nor
   a Codex evaluation on the `codex:` block's own login — a claimed item is released, and that
   project's items, plans and rebases are left unclaimed — asked again
   by one claim every 10 minutes, so a credential attached on the console's credentials page
   is picked up without a restart. The heartbeat's
   `holding.unclaimed[].reason` carries the line, which the goal's
   waiting reason shows, and node.log says it once per goal per run:

```
accounts: org acme's project widgets has no claude-code credential open to this node; goal <goal id>'s items and plans left unclaimed until one is (asked again every 10m0s; said once for the goal)
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
credential id and its kind, input, output and cache tokens, tool calls
(the total, and every MCP call counted per tool as `tools: [{tool,
calls, errors, ms}]`), wall time, the list cost in cents (and the actual cost, on an API key),
and the plan's windows before and after. Every session's record is in
the node's log too, one `<label>: usage <kind> {…}` JSON line per
session, whether its finishing command carried it, refused it or — a
session that ends in a failure — never sent it (see [Usage](#usage)).

### accounts.json

`~/.mesh/accounts.json` is every account a claim-time read has issued
this node lately — label, kind, platform id, adapter, caps, the
priorities it serves and its own pace point, never the secret — so the
loop can schedule on issued accounts across a restart, before the first
claim reads them again. One row per label:

```json
{
  "accounts": [
    {
      "label": "max20",
      "kind": "subscription",
      "id": "cred-123",
      "issuedAt": "2026-10-04T12:00:00Z",
      "projectIds": ["proj-1"]
    },
    {
      "label": "Temp - Subsciprtion",
      "kind": "subscription",
      "id": "cred-456",
      "issuedAt": "2026-10-03T03:00:00Z",
      "withdrawnAt": "2026-10-03T03:54:00Z",
      "projectIds": ["proj-1"]
    }
  ]
}
```

`issuedAt` is when the row was last noted (a session was given this
credential); past `KnownFor` (24h) of it the row is dropped on the next
save. `projectIds` are the projects whose claim-time reads have issued
the label (empty for a row written before project scoping, read as
every project). **`withdrawnAt`** is set the moment a claim-time read
for the label's adapter and one of its `projectIds` answers without
naming it — a credential deleted or disabled on the platform, or one no
longer attached to that project — rather than waiting out the 24 hours:
the row stays for the ledger, but the account is left out of
`Accounts(now)` and so out of the heartbeat's `holding.accounts` and
`limits[]` until it is issued again, which clears `withdrawnAt` (adr-005's
amendment 2026-10-03, a withdrawn account leaves the report). The 24-hour
prune is still the only deletion.

The platform's own credential read is preferred: every tick, before
the heartbeat, the loop reads each served org's `credentialIndexs` and
withdraws a known label whose row is `disabled` or `deleted`, whose
`allowedNodeIds` name other nodes but not this one, or that no row
names any more — logged `accounts: <label> withdrawn (platform:
<why>)`, gone from that very heartbeat. A platform that refuses the
read is said once (`credentials: the platform does not serve
credentialIndexs to this node; withdrawals come from the claim-time
read`) and the claim-time inference above is the only source; a
failed read withdraws nothing that tick (adr-005's amendment
2026-10-03, item 2).

### Who owns a node

A node has an owner — a person, an enterprise or an org
(`internal/node/owned.go`, `internal/commands/nodecmd.go`,
[ADR-015](docs/adr/adr-015-a-node-registers-once-with-an-owner.md)).
node.yaml's `owner:` says which: empty or `person` (the default) is
**bring your own infra** — the node serves every org the person is a
member of, on the person's own accounts and each org's tools, and
leaves an org when the person does, exactly as it always has.
`owner: enterprise` is shared infra — the node serves every org the
enterprise has, on the enterprise's own credentials, and an org
outside the enterprise only by an explicit share from an enterprise
admin.

`owner: org` is one company's own box — the node serves that org and
no other, whoever operates the machine: an org admin can own a node for
their org, and no other org lists it "via membership". It
names the org beside it, by slug (or login, tenant slug) or id:

```yaml
owner: org
org: grapevine
```

`LoadConfig` refuses `owner: org` with no `org:`, and `org:` with any
other owner (unless an older single-org registration wrote it beside
`account_id:`). The registration needs no person: on a machine
credential it never calls `signIn`. The first start resolves the slug
to the org's id from the machine token's orgs read (`myOrgs`, the read
the node already makes for the orgs it serves; `node.FindOrg`) — a slug
the token does not reach refuses the start naming the token's orgs —
and caches it in node.yaml as `org_id:` beside `node_id:` and
`node_id_source:`; a later start registers on the cached id without the
read. It sends `ownerKind: org`, `ownerId` the org's id, `orgId` the
same, and `credentialId` the machine id, under the node id
`NodeOwnerID("org", orgId, slug)` the platform derives. The start line
reads `node mini (runsheet-mini) owned by Grapevine (org), serving
grapevine`. A gateway whose `ownerKind` enum lacks `org`
(`TakesOwnerOrg`, before runsheet's a-node-is-owned-by-an-org) refuses
the start with one line, before any `registerNode`: `owner: org needs a
gateway that takes ownerKind org (runsheet with
a-node-is-owned-by-an-org); this gateway does not`.

`owner: enterprise` names no enterprise by itself: node.yaml also needs
`enterprise:`, the enterprise's slug (as the console shows it) or its
id, beside it:

```yaml
owner: enterprise
enterprise: anythink
```

`LoadConfig` refuses the one without the other: `owner: enterprise`
with no `enterprise:` is refused naming the fix (`` add `enterprise:
<slug or id>` ``), and `enterprise:` set with `owner:` empty or
`person` is refused too. `mesh node register --owner enterprise
--enterprise <slug or id>` writes both at once.

The first owner registration resolves a slug to the enterprise's id
through `myEnterprises` — the token must be an admin's of that
enterprise; a slug it does not administer refuses the start, naming
the slug and the token's enterprises — and caches it in node.yaml as
`enterprise_id:`, beside `node_id:`, so a later start on a token that
cannot list enterprises still registers. The id is what the
registration sends as `ownerId`; `owner_id:` (the person) is left as it
is. Naming another enterprise in `enterprise:` means deleting
`enterprise_id:` (`mesh node register --enterprise` does it for you).

The node registers once with its owner and takes the orgs it serves
from the platform's answer to that registration and to every
heartbeat since, never by re-reading the person's orgs. An org
attached to the owner later, or a hand-over from a person to an
enterprise, is picked up on the next heartbeat, no restart — logged
`owner changed: Chris → anythink (enterprise)`. `orgs:` in node.yaml still
restricts which of the answer's orgs the node serves; a name it does
not meet is logged, never fatal, and a changed served set is logged
too (`orgs a, b → a, b, c`).

`shares:` in node.yaml divides this node's capacity into a floor per
served org for a person's node — equal shares when it is left out
([ADR-023](docs/adr/adr-023-every-org-gets-its-share.md), Floors and
Every org gets its share above):

```yaml
shares:
  acme: 3     # weight, a positive integer — floors by capacity × weight / total
  globex: 1
```

A weight is keyed by the org's login, or its slug when it has none, as
`orgs:` names it; zero or negative is refused at load naming the org
(`shares: acme: must be a positive integer`), and a weight naming an
org this node does not serve is logged once (`shares: ghost is not a
served org`) and ignored. Re-read every tick like the claim limits, so
an edit needs no restart; a change is logged
(`shares acme 1, globex 1 → acme 3, globex 1`). An enterprise's own
`shares` on the owner contract — a later item of the goal — takes it
over node.yaml's when both are present; node.yaml's is what a person's
own node, serving the orgs they belong to, sets by hand.

node.yaml records which registration the node made as `contract:`:
`contract: owner` (one node id, under `node_id:`, shared by every org
it serves) when the platform answered the owner registration, or
`contract: per-org` (legacy — one node id per org, from `(org,
slug)`) when it did not, because the platform's `RegisterNodeInput`
has no owner field under either spelling — `ownerKind`, the
platform's, or `owner`, a platform before it (`OwnerField`); the node
sends whichever the schema has, with `ownerId` the owner's id (the
enterprise's for an enterprise). `mesh node status` and the rollback reporter
(the self-update confirm path) heartbeat the same way the last
registration recorded: once, under the one id, on the owner contract;
per org on the legacy one. An org with no GitHub login — `grapevine`,
an org the platform derives rather than links to a login — is named
by its tenant slug throughout: in the log, in `mesh node status`, and
as the name to give `--org`.

node.log's first line after the start lines names the owner and the
orgs, on the owner contract:

```
node mini (runsheet-mini) owned by Chris, serving runsheet, wardroomid, grapevine
node mini (runsheet-mini) owned by anythink (enterprise), serving runsheet, wardroomid
node mini (runsheet-mini) owned by Grapevine (org), serving grapevine
```

or, on the legacy path:

```
node mini (runsheet-mini) per org, serving runsheet, wardroomid, grapevine
```

followed by the owner field the platform took — `owner contract: field
ownerKind`, `owner contract: field owner`, or that it takes neither and
the node registers per org.

The first registration answered on the owner path after a per-org one
retires the node's legacy per-org rows itself (`retireNode`), so an org
outside the owner — Grapevine, for the Mini — stops listing the node
the same hour. The ids are the node's own, each derived from `(org,
slug)` for every org the answer names and every org of the token's
person; the owned id is never one. One line each, after the start line:

```
retired legacy node 0d6c…-… (org grapevine)
legacy node 9a1e…-… (org runsheet): no such node
```

The second shape is a refusal — already retired, never registered —
and is not fatal. A restart already on the owner contract, a nil answer
and the per-org path retire nothing.

`mesh node status` says the same, beside the org's nodes, followed by
the node id and how it was arrived at — `derived` (`OwnedNodeID`
computed it), `adopted from the platform` (a refusal named it, see
above), or `recorded` for a `node_id` saved before `node_id_source`
existed:

```
owned by Chris (person); serving runsheet, wardroomid, grapevine
node id: 18a8b766-18f3-5e73-b4cf-efb91c01946a (adopted from the platform)
```

and `mesh node register` prints it too, on whichever contract the
platform answered:

```
Registered node mini (runsheet-mini) owned by Chris, serving runsheet, wardroomid, grapevine, capacity 1. Saved ~/.mesh/node.yaml.
Start it with: mesh node start
```

```
Registered node mini (runsheet-mini) in runsheet, wardroomid, capacity 1. Saved ~/.mesh/node.yaml.
Start it with: mesh node start
```

### Register

```bash
mesh node register --org acme --name "Chris's laptop" [--slug chris-mbp] [--capacity 2]
```

Signs in, finds the org among yours, probes whether the platform's
registration takes an owner (see Who owns a node above), and writes
`~/.mesh/node.yaml` with what it decided: on the owner contract, the
node's one id and `contract: owner`, from the platform's answer; on
the legacy path, the node id derived from `(org, slug)` — registering
twice converges on the same node — org login and id, slug and
`contract: per-org`. Either way node.yaml keeps the slug and the
capacity too. `mesh node status` heartbeats once and lists the org's nodes, above a
line naming the credential this machine runs on — the machine, its org
and operator, and when its access token is next refreshed, or the rs_*
console token it is still on — and a line for the node process on this
machine: running (its pid), draining, or none (see Drain below). While
a node runs here it prints the **quartermaster**'s findings — `findings:`
then one line per finding, `  <kind> <severity> <subject>: <detail>`,
critical first (`disk critical /home/mesh/.mesh/jobs: 4% free (…)`,
`scratch ok /tmp: reaped 3 dirs, 12.0K; 0 in use kept`, `dead_checkout
ok …: 1 dead found, 1 removed, …`, `version warn v1.0.0: … desired
v1.1.0, differing for 1h1m0s`), or `findings: none` — read from
`~/.mesh/node.findings`, which the node rewrites every ten minutes as it
checks its disk, reaps stale `/tmp` scratch and dead checkouts, and
compares its version with the desired one
([ADR-034](docs/adr/adr-034-the-quartermaster-keeps-the-node-fit.md)).
When its container runtime is latched broken — a `*sandbox.RuntimeError`
out of a session start, or a failed probe, and neither has cleared since —
it next prints one line, `runtime_broken since <RFC3339>: <reason>; the
node claims nothing until the runtime heals`, podman's own words for
`<reason>`; nothing while the runtime reads healthy or no node runs here.
It reads `~/.mesh/node.runtime`, written each tick beside `node.standing`
([ADR-034](docs/adr/adr-034-a-node-whose-runtime-is-broken-stops-claiming.md)).
It also prints a **Held goals** section — a table of
org, goal, kind (`plan` or `item`), since, until and why for every goal
the node holds off claiming for (a plan claim released by a repeating
refusal, or an item claim refused for budget, pause or approval), or
`Held goals: none.` with nothing held; it reads `~/.mesh/node.held`,
written each tick beside `node.scaling`. node.log says the same, as
each is first noted (`held: <org> goal <slug> (<kind>): <reason>`) and
as it is released (`held: <org> goal <slug> (<kind>) released`) — the
node's own account of what it did, the kind of line
[ADR-019](docs/adr/adr-019-the-node-writes-its-own-log.md) keeps in
node.log rather than sessions.log. After it comes the **shares** table
— `ORG FLOOR HELD READY`, one row per served org: the floor its share of
the capacity gives it, the builds, plans and research it holds here, and
its ready items as last listed (`-` when that read failed) — read from
`~/.mesh/node.shares`, written each pass, or `shares: none recorded`
when there is none; the heartbeat's holding report carries the same rows
as `holding.orgs`
([ADR-023](docs/adr/adr-023-every-org-gets-its-share.md), Reporting).
It also prints the shared validation cache's line — `validation cache:
75% hits (30/40), 1.2 GiB of 20.0 GiB`, the hit rate over every tree and
blob lookup the node's served-diff validations made (counts persisted
in `~/.mesh/node.cache`), the cache root's size on disk and node.yaml's
`validation_cache_max_bytes` — or `validation cache: no lookups yet`
(Main validations from the served diff, below).
Beside it is the cache's size line, from the node's hourly cache trim
(`~/.mesh/node.cachesize`): `cache: 4.2 GiB of 20.0 GiB
(/home/u/.cache/mesh/cache); measured 2026-10-17T18:00:00Z`, or `cache: size unknown (no reading
yet)` before the first trim (A host session's scratch, below).

### Start

```bash
mesh node start [--interval 30s] [--unattended] [--max-budget-usd 10]
```

Every interval the node heartbeats (`HeartbeatNode`), lists the `ready`
work items of every org it serves and claims up to its free capacity
(`ClaimWorkItem`; the aggregate refuses a second claimant, so two nodes
never run one item). A node serving several orgs divides its capacity
into a **floor** per org — equal shares by default, or by weight when
node.yaml's `shares:` (below) names one, the largest
remainder to the orgs in listed order (capacity 4 over two orgs is 2
and 2; capacity 5 is 3 and 2; capacity 1 is 1 and 0) — and claims first
for an org holding fewer sessions than its floor while it has ready
work, its best item first whatever another org's priority; the org's
builds, plans and research count as its sessions, evaluations and
rebases do not. A floor is not a ceiling: an org with nothing ready
leaves its slots to the rest, and once every org is at its floor (or
has nothing ready) the slots left go in one order across the orgs
([ADR-023](docs/adr/adr-023-every-org-gets-its-share.md)): by
their goal's priority — urgent, high, normal, low; a goal with none is
normal — then, within a priority, finish before you start: an item with
work already begun on a branch (an open pull request, an evaluation, a
rebase conflict or a rebase state other than none) goes before fresh
items while it has fewer than two failed evaluations (`attempts`, one
under the platform's cap of three); one at that bound falls back behind
the fresh items, as retries sorted before, so a poisoned item takes the
head of the queue at most twice and never blocks it. Within each class
the item ready longest (`readyAt`) goes first, then oldest seq; the sort
is stable, so full ties keep the order the orgs were listed in
([ADR-017](docs/adr/adr-017-finish-before-you-start.md)). Each org's goals are read once a tick to
join the priorities; if that read fails, the org's items count as normal.
Planning and evaluation follow the same order across the orgs: the goals
in planning are claimed (`ClaimPlan`) and planned urgent, high, normal,
low (empty is normal), and the items this node holds for evaluation are
evaluated by their goal's priority, then seq — on the same one goals read
per org per tick.
With node.yaml's `claim: {unmerged_per_repo: N}` set, a repo with N
items in `review` — pull requests open and waiting to merge — gets no
fresh claim until one merges, while its started work (rework, a rebase,
a re-evaluation) is still claimed and other repos are unaffected; left
out or 0 there is no limit, and it is re-read every tick (see Unmerged
work per repo below).
node.yaml's `takes:` names the kinds of session the node claims — left
out, every kind — and a kind it does not take is never listed or claimed
(see Which sessions a node takes below).
The attempt bound guards the class order within a priority; across
priorities the order is strict above the floors, so a steady stream of
urgent work keeps a low goal's items, plans and evaluations waiting —
but never another org's floor. A build taken for a floor is logged
`<org>: claimed item N "title" (below its floor of F, holding H)`.
Rebases follow it too (see The rebase session below).
Capacity and the orgs served are re-read at the top of every tick, so
neither needs a restart: `capacity` comes from `~/.mesh/node.yaml` as
it stands (a file that cannot be read is logged and the last good
capacity kept). The orgs served come, on the owner contract, from
every heartbeat's answer — an org the platform adds, or a hand-over
from a person to an enterprise, is served from the next heartbeat on,
restricted by node.yaml's `orgs` as it stands — or, on the legacy
path, from the platform's `MyOrgs`: an org seen for the first time is
registered in and served that tick, one no longer listed (or outside
`orgs`) is dropped. A change is logged (`capacity 3 → 6`, `orgs acme →
acme, go-apis`).

Each claimed item runs its own
`claude -p "<prompt>" --output-format stream-json --verbose --max-budget-usd <budget>`
in its job's checkout, `~/.mesh/jobs/<item id>/checkout` (a clone of
its own, branch `mesh/<goal-slug>-<seq>` from the base ref),
with the goal's runsheet context written under `.mesh/context/`
(excluded from git). The job's `job.json` names its unit, connection,
org, product, item and goal, written before anything is checked out
(ADR-039,
[`docs/adr/adr-039-a-session-works-in-its-own-job-directory.md`](docs/adr/adr-039-a-session-works-in-its-own-job-directory.md)).
Each job's checkout, evaluation (`eval/`) and rebase (`rebase/`) is a
git clone of its own inside `~/.mesh/jobs/<id>/`, cloned by `git clone`
over https on the job's token, which rides each command's environment
and is never on the command line or in a config file (`cloneCommand`;
the operator's `gh repo clone` only when no job token is minted). A
complete clone, not a partial one: an agent in its pod holds no token to
fetch a missing blob. A rework reuses its checkout; plan and research
checkouts are clones of their own too. Nothing is shared between jobs,
so there is no shared clone and no worktree registration to prune: the
sweep removes a finished job's directory, and the quartermaster's
dead-checkout check removes a dead checkout. A `.git/config.lock` older
than five minutes that no git process holds is cleared before a config
write. Every session installs
against its job's own pnpm store and npm cache,
`~/.mesh/jobs/<id>/store/pnpm` and `…/store/npm` (`jobSandbox`; ADR-039,
Nothing is shared between jobs) — see
[Several orgs on one node](#several-orgs-on-one-node). Every clone and
`git fetch` a job runs is under a per-repo lock, a process-wide mutex
plus an exclusive `flock` on the job's own `.locks/<owner>--<name>.lock`
(`lockRoot`), released before any checkout, so two sessions (or two node
processes) fetching into one clone at once never fail on `cannot lock
ref`; a fetch that still fails with `cannot lock
ref` or `unable to update local ref` — a git outside the lock's reach —
is retried up to three times with a short backoff and logged once
(ADR-023,
`docs/adr/adr-023-fetches-of-one-repo-are-serialised-on-the-node.md`). The budget is
the smallest of the goal's remaining budget, node.yaml's `budgets:` for
build and the item's model (see "Session budgets" above), and
`--max-budget-usd`. The node
reports progress every minute (`ReportWorkItemProgress` with the cents
since the last report), pushes the branch with your credentials and
`FinishWorkItem`s — or `FailWorkItem`s; a workspace that cannot be prepared releases the claim instead (`ReleaseWorkItem` with `workspace: <error>`, attempts unchanged, the item held off this node for ten minutes; docs/adr/adr-024-a-workspace-failure-releases-the-claim.md). `--once` runs one tick.
`--mode` is deprecated: every node runs one session per job, and the
flag accepts only `poll`, so a service unit installed with it still
starts; any other value refuses to start.

`--unattended` adds `--dangerously-skip-permissions` so a
session never waits on a permission prompt — only on a machine you trust
with that. On SIGTERM the node exits leaving its host `claude -p`
sessions running in their own units, for the next `mesh node start` to
adopt; a sandboxed session ends with it and its claimed
item is left for a retry. It never fails an item on its own shutdown.
SIGHUP does the same and execs the binary again at once.

### node.log: bounded and rotated

([ADR-019](docs/adr/adr-019-the-node-writes-its-own-log.md))

`mesh node start` writes its own log, `~/.mesh/node.log`, and rotates
it: at **10 MB** it becomes `node.log.1` (the older ones shift to
`node.log.2` … `node.log.5`, the sixth is deleted) and a fresh
`node.log` starts, so **5 rotated files are kept** and the node's log
never takes more than about 60 MB. A node.log already past the limit
when the node starts — one a service appended to for months — is
rotated at once, and no write takes it past the limit: a single line
larger than 10 MB is cut to it, and if the rotation fails the node drops
lines (and says so once on stderr) rather than grow the file. node.log holds the node's own reasoning: ticks, claims,
reloads, heartbeats, holds, and each session's one-line summaries
(start, end, `usage`, cost, what its end killed). Watch it with `tail -f
~/.mesh/node.log`; `mesh node usage` reads it and its rotated files.

What sessions say goes to **`~/.mesh/sessions.log`** instead, capped the
same way at **20 MB with 2 rotated files kept** (`sessions.log.1`,
`sessions.log.2`): the workspaces' git commands and their output, and
the agents' stderr. A node started from
a terminal also echoes both to it.

The service no longer points its output at node.log: the systemd unit
sends stdout and stderr to the journal (`StandardOutput=journal`; read
it with `journalctl --user -u io.projectmesh.mesh-node`) and the
launchd agent to `~/.mesh/node.crash.log`. Only what the node cannot
write itself lands there — a panic's stack, the `restarting: exec` line
(also in node.log). **A unit installed before this change keeps
systemd's `StandardOutput=append:~/.mesh/node.log` redirect until
`mesh node service install` is run again**: until then the node still
rotates node.log, but systemd's descriptor follows the renamed file and
a panic's stack lands in whichever `node.log.N` it has become.

**Measured on the mini, 2026-10-02 11:13Z, v0.2.222** (under real work,
four to eight sessions at a time; `scripts/node-reload-reading.sh`):
node.log grows about **240 KB an hour** (4,485,367 bytes in the 67,220 s
since it began at the 2026-10-01 16:33:14Z rotation), so it reaches the
10 MB cap about every 44 hours and the five kept files hold about nine
days. sessions.log grows far faster, about 9 MB an hour
(`sessions.log.1`'s 20,760,274 bytes in the 2 h 15 min after
`sessions.log.2` closed), so its 20 MB × 2 holds the last four to six
hours of what sessions said.

### The work sweep: finished jobs go

`~/.mesh/jobs` is swept by the platform's answer (ADR 008, The work
sweep; ADR-039), each job judged by its `job.json`: an item's job once
the item is merged, cancelled or superseded, or failed for over a week;
a plan's once its goal is not planning; a merge batch's or main
validation's six hours after it was opened; any job whose connection
the node's answer no longer names, once nothing in it runs; each job's
kept sessions past 14 days. A research job stays: its clones are
fetched over for the next research of the direction. Nothing a live or
adopted session runs in, nothing the platform cannot answer for, no
symlink; a directory with no `job.json` is left and logged. The node
runs a pass at start-up, after the scope sweep, and then hourly, logging
`sweep: removed <label> (<why>, <bytes>)` per removal (an item's job by
`<goal>/<seq>`) and a total; a removal that fails is logged and retried next
pass. To see what a pass would remove, or run one by hand:

```sh
mesh node sweep --dry-run   # print the lines and the total; remove nothing
mesh node sweep             # remove them and print the same lines
```

### Drain: take a machine out of service

A node's host `claude -p` sessions run in their own units and outlive a
restart — the next process adopts them (see [Upgrading a node without
ending sessions](#upgrading-a-node-without-ending-sessions), which is how
a new binary goes in). A sandboxed session still ends with the node. To take a machine out of service — or to restart a
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

Swapping the binary on the mini by a drain — for a node whose sessions
do not outlive it (anything else:
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

What survives a swap:

- **Open sessions are adopted** — any session whose unit had started
  (`<workdir>.session/state.json` names the node) is followed to its end
  by the new process, as above.
- **Unopened plan and research claims are re-opened.** A plan or research
  claim is written to `~/.mesh/jobs/.claims/<kind>-<subject>.json` the
  moment the platform grants it, before its credential read, context
  fetch or unit. A record with no session beside it is claimed again by
  the same node id (the platform lets its holder re-claim inside
  `PlanClaimTTL`) and run as a fresh session, logged once:
  `re-opening the plan claim of goal <slug> held since <t>` or
  `re-opening the research claim of <project>/<direction> held since <t>`.
  A refused re-claim, or a goal no longer planning, or a direction no
  longer live, removes the record and says why. The record goes once the
  session's end is handled.
- **Builds and evaluations are re-run from the platform's record.** An
  item the node holds (`claimed` or `running`) with no session left is
  run again from the start (the workspace keeps the branch), and an item
  in `evaluating` on this node is evaluated again on the next tick.

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

**The confirm window.** When an update is pending, the new process logs its
version and update state to `~/.mesh/node.log` **before** it tries to
register or heartbeat — a start that dies during registration is never
silent. The new process then keeps trying to heartbeat for a confirm window
(default 5 min, node.yaml `update.confirm_within`) to complete the update.
Each failed heartbeat is logged with the error and time remaining. If the
platform is slow (timeouts or high latency), the process will keep retrying
and confirm once the gateway responds. Only a failure to *run* (a panic, a
config it cannot read, an exit) counts as 'did not start'; `mesh node upgrade`
prints the confirm window and the log path to watch while the update settles.

**A node that dies names its death.** A panic anywhere in `mesh node
start` or in a goroutine the node starts is one line in `~/.mesh/node.log`,
`mesh node: fatal: <panic value> (version <v>)`, followed by its stack, and
the process exits 70 (systemd: `status=70/SOFTWARE`) — never Go's bare 2,
which systemd shows as `status=2/INVALIDARGUMENT`. The service restarts it
as on any exit (`Restart=always`, `KeepAlive`). `grep -n -m5 '^mesh node:
fatal:' ~/.mesh/node.log` finds every one.

**On the mini, by hand.** The mini ordinarily updates from a release
cut on merge (see [Release](#release)); this is the fallback for a
build that is not a release. Build, copy, upgrade, and watch the
sessions carry on:

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
standings — reaches the mini on the next release: a node-changing merge
is tagged and signed on merge (see [Release](#release)), and the mini
takes it once `desiredVersion` is set on the console or by
`mesh node update <tag>` — with `nodes.autoUpdate` on, the platform
sets it itself once the mini is idle, with no person. Until then, every
session's report is only in the node's local log, a fallback line that
names the tokens and windows but tells the platform it keeps no ledger.
The by-hand `--binary` upgrade above is the fallback for a build that
is not a release, not the way to ship a contract change.

<!-- RECORDED MANUAL RUN: replace this block with the run on the mini —
     date, the versions from → to, the `mesh node status` and
     `systemctl --user list-units 'mesh-session-*'` output before and
     after, and the node.log lines of the adoption. -->
> **Recorded manual run on the mini:** _not yet recorded — to be filled
> in from the first `mesh node upgrade` there._

### Near the plan's limits

Every `claude -p` session — build, evaluation, plan, research and
rebase — reports its Claude plan's standing in a
`rate_limit_event`: a status (`allowed` while sessions may run), and how
much of the five-hour and the seven-day window is used, with when each
resets. The node keeps the latest reading per account (the issued
credential's label, else the session's `CLAUDE_CONFIG_DIR`, or `default`) and
paces each account by its own, so it slows down before the plan runs
out rather than failing items once it has. With several accounts, the
modes below are each account's, and every session goes to an account
that may take it (see [Credentials come from the platform](#credentials-come-from-the-platform)); the node as a whole is
paced or stopped — and logs it — only when every subscription is:

- **Paced** — the five-hour window at `limits.pace_five_hour` (80%) or
  past it: the node claims only items of urgent and high goals, and runs
  at most half its capacity (never less than one) at once; it plans only
  urgent and high goals and starts no research. Evaluations and rebases
  go on.
- **Stopped** — either window at `limits.stop` (98%) or past it, or a
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
  stop: 0.98
  reserve: 0.05
```

Every heartbeat carries the latest reading of each account as `limits`
— account, status, rate-limit type, `resetsAt`, overage status, the
`fiveHour` and `sevenDay` windows (utilization, `resetsAt`) and
`observedAt` — for the console to show (runsheet
`limits-on-the-console`); it is left out until a session has reported
one. A Claude account's row also carries `target`, `distance` and
`push` — its standing against the clock's time-proportional target
line — when the platform's schema takes them; a Codex account carries
none of the three.

The log says each crossing once, not every tick:

```
limits: paced until 2026-09-25T15:00:00Z: five-hour window at 85% (pace 80%) — only urgent and high goals are claimed and planned, at most 2 item(s) at once, and no research
limits: stopped until 2026-09-26T14:00:00Z: seven-day window at 99% (stop 98%) — nothing new is started; 1 session(s) to finish
limits: back to normal (was stopped)
```

and a change to the thresholds in node.yaml as `limits: pace five-hour
80% → 50%, stop 98% → 95%`.

Each subscription is paced against a clock too, not just the brake
above: a target line, the fraction of the window already elapsed less
a small reserve (`limits.reserve`, default 0.05), and how far the
account's own use stands from it — behind (room to spend) or ahead
(spent too fast). `mesh node status` prints it, one line per account
after the capacity line:

```
account max-mini: week 61% used, target 64% (3% behind), resets 2026-10-10T14:00:00Z; five-hour 42%/58% (16% behind)
account max-ci: week 88% used, target 64% (24% ahead), resets 2026-10-10T09:00:00Z; five-hour 70%/58% (12% ahead); push; paced
```

`push` marks an account still behind its line with a fifth or less of
the window left before reset — the scaler (below) gives it extra slots
so it spends down to its target instead of leaving the window's last
hours unused — and the trailing `paced`/`stopped` is the account's own
mode when its reading puts it there.

Spending every subscription down to its line at once would leave all
of them stopped together, with days left on their weekly windows and
nothing to claim but an API key: once every subscription but one is
within 10% of `stop`, the one with the latest reset is **held back**
for urgent work alone, so one subscription always has headroom. The
tick logs it in place of the ordinary no-headroom line:

```
accounts: max-ci held back for urgent work (the others within 10% of stop); 2 item(s) left unclaimed until 2026-10-10T14:00:00Z
```

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
   and capped by **free memory**: when it is 2 GB or less (CI runs on
   the same machine), or at or under `host.min_free_memory` when that is
   set, the node grows no further than the sessions it already runs. The machine's load and temperature are [the
   back-off's](#backing-off-a-hot-host), a layer over this target.

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
| `low memory` | the machine's free memory |
| `host hot: …` | [the host back-off](#backing-off-a-hot-host) |

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

A ranged node scales by what **all** the accounts the platform has
issued it afford together rather than by one plan's windows (adr-005,
capacity across accounts): each subscription by its windows, each api
key by its money left under its caps and the project's budget.

**The rule, every minute:**

- **A subscription** affords the smaller over its windows of (target −
  utilization) ÷ minutes to reset ÷ its burn per slot-minute — the
  targets 0.90 five-hour and 0.95 seven-day, the burn measured over the
  last 30 minutes of *its own* sessions. **The push:** in a window's
  last 20% of time, a subscription still behind its target line
  (elapsed fraction less `reserve`) affords (`stop` − utilization) ÷
  minutes to reset ÷ burn instead, and at least one slot more than it
  otherwise would, so it lands near full at reset — reason `push`,
  logged once as `capacity: pushing <label> (seven-day window 92%
  elapsed, 70% used, target 87%)`; a window at `stop` affords nothing.
- **An API key** affords its money left today ÷ hours left today ÷ its
  dollars per slot-hour (measured the same way; $10 before there is
  data). The money left is the least of the daily cap less today's
  spend, the monthly cap less this month's, and the project's daily
  budget less what the project's keys spent today. **The last 10% of a
  daily cap is for urgent and high work only**; a key at either cap
  affords nothing — the hard stop.
- **Capacity** is the sum of every account's slots, clamped to `[min,
  max]` and held at the sessions running when the machine is short of
  memory (a hot host is [the back-off's](#backing-off-a-hot-host));
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
| `low memory` | the machine's free memory |
| `host hot: …` | [the host back-off](#backing-off-a-hot-host) |
| `five-hour pace` / `weekly pace` | every account, by that window |
| `daily cap` / `monthly cap` / `project budget` | every account, by that money |
| `accounts` | the sum of several different bounds |

Each account's own slots (`Loop.AccountSlots`) are kept for the
heartbeat's per-account slots, which it carries once the platform
accepts them.

### Backing off a hot host

`host:` in `~/.mesh/node.yaml` sets the thresholds past which this
machine is judged hot — CI and other tenants can share it with the
node without either knowing:

```yaml
host:
  load_per_core_max: 1.5    # default; the 1-minute load average ÷ CPU count
  package_temp_max_c: 90    # default, °C; the package thermal zone, when there is one
  overload_per_core: 1.0    # default; past it a silent session is starved, not failed
  overload_cpu_some_avg60: 70  # optional, %; /proc/pressure/cpu's some avg60 (0 or absent: not judged)
  starve_after: 15m         # default; how long a build may say nothing on an overloaded host
  min_free_memory: 32g      # optional; 32g, 512m or 15% — free memory at or under it claims nothing (ADR-037)
```

Each left out, or set to 0, takes its default (`overload_cpu_some_avg60`
has none: zero turns that half of the check off). Read again every
tick like the rest of node.yaml, and a change logged once —
`host: load_per_core_max 1.5 → 2`. The node reads the package
temperature from the `x86_pkg_temp` or `TCPU` thermal zone under
`/sys/class/thermal` (Linux only; absent elsewhere, or with neither
zone, logged once rather than every tick).

**The back-off** (ADR-023, the node backs off a hot host) is a layer
over the plan figure — what the scaler or the accounts scaler steps
toward for a range, `N` for a fixed `capacity: N` — run **every tick**,
before claiming, not once a minute:

- **Hot** — the load per core past `load_per_core_max`, or the package
  temperature past `package_temp_max_c`: the capacity in force drops
  **one slot per tick down**, to `min` (never below 1; a fixed node's
  floor is 1), and **nothing new is claimed** while it is hot. Running
  sessions are never stopped.
- **Quiet** — both under: the capacity climbs **one slot per five quiet
  minutes up**, back to the plan figure and never above it. A hot tick
  starts the five minutes again.

Each step is logged once:

```
capacity 4 → 3: host hot: load 55/18 cores
capacity 3 → 4: host quiet 5m
```

While it holds the capacity the scaling reason is the host's (`host
hot: load 55/18 cores`, or `host hot: 101°C` for the temperature alone);
level again, it is the plan's. The scaling report (`Loop.Scaling`,
`~/.mesh/node.scaling`) carries the host as last read — `load`,
`cores`, `tempC` when a zone was read, and `hot` — and a fixed node
reports scaling while it is held back or hot, `fixed` again once level
and quiet. A host with no probe (not Linux) never backs off.

The heartbeat's `scaling` carries the same host reading (`host
{load, cores, temperatureC, hot}`) when the platform's schema takes it,
nil when there is none, so the console's Nodes page and
`mesh_health_nodes` can show why a node is running fewer sessions than
its plan-window figure affords. `mesh node status` shows it beside the
node's row and a `host:` line naming the thresholds in force:

```
host: load_per_core_max 1.5, package_temp_max_c 90
ORG        NAME  SLUG       STATUS  RUNNING  CAPACITY                                                  VERSION  LAST_SEEN
runsheet   mini  mini *     online  3        4/1–8 (host hot: load 55/18 cores) — host load 55 / 18 cores, 101°C  1.4.0   …
```

and once it has cooled:

```
ORG        NAME  SLUG       STATUS  RUNNING  CAPACITY                                     VERSION  LAST_SEEN
runsheet   mini  mini *     online  8        8/1–8 (max) — host load 9 / 18 cores, 70°C  1.4.0   …
```

— a host with no thermal zone to read shows `no temperature` in place
of a figure.

`scripts/host-backoff-reading.sh` takes the same reading by hand — the
load, cores, package temperature and `mesh node status`'s capacity line
— with `--interval S --count N` to repeat it, so an operator can watch
capacity fall under CI and climb back once the host is quiet. One run's
output is recorded, whatever its numbers, in
`docs/measurements/host-backoff-reading.md`.

### A starved session is released, not failed

`overload_per_core` and `overload_cpu_some_avg60` say when the host is
**overloaded** — too busy for a session to be blamed for going quiet
(`MachineReading.Overloaded`, its reason `host overloaded: load/core
1.44 (138.2 on 96 cores), cpu some avg60 71%`). A build whose stream
has said nothing for `starve_after` while the host reads overloaded is
stopped — its unit and every process in it killed, its pod closed, its
slot freed — and its item released with the note `stalled: host
overloaded: …`: a release, so no attempt is charged (ADR-029). node.log
says `item <label>: stalled on an overloaded host; released: <reason>`.
A silent session on a host that is not overloaded runs on as before.
When the platform stalls an item first (its 45-minute heartbeat
window) while the host is overloaded, the node's `claim lost (…)` line
names the overload too.

### A node low on disk claims nothing

`disk:` in `~/.mesh/node.yaml` sets the free space under which this
node claims nothing (ADR-035):

```yaml
disk:
  free_min_gb: 10           # default, GiB; free space under it claims nothing
  path: /home/chris/.mesh/jobs  # default: the jobs tree
```

Each left out, or `free_min_gb` 0, takes its default; a negative floor
is refused at start naming `disk.free_min_gb`. Read again every tick,
a change logged once — `disk: free_min_gb 10 → 20`. Every tick, before
any claim, the node reads the path's free space; under the floor it
claims nothing of **any** kind — builds, evaluations, rebases, plans,
research, merge batch candidates and main validations — while what
already runs runs on. It says so once as it crosses, at its first tick
when it starts under the floor:

```
disk_low: /home/chris/.mesh/jobs 3.0GiB free < floor 10.0GiB; claiming nothing
disk ok: /home/chris/.mesh/jobs 40.0GiB free >= floor 10.0GiB; claiming again
```

Free space and the next tick claims again; no restart is needed.

### A node short of memory claims nothing

`host.min_free_memory` in `~/.mesh/node.yaml` sets the memory available
(`/proc/meminfo`'s `MemAvailable`) at or under which this node claims
nothing (ADR-037) — a size, `32g`, `512m`, `1024k` or bytes, or a share
of the machine's memory, `15%`:

```yaml
host:
  min_free_memory: 32g
```

A zero, a percentage of 0 or 100 and more, or anything else unreadable is
refused at start naming `host.min_free_memory`. Read again every tick, a
change logged once — `host: min_free_memory (none) → 32g`. While it is
set, every tick before any claim the node reads its memory; at or under
the floor it claims nothing of **any** kind, as on a full disk, while
what already runs runs on, and says so once as it crosses:

```
memory_low: 20.0GiB free <= floor 32.0GiB; claiming nothing
memory ok: 100.0GiB free > floor 32.0GiB; claiming again
```

The ranged scalers take the same floor for their growth cap. Left out,
nothing changes: the scalers alone hold growth at 2 GB free and a fixed
`capacity:` is not affected by memory. A host whose sessions run in
large pods — dl360 runs twenty at up to 32g each on 251 GB — sets a
floor at least one pod's size, so a claim never starts a session the
memory cannot hold.

### Capacity counts live sessions

The capacity check counts the build sessions **alive on the host** —
active `mesh-session-build-*` scopes, else the session state files
under `~/.mesh/jobs` whose pid is alive — not only the ones the node tracks:
`free = capacity − max(running, live)`. An orphan this node's item no
longer claims is stopped by the periodic sweep and logged `capacity:
orphan: stopped …`; past capacity the node claims nothing and says
`capacity: <n> live agent sessions (<m> orphans) against capacity
<c>; claiming nothing` (ADR-029).

### Tools

A session gets the tools its project and role have, configured in the
console and handed to the node that claimed the session (ADR-006, trd
§3 Tools). The built-in context server, `runsheet`, is not one of
them: the node attaches it to every session itself (`mcp__runsheet__*`
in Claude Code, `[mcp_servers.runsheet]` for Codex), and a console tool
named `runsheet` is refused and noted. Its old names, `mesh-context` and
`prologue`, are aliases now: a console tool so named is an ordinary
tool, and the node reads either as the built-in only in the stream and
state of a session an older node started and this one adopted — that
session's `mcp__mesh-context__mesh_finish` is its finish, as
`mcp__runsheet__mesh_finish` is a new one's (ADR-006's amendment
2026-10-15). The node reads the platform's context under `/mesh/v1`
only; the `/prologue/v1` fallback is gone.

**runsheet goes through a relay.** The built-in is no http entry
with a bearer header: a session's token would rotate under it and
Claude Code never re-reads its MCP config, so the session would lose
its tools mid-build. It is a stdio relay, `mesh mcp-relay --url
<gateway>/mcp --token-file <path>` (in a pod, `node
runsheet-relay.js <url> <path>` from the session's HOME), which
reads the bearer from the file on every request. The file,
`runsheet.token`, is 0600 in the session's own directory
(`<workdir>.session/`, the pod's HOME, a Codex session's `CODEX_HOME`);
the node writes it from its own credential when the session starts,
rewrites it within 15 seconds of the credential renewing — a minute
before the old token expires — and removes it when the
session's end is handled. A node that adopts a session keeps the file
its `state.json` names — an older session's `mesh-context.token`
included (docs/adr/adr-029-a-sessions-mcp-token-is-refreshed-by-the-node.md).

**Every server goes through the session's proxy.** That relay and its
token file are now only a rebase's and an older node's. A build, evaluation, plan or research session reaches runsheet
and every console tool through its own proxy (`internal/sessionproxy`,
docs/adr/adr-038-a-session-reaches-every-mcp-server-through-one-proxy.md):
the node runs it on the host, serving `/mcp/<server>` on a 0600 unix
socket alone in a directory under its job's MCP dir
(`~/.mesh/jobs/<id>/mcp/px/<rand>/s`), and it holds every
credential — the session's runsheet bearer in memory, asked per
request; a remote tool's headers; a local tool's secret env, in the
`npx`/`uvx`/`docker` child the proxy itself starts on the host. The
agent's MCP config names only a shim that knows the socket and a server
name — `mesh mcp-relay --socket <s> --server <name>` on the host, `node
mcp-proxy-shim.js <s> <name>` in a pod — and a pod mounts the socket's
directory and nothing else of it: no `runsheet.token`, no
`MESH_TOOL_*`, no secret is in the agent's env, disk or config, so an
agent that runs `env` or greps `/` finds none. When a server fails, the
item's note names the proxy's own error — `tools: runsheet served,
never called: sessionproxy: runsheet: POST <url> answered 503` — not
the agent's bare `failed`. A session adopted after an upgrade keeps its
socket path: `state.json` names the socket and the server names, never
a secret, and the new node starts the proxy there again (`<label>:
proxy restarted on <socket> (<servers>)`). `scripts/e2e-sandbox.sh`
proves it in a real pod (`TestSessionProxySandboxedE2E`,
docs/e2e-sandbox.md).

**A build is a session's ordered steps.** `internal/session` gives a
build the shape of an Actions job (docs/adr/adr-038-a-session-runs-as-ordered-steps.md):
`Runner.RunItem` runs `checkout` (the workspace, no credential left in a
sandboxed one), `exchange` (the account, the session's bearer, its
tools), `pod` (the job container and its services), `run` (the harness,
its end read), `validate`, `finish` and `release` — always run, letting
go of what the others held — in that order, the first failure skipping
the rest but `release`. A step that hands the item back (released,
failed, finished already done) ends the job without failing it. node.log
carries one line per step once the build ends:

```
item ship-it/1: step 1 checkout: succeeded (95ms)
item ship-it/1: step 4 run: succeeded (12m3s)
item ship-it/1: step 6 finish: succeeded (1.2s)
```

The finish is the node's, outside the pod. When the platform takes a
changeset (its `FinishWorkItemInput` has `changesetId`), the node writes
the checkout's tree, uploads only the objects the platform lacks,
records the changeset and finishes the item naming it — the platform
makes the branch (`item <label>: changeset <id> recorded …; finished
<branch>`). Otherwise it pushes on the item's token and finishes on the
head it pushed, as before.

**What the node reports.** Every tick the node looks on its `PATH` for
the tool runtimes it can start — `npx`, `uvx`, `docker` — and the agent
CLIs it has — `claude` (claude-code) and `codex` — and its heartbeat
carries them as `runtimes` and `adapters`, so one installed while the
node runs is reported without a restart.

**How tools reach the session.** At claim the node reads the session's
credentials and its `tools` (name, remote `url`/`headers` or local
`runtime`/`command`/`args`/`env`, `required`, `roles`) in one read, and
keeps the tools whose `roles` include the session's — `builder` for a
build, `evaluator` for an evaluation, `planner` for a plan, `director`
for research (`tools.RoleFor`); a rebase session keeps runsheet alone
for now. A plan or research session that cannot
start a required tool starts no agent: the plan's claim is released
with the reason, the research hands in an empty one naming it, and the
node leaves that goal or direction alone while its runtimes are
unchanged. Each agent gets them in its own format:

- **Claude Code**: `--mcp-config` with `mcpServers` — runsheet plus one
  entry per tool, `{type: http, url, headers}` remote or `{command, args,
  env}` local (`npx -y …`, `uvx …`, `docker run -i --rm -e KEY… image
  …`) — and `--allowedTools mcp__<name>__*` for each, so a print-mode
  session can call them.
- **Codex**: one `[mcp_servers.<name>]` table per tool in a 0600
  `config.toml` in the session's temporary `CODEX_HOME`, removed with it
  (`tools.WriteCodexConfig`, from `Job.Tools`).

**Secrets in the environment only — without a proxy.** On a session
the node does not proxy (above), a secret header or env value is
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
report on its finish. Unpriced tools and runsheet report nothing.
A platform without `recordToolUsage` gets none: the node says so once
per run, logs each call instead, and the item goes on.

**Every call is counted.** Priced or not, each MCP server's calls,
errors and wall time ride the session's usage report as `tools` (ADR-006,
amendment 2026-09-28), and the item says how the session used what it
was served: `tools: brave served, never called` for a tool it never
called, `tools: perplexity: first call errored: …` (redacted, 200
characters at most) for one whose calls failed — a build in its
progress notes, an evaluation in its verdict's notes; a plan or research
session logs it. A platform whose usage report takes no `tools` gets the
total alone: the node says so once per run and logs each session's list.

A platform that does not hand out `tools` yet costs nothing: sessions
run on runsheet alone, as before.

### Context budget

Every session gets the project's documents on disk under
`.mesh/context/`, and every turn re-reads them. node.yaml's `context:`
bounds what is written there, in bytes:

```yaml
context:
  bytes_max: 245760      # the default (240 KB): the total on disk
  doc_bytes_max: 61440   # the default (60 KB): the most one design document may be
```

A PRD, TRD or other design document past `doc_bytes_max`, or past what
is left of `bytes_max`, is listed in `INDEX.md` and fetched on demand
with `mesh_document` instead. `INDEX.md` itself is inside
`bytes_max`: its length is reserved before any document is placed, so
the documents and the index together never pass it (the live goals and
items after the tables are not counted: their size is known only after
the fetch). The index is two tables — `Documents a session reads`
(instructions, ADRs, PRD, TRD, with a summary for those not on disk)
and `Other documents`, every other kind by kind, title and slug alone —
one row per document either way (ADR-015). A key left out takes its default; a
negative one is refused. Like the capacity and the accounts, `context:`
is read again every tick: a change is picked up on the next tick
without a restart, and logged
(`context: bytes_max 245760 → 131072, doc_bytes_max 61440 → 61440`).

### Usage

Every session logs its usage record to node.log when it ends, on its
label with its kind, whatever became of the report — carried by its
finishing command, refused, or never sent — as the platform took it:

```
item <goal>/<seq>: usage build {"sessionId":…,"inputTokens":…,"cacheReadTokens":…,"listCostCents":…,"contextBytes":…}
goal <slug>: usage plan {…}
project <slug>: usage research {…}
```

The kinds are build, evaluate, rebase, plan and research (a second
opinion, or a plan asked twice, is in the one record its finish
carried). `mesh node usage` reads those lines back into a reading of a
window:

```bash
mesh node usage --from 2026-09-28 --to 2026-09-29 [--log ~/.mesh/node.log] [--goal <slug>] [--kind build]
```

```
window 2026-09-28..2026-09-29  records 2026-09-28T09:10:00..2026-09-28T14:00:01
kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool
build     2         200000               200000             90.0          1000                  1                   4                         1
evaluate  1         50000                50000              80.0          -                     0                   -                         0
plan      2         50000                50000              60.0          3000                  2                   -                         0
unknown   1         7000                 7000               100.0         -                     0                   -                         0
unparsed 1
```

`--to` is exclusive and both dates are local, as node.log's timestamps
are. Input tokens are the report's `inputTokens`, cache reads and
writes among them; the context-bytes median is over the sessions that
carry `contextBytes` (`with context bytes`), `-` when none does; the
documents-fetched median is over the sessions that fetched any document
by tool (`mesh_document` or `mesh_search`, the record's
`documentsFetched`), and `fetched by tool` is how many did (ADR-016,
amendment 2026-10-02).
`--goal` keeps the records whose label names the goal — its items' and
its plan's. An older node's unlabelled `usage: {…}` line takes the kind
and label of the nearest `finished …($x)`, `evaluation …($x)` or
`rebased …($x)` line within 12 lines whose `$x` is its `listCostCents`,
else `unknown`; a record whose JSON does not parse is counted as
`unparsed` and skipped (ADR-016, amendment 2026-09-29).

An evaluate record also carries `priority` (the goal's priority when the
evaluation ran) and `evaluators` (1 or 2, whether a second opinion ran);
both are logged, never sent to the gateway, and zero on a record from
before this change. The reading adds, under the kind table, one row per
priority present whenever the window holds any evaluate record:

```
evaluate urgent: 2 items, 1×1 / 1×2, median $0.80/item, total $1.60
evaluate high: 2 items, 1×1 / 2×2, median $1.15/item, total $2.30
evaluate normal: 3 items, 2×1 / 1×2, median $0.34/item, total $1.54
evaluate low: 1 items, 1×1 / 0×2, median $0.20/item, total $0.20
evaluate unknown: 2 items, 0×1 / 0×2, median $0.80/item, total $1.60
```

Rows are ordered urgent, high, normal, low, then `unknown` for records
logged before this change. Items are distinct items; `×1` and `×2`
count the evaluations run on one evaluator and on two; the median is of
cost per item (a retried item's evaluations summed into one) and the
total of every evaluation, at list cost (adr-016, amendment 2026-10-02,
goal one-evaluator-for-ordinary-work).
`scripts/evaluation-cost-by-priority.sh --since <date>` (or `--from
<date> --to <date>`, and `[--log <path>]`) runs `mesh node usage --kind
evaluate` and prints just these rows, so the goal's before and after
readings on the mini are one command each.

### Unmerged work per repo

A repo whose pull requests cannot land fast enough should not get more
of them. node.yaml's `claim:` block sets the most a repo may have open
and waiting to merge before the node claims no fresh item of it:

```yaml
claim:
  unmerged_per_repo: 7   # no default: left out or 0 is no limit
```

Unmerged is an item in status `review` on the platform: its pull
request is open and waiting for the integrator. While the key is set the
node lists every served org's `review` items once a tick (when anything
is ready, and then every org's, even one with nothing ready: a repo's
open pull requests may sit under another org) and counts them by repo; a fresh item of a
repo at the limit stays `ready`, and each fresh item claimed in the tick
counts against its repo at once, so one tick never overshoots. Started
work in that repo — an item with an open pull request, an evaluation or
a rebase conflict — is still claimed: it finishes a pull request that
already counts. Other repos are claimed as before. The hold is logged
once per repo per tick and the heartbeat's holding report names it
(`repo <id> has 7 unmerged pull requests, at node.yaml's
claim.unmerged_per_repo 7`), each held repo of a goal named in its
entry. A failed `review` listing in any served org holds every fresh item
for the tick, as at the limit (any repo may have review items there),
and still claims started work: an unknown count is never taken as
zero. A negative value is refused. Like the
context budget, `claim:` is read again every tick without a restart,
and a change is logged (`claim: unmerged_per_repo 0 → 7`). With the key
left out no `review` listing is read at all
([ADR-017](docs/adr/adr-017-finish-before-you-start.md)).

### Which sessions a node takes

A node claims every kind of session — build, evaluate, rebase, plan,
research — unless node.yaml's `takes:` names the ones it takes, so one
machine can be the evaluator and another never evaluate
([ADR-025](docs/adr/adr-025-a-node-says-which-sessions-it-takes.md)):

```yaml
takes: [build, rebase, plan, research]   # the AI node: never evaluates
# or, on the CI machine:
takes: {evaluate: 3}                     # evaluates only, at most three at once
```

Left out, every kind is taken. A kind not in the known five is refused
when node.yaml is loaded, naming it, and `takes: []` is refused too.
A kind the node does not take is skipped before anything is listed or
asked of the platform: no claim is made and released, and its listing
(the ready items, the goals in planning, the evaluating items, the
requested rebases, the directions) is not read. Like `claim:`, `takes:`
is read again every tick without a restart, and a change is logged once
at the tick it takes effect (`takes: every kind → evaluate`).

A number beside a kind caps it: the node runs at most that many
sessions of the kind at once, inside its capacity, and a kind named
with no number (or in the list form) is uncapped:

```yaml
capacity: 4
takes: {build: 4, evaluate: 2}   # four builds, but never more than two evaluations at once
```

The cap counts what runs here and what the tick has started, so one
tick never overshoots it; a build's cap lowers the free slots before
any claim, and a cap on a kind capacity does not bound (evaluations,
rebases, plans, research) holds that kind alone. Work the cap holds
back stays where it is — ready, in evaluating, in planning — and is
named on the heartbeat's holding report (`at node.yaml's takes cap of
2 evaluation(s)`) and in node.log once a tick (`takes: evaluate at its
cap of 2; 3 waiting`).

A machine that takes `evaluate` runs the repo's tests and its
`.runsheet/validate.yml` suites, so it needs what a build does short of
writing code: the repos' toolchains (Go, Node, …), podman or docker for
their services, `git` and the `claude` (or `codex`) CLI. It needs no
GitHub login or Claude account of its own: the item's token and the
session's credentials are issued by the platform at claim.

#### An evaluate-only machine

A node with `takes: [evaluate]` evaluates what other nodes built: for
every item in `evaluating` that another node holds, it claims the
evaluation with `claimWorkItemEvaluation` before the session starts —
refused, another node has it, and that is the ordinary answer — and an
item it holds itself it evaluates with no claim, as before. A platform
whose schema has no `claimWorkItemEvaluation` leaves the node to its own
items, said once: `the platform takes no evaluation claim; evaluating
only what this node holds`.

The evaluator checks out the item's branch, runs its acceptance and the
`.runsheet/validate.yml` suites the change touches, so the machine needs:

- the toolchains the project's validate suites run — Go, Node, Python,
  whatever each repo's `.runsheet/validate.yml` names;
- docker (or podman) when a validate suite or a declared service needs it;
- `claude`, and optionally `codex` for evaluations on the Codex family;
- `mesh node login`, for the machine credential in `~/.mesh/token`.

No `gh` login and no Claude account of its own: the item's GitHub token
and the session's Claude (or Codex) credential come from the platform at
claim time (ADR-005, ADR-022). On bob, the CI slot limit
(`~/bin/ci-slot.sh`, `~/.ci-slots/limit`) is honoured by the validate
suites themselves, which take a slot as CI does; the node knows nothing
of it, and `takes: {evaluate: N}` bounds only how many evaluations it
runs at once.

`mesh node status` prints a `Takes: …` line right after `This machine:`
— `Takes: every kind`, or `Takes: evaluate (at most 3)` — and the
heartbeat's holding summary opens with `takes <list>; ` and carries the
same kinds as `holding.kinds` once the platform's schema reads it, so
the console can say a kind no online node takes is why a goal waits.

### A failed check resumes its session

A failing validation step, or a retry claimed after a failed
evaluation, resumes the build session that made the item — same
adapter, credential and checkout, its transcript kept under
`~/.mesh/jobs/<item id>/resume/<item id>/` until the work sweep removes
the item's job — with one message naming what
failed (ADR-030). Beside the resume record the node keeps
`record.jsonl`: one line per note, question, answer, failed check,
hand-back and turn, appended as it happens, never rewritten, and never
holding a token or env value. When the session cannot be resumed, the
fresh one gets the brief, then `# What earlier sessions did` — the
record, oldest first — and `# What the branch holds` — the commits and
diff stat past base — then the failure. The record is given whole up to
`RecordBudget` (6,000 characters); past it the oldest whole entries are
dropped, the first turn entry kept, never an entry shortened. With no
local record (another node built it) it is read from the platform's
activity rows. node.log says `fresh session with the item's record (N
entries, M commits)` or `fresh session with no record (<why>)`.

### Attestations: the head it pushed

After the last push of a build, a rework or a rebase — the push on the
build's own write token, above — the node attests
exactly the sha it pushed — validated again first when the push landed
another commit than the one `.runsheet/validate.yml` ran on — with that
commit's tree hash, and only then reports the session finished, so the
item is still this node's while the platform signs. A refusal `<sha> is
not the head of <branch> (GitHub has X)` is answered once: X fetched,
validated and attested. A 429, a 5xx or a transport error is retried
for two minutes (waits 2s, 5s, 10s, 20s, 30s, the last repeated); a
refusal is not retried, and an attestation never fails an item.
node.log says `attestation recorded <sha> <suites> <status>` or
`attestation refused <reason>`, the health line ends `attestations
recorded <n> refused <m> (1h)`, and `mesh node status` prints
`attestations: <n> recorded, <m> refused (last hour)`. The gateway's
half is runsheet goal `an-attestation-is-about-a-commit-not-an-item-state`:
until it is deployed every upload is refused, which the counts show.
ADR-014 (its 2026-10-09 amendment) has the detail.

### Main validations from the served diff

A merge batch candidate or a merged item's merge commit is validated
without a clone: the node reads the commit and the compare
`base...head` from GitHub's Git Data API on the minted token, writes the
merge base out of its one **shared validation cache** —
`<user cache dir>/mesh/cache` (`~/.cache/mesh/cache` on Linux), blobs,
trees and commits by sha, filled from the validating job's own clone
when it has one and holds the object (`WithClone`), never another job's,
and from GitHub otherwise — into a scratch dir of its own
under its job's `.scratch`, applies the diff's files through the
cache, checks what is on disk against the commit's tree, and runs
`.runsheet/validate.yml` there, its suites chosen by the compare's
paths. No clone is made and nothing is fetched. The step's TMPDIR,
GOTMPDIR and GOCACHE point into the scratch and the node's Go build
cache; a sandboxed run mounts that scratch read-write, the Go cache
read-write and the cache's `blobs/` read-only, nothing else of the
cache. The scratch is removed on every path, so a validation leaves
nothing under `/tmp` or its job but an empty `.scratch`.

The cache is held under node.yaml's `validation_cache_max_bytes`
(default 20 GiB, the Go build cache counted): the least recently used
entries go first, logged `cache: evicted <n> least recently used
entries (<size>) to hold the cap of <cap>; <size> now`, then `go clean
-cache`; still over after both is the finding `cache over cap: <size> of
<cap>`, logged once until it is back under. A diff past GitHub's
300-file cap, a truncated tree, no minted token or a materialised tree
that does not match the commit's falls back to a checkout of
[ADR-032](docs/adr/adr-032-a-node-validates-a-merge-batch-candidate.md),
a clone of its own in the batch job (`<job>/candidate-<n>`,
`<item>-main/main-<n>`; `addDetachedClone`), said once per run per
reason (`served diff: <reason>; validating from a worktree`). A merged
item's request runs on the operator's credentials today, with no minted
token to read the diff on, so it takes that clone (`served diff: no
minted token`) until the platform mints one for it. An evaluation still runs on the item's own checkout (ADR-027).
`mesh node status` prints the hit rate (above).
[ADR-034](docs/adr/adr-034-a-node-validates-from-a-served-diff-with-a-shared-cache.md)
has the detail.

### A host session's scratch

An unsandboxed session or validation never writes into the node's own
`/tmp`. Each host build, evaluation, plan, rebase and research session,
and each host validation, runs with a scratch dir of its own under its
job's `.scratch` (`~/.mesh/jobs/<id>/.scratch`) as its TMPDIR and GOTMPDIR, so whatever its
`go test`, `mktemp` and go-build leave goes with it. They all share one
Go build cache, `~/.cache/mesh/cache/go`, as GOCACHE. The scratch is
removed when the session or validation ends, by whichever node handles
the end, adopting or giving up included.

The cache root, the Go build cache included, is held under node.yaml's
`validation_cache_max_bytes` (default 20 GiB):

```yaml
validation_cache_max_bytes: 21474836480   # 20 GiB, the default
```

At the first tick and hourly after, a root over the cap is trimmed below
80% of it, logged `cache: trimmed <before> to <after>`, and the size
lands in `mesh node status`'s `cache:` line (above).

A node killed mid-session leaves that session's scratch behind. At the
next `mesh node start` the scratch sweep removes every dir under the
scratch root and the node's own `mesh-session-*`, `mesh-gh-*`,
`mesh-claude-*`, `mesh-node-*` (and, on a host node, `mesh-home-*`) dirs
under `/tmp`. It keeps whatever an adopted session still names, another
user's dirs and symlinks, and logs `scratch: swept N dirs, M bytes at
start (K kept)`. `scripts/host-tmp-reading.sh [--baseline FILE]
<user>...` reads what the node users hold under `/tmp` by prefix, to
compare an hour apart.
[ADR-036](docs/adr/adr-036-a-host-sessions-scratch-dies-with-the-session.md)
has the detail.

### Credentials on fetch, push and pull

Every git command the node runs for an item goes out on the item's own
token as the platform (the broker) issues it at that moment, never on
what the checkout's helper holds by then: the validator's fetches — the
diff's base and a newer head — take `ItemCred.ForFetch`, the build's
push `ItemCred.ForPush` (renewed near expiry, re-issued once on a
refusal), each run under `OnItemToken`, the token by `GIT_CONFIG_COUNT`
at the command's scope and never in a clone's config (ADR-001's
and ADR-039's). The sandbox image is pulled on the platform's registry
credential (Node image, above).

### Environment failures

A validation that fails on the node rather than on the code says so
with an **environment kind**, a closed set the platform reads to park
the item as an infrastructure hold instead of re-planning it (ADR-030;
the platform half is runsheet goal
`an-item-that-fails-on-its-environment-is-parked-not-replanned`):

| kind | detected by |
|------|-------------|
| `step_timeout` | a step's exit 124: killed at its timeout (the host runner and the pod alike) |
| `tool_missing` | a step's exit 127: the shell found no such command (`tsc` with no `node_modules`) |
| `credential_refused` | git or gh refused by GitHub on the item's token (`Repository not found`, a 401/403, `Write access to repository not granted`) |
| `image_pull_refused` | the registry refusing the sandbox image's pull: the node's own pull, or podman's exit 125 on a run that could not pull |

The kind rides the attestation's command as `environmentKind`
(`specs/contracts/recordValidationAttestation.json`) and opens every
reason the node sends with it — `failWorkItem`'s
(``tool_missing: validation failed: step check exited 127: command not
found: tsc (in `npm run check`)``),
`releaseWorkItem`'s (`credential_refused: github credential: …`) and a
self-update's `updateError`. node.log's `validation step <name> exited
<code> in <ms>ms` ends ` (<kind>)`. Any other non-zero exit is a code
failure and carries no kind.

A `tool_missing` reason names the missing tool, read from the shell's
own "not found" (sh, bash, or Go's `executable file not found in
$PATH`), and the step's command — ``command not found: <tool> (in
`<command>`)``, or the command alone when the output names no tool — on
a build's `failWorkItem` and a merge batch candidate's or main
validation's failure alike (ADR-030's amendment 2026-10-16).

### Reporting errors to Tripline

A node can send its failures to Tripline, a Sentry-protocol ingest
(`internal/tripline`, ADR-034). Give it a DSN in node.yaml, or in
`MESH_TRIPLINE_DSN`, which wins and is never saved:

```yaml
tripline:
  dsn: https://<key>@tripline.example/<project>
  enabled: true   # left out, a dsn is on; false turns it off
```

`mesh node start` logs `tripline reporting on (<host>)` or `tripline
reporting off`, and `mesh node status` prints `reporting: on (tripline
<host>)` or why it is off (`no tripline dsn`, `disabled in node.yaml`,
`bad tripline dsn`) — the host only, never the key.

Four classes are sent, each where node.log already names it:
`infrastructure` (an infrastructure or workspace release),
`harness_unavailable` (an agent CLI out of credit or rate-limited),
`sandbox_broken` and `session_failed` (a build's `session ended without
success`). Each event is tagged `node`, `item`, `seq`, `goal`, `repo`,
`role` and `class` and fingerprinted on the class and the message with
ids, paths, shas and numbers taken out, so the same failure on three
items is one issue counted three times. Every event passes a scrubber
first: tokens, keys, URL credentials, the node's bearer and home paths
never leave the machine.

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
session gets the environment but not the hook (the pod has no `mesh`).
`mesh tool-trim --max-kb N`
works by hand too: it trims stdin to stdout.

### Sandbox

Every session — a build, an evaluation, a plan, a research or a rebase
session — runs in its own rootless **Podman** pod
(mesh-specs ADR 007; `internal/sandbox/`), created when the session
starts and removed when it ends. Build, evaluate and rebase sessions
get their repository's services beside them in the pod (see
[Services](#services)). A Codex session — an evaluation, its preflight
and the second opinion — runs in its pod too, from the image's `codex`
(see [Codex on a node](#codex-on-a-node)).

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
- **Mounts.** Only what lies inside the session's own job (see
  [Several orgs on one node](#several-orgs-on-one-node)): the item's workspace, read-write at its own
  path, and a fresh temporary HOME under the job's `homes/` (removed
  after the session) holding
  the `--mcp-config` file, `CLAUDE_CONFIG_DIR` (`$HOME/.claude`), an
  empty `GH_CONFIG_DIR` (`$HOME/.config/gh`) and a `.gitconfig` with the
  operator's commit identity — and, for a Codex session, its
  `CODEX_HOME` at `$HOME/.codex` with the session's `config.toml` and
  the issued credential's `auth.json`. Never the operator's home,
  `~/.mesh`, a runner's directory or a container socket.
- **Environment.** Built from an allow-list, never inherited:
  `PATH`, `HOME`, `LANG`, `TZ`, `TERM`, `CLAUDE_CONFIG_DIR`,
  `MESH_ITEM`, `MESH_SESSION_KIND`, `DISABLE_AUTOUPDATER=1`, and of what the node gave the
  session: `CLAUDE_CODE_OAUTH_TOKEN` or
  `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `CODEX_API_KEY`, `CODEX_HOME`
  (moved under the session's HOME),
  `MESH_ACCOUNT` and the tool secrets `MESH_TOOL_*` — never `GH_TOKEN`,
  `GH_CONFIG_DIR` or another GitHub token (`HeldOutEnv`, ADR-038). Values reach
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
- **Cleanup.** A session's HOME, pod, containers and their volumes go
  when it ends, whichever way: `Session.Close` falls back to `podman
  unshare rm -rf` for what the agent left the node cannot remove, and
  logs `sandbox: session <label>: close: <err>` when even that fails.
  A node start sweeps what an earlier process left — every `mesh-home-*`
  directory no running container mounts, every mesh pod with no running
  container, every exited mesh container — and logs `sandbox: swept N
  homes, M pods, K containers at start (L homes in use)`.
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

Podman's own process keeps the node's `HOME`, `USER`, `LOGNAME`,
`PATH`, `XDG_*`, `CONTAINERS_*` and `TMPDIR` whatever a session's env
sets, so rootless podman always looks in the node user's image store;
the session's temporary `HOME` (and any other of those names it sets)
reaches the container as `--env HOME=<value>`, every other variable as
a bare `--env NAME` (ADR-007, Env by name). `TestLocalOnlyImageE2E`
checks it on a real pod: a session starts from an image only the local
store holds, under a registry name that cannot resolve, and podman
never tries to pull it.

### Several orgs on one node

A node may serve several orgs (an `owner: enterprise` or `owner:
person` registration, the orgs `orgs:` lets through), each by its own
connection (runsheet ADR 066: a tenant and a product; the node's answer
names each scope's `connectionId`). Their sessions share a machine and
nothing else: each unit of work has its own job directory — its
clones, package store and locks — and every session is held inside its
job (ADR-039,
[`docs/adr/adr-039-a-session-works-in-its-own-job-directory.md`](docs/adr/adr-039-a-session-works-in-its-own-job-directory.md);
ADR-035,
[`docs/adr/adr-035-a-session-reads-mesh-context-on-its-own-bearer.md`](docs/adr/adr-035-a-session-reads-mesh-context-on-its-own-bearer.md)).

**Jobs and connections.** No path carries an org's or a product's
name; every key is an id the platform gave. Every unit of work — an
item, a goal's plan, a direction's research, a merge batch's candidate,
a merged item's main validation (keyed `<item id>-main`) — has its own
`~/.mesh/jobs/<id>/`, with a `job.json` written before anything is
checked out that names its unit, connection, org, product and its
item, goal, direction or batch; the sweep drops a job whose
connection is gone. The node keeps nothing per connection:

| path | what |
|------|------|
| `jobs/<id>/checkout/` | an item's build checkout, a clone of its own |
| `jobs/<id>/eval/`, `…/rebase/` | the item's evaluation's and its rebase's own clones |
| `jobs/<id>/plan/`, `…/research/` | a plan's or research's full clones, under `repos/<name>` |
| `jobs/<id>/<what>-<attempt>/` | a batch candidate's (`candidate-<n>`) or main validation's (`main-<n>`) fallback checkout, a clone of its own |
| `jobs/<id>/<dir>.session` | each session's state, beside its checkout |
| `jobs/<id>/.scratch/` | host runs' TMPDIRs |
| `jobs/<id>/kept-sessions/` | kept streams |
| `jobs/<id>/resume/` | a build's resume record |
| `jobs/<id>/mcp/` | session token files (`mcp/sessions/<item>-<attempt>/`) and proxy sockets |
| `jobs/<id>/homes/` | sandboxed sessions' HOMEs |
| `jobs/<id>/store/{pnpm,npm}` | the job's package stores |
| `jobs/<id>/.locks/` | the job's per-repo locks |

Plan and research claim records are in `~/.mesh/jobs/.claims`, planner
refusals in `~/.mesh/jobs/.plans/refusals.json`. A job is never reopened
for another connection. A job clones its own code, a session installs
against its own job's store, and its kept streams, resume record and
token files land in its own job. An item, plan or research of an org
the node does not serve, or of a scope the platform named no connection
for, opens no job and is refused at its workspace. The node's sandbox
has no package store of its own. The
same layout serves one connection or many. A node does not read or
write `~/.mesh/work`, `~/.mesh/repos`, `~/.mesh/store`, `~/.mesh/resume`,
`~/.mesh/mcp`, `~/.mesh/orgs/` or `~/.mesh/connections`, and nothing is
migrated from them.

**The refusals.** A session's pod is confined to its job's directory
alone (`sandbox.Confinement`, one root): every mount — the checkout, the
HOME, the package stores, the hooks, a detached agent's stream dirs —
must resolve, symlinks followed, inside it, checked before any pod is
made. The
first that does not refuses the whole session with `sandbox: <what>
<path> is outside job <id>'s roots <roots>; refused`
(`ErrMountOutsideJob`) and nothing starts: another job's checkout, kept
sessions, clone, package store or MCP token file, or a symlink out of
the job. Build, evaluate, rebase,
merge-batch and main validations, **plans and research** all run in
their job's pod; a plan whose pod cannot start releases its plan claim
(no try counted) and a research hands in an empty one, each held off
the node for ten minutes. With no sandbox (`sandbox: off`, or no
podman) a node serving two or more orgs runs nothing on the host — the
node user can read every job — and says why once per run,
`sandbox_required: …`: a build is released (no attempt charged), a
rebase failed back to its builder, a plan's claim released and a
research handed in empty.

**Session bearers.** A build's runsheet token file holds the id token
of its own mesh session — opened by the node (`POST
/mesh/v1/sessions`, runsheet ADR 056) for the item's claim, narrowed by
`/mcp` to its project and refused once the claim ends — never the
node's machine bearer, which reads every served org's MCP data. The
file is 0600 in the pod's HOME (a host session's under its job's
`mcp/sessions/`), refreshed from that session alone and removed with
it. A platform without the route refuses a build on a node serving
several orgs (released, named, `… gives no session the machine
bearer`); a one-org node falls back to the machine bearer, said once.
Evaluations, rebases, plans and research still read on the machine
bearer until the platform opens sessions for them (ADR-035, What this
leaves).

`internal/node/two_orgs_sandboxed_e2e_test.go`
(`go test ./internal/node -run TwoOrgs`) runs it end to end on the
gatewaytest fake and the fake runtime: an enterprise owner's node
serving acme and beta builds one item of each at once, then plans and
researches for each, each org by its own connection; every session ran
in a pod, its mounts are its job's checkout, HOME and stores alone, its kept-sessions root and token
file its job's, a build's token its own mesh session's, and from inside
each session a mount of the other org's checkout, clone, kept sessions,
package store or MCP data is refused with no pod made.

### Node image

Every sandboxed session runs one image, versioned with the node:

```
ghcr.io/runsheet/mesh-node:<version>     # e.g. ghcr.io/runsheet/mesh-node:v0.4.0
```

It is built from [`images/node/Containerfile`](images/node/Containerfile),
which the binary embeds: Debian with `git`, Go at go.mod's version, Node
LTS with `npm` and corepack, Python 3 with `pip` and `uv`, `make` and a C
toolchain, a Postgres 17 server (`initdb`, `pg_ctl`, `psql`, linked into
`/usr/local/bin` so a session's own PATH finds them; no cluster is made,
a step starts its own) and Debian's headless `chromium` (amd64 and arm64
alike), `gh`, `nftables`, `ca-certificates`, and the `claude` and
`codex` CLIs, each at a version pinned in the file — what latchkey's
conformance step, the console's `npm run check` and runsheet's
integration shard need. `scripts/image-tools-check.sh` proves them
inside the image (`npm ci && npx tsc --version` in a fixture package, a
Postgres server answering `select 1`, `chromium --headless --version`),
run by `go test -tags e2e ./internal/sandbox -run TestImageToolsE2E`
on the image built from the embedded Containerfile. The agents do not
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
**The pull is on a registry credential from the platform**: the node asks
`nodeRegistryToken(registry)` for the image's registry (`ghcr.io`) and
pulls with `--authfile` on a temporary `auth.json`, removed once the
pull returns, never in an image or a build context. A platform without
the read, or with no credential for the registry, leaves the pull on
podman's own login (`podman login ghcr.io`, or `sandbox.image: build`)
as before. A pull the registry refuses is the update's `updateError`,
opening `image_pull_refused: ` (below). To
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
- **Sharing a repository is not a dependency; sharing a code file is,
  and the node adds it.** Each item names the `files` it expects to
  change. When two items of one repo name the same code file and neither
  depends on the other, the node's plan check (`sharedCodeFiles`) makes
  the later depend on the earlier, so the second starts from the first's
  merged work. **Sharing a document is not a dependency**: documents
  never chain, because one item writes them (below).
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

**One item writes the goal's shared documents.** These are the ADR, the
TRD section, AGENTS.md and README. The planner marks the writer
`"writes_shared_docs": true`: the last item, or one that depends on the
rest. With none marked, the node picks the highest-seq item nothing
depends on. The writer's brief lists every sibling by seq and title.
Every other item's brief says not to edit those documents. Instead it
leaves what it would have added as a `mesh_progress` note opening
`for the docs writer:`. When the writer is built, its prompt carries
those notes after the brief, under `## Notes to fold in`: one line per
sibling, `item <seq> (<title>): <note>`, with the sibling's pull
request. Each sibling's merged pull request description is the other
source.

The hand-in's `planned, N items` line in node.log ends with one of two
things: `K dependencies added by the plan check (3→2 internal/x.go; …)`,
or `none added by the plan check`. See
[ADR-025](docs/adr/adr-025-items-that-share-a-file-are-planned-in-a-line.md).

The case that made the rule: in goal `node-plans-from-the-truth`, item #29
waited on four items it did not use, so it ran only after all of them had
merged instead of alongside them.

### The goal's record in the repo

Status lives on the platform, not in git (ADR 003, decision 4). No item's
branch writes a goal status table: a build's branch carries only its own
work. A session reads live status from `.mesh/context/INDEX.md` or the
platform.

The runsheet context server is read-only over git, so a goal still writes itself into the
repo once, when it ends: `specs/goal-<goal-slug>.md` (frontmatter
`title: Goal — …`, `kind: plan`, `status: approved` when done /
`superseded` when cancelled, `summary:`, `parent:` the project's PRD when
runsheet lists one), with the statement, the acceptance and the final
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
- **A writable PGDATA**: a Postgres (`postgres`, `postgis`) service with
  no `PGDATA` or `POSTGRES_INITDB_ARGS` of its own, from either source,
  gets `PGDATA=/tmp/pgdata` and `POSTGRES_INITDB_ARGS=--no-sync` — under
  keep-id the image's `/var/lib/postgresql/data` belongs to the pod's
  user and the entrypoint cannot `chmod` it; a service that sets either
  variable keeps its own value.
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
on `PATH`, and an evaluation runs on Codex when an issued Codex
credential (an OpenAI credential the console opens to the project,
serving `codex`) has headroom for the goal's priority, else on Claude.
The node never runs a Codex session on the machine's own login: not
`~/.codex`, not `$CODEX_HOME`, and node.yaml names no Codex account
(ADR 011, amendment 2026-10-02). A `gpt-…` evaluate or agent model
pins Codex, a `claude-…` one Claude. An item gets a second opinion on
the other family by the rule below
(docs/adr/adr-011-evaluations-run-on-the-family-with-headroom.md).

**The node's family choice** (ADR-011): before an evaluation starts, the node
judges headroom across the issued Codex credentials and the Claude accounts and
prefers Codex when an issued Codex credential has headroom for the item's priority.
`mesh node start` says once where its Codex comes from: `codex: evaluations on
Codex from the node image (codex 0.157.0) when an issued Codex credential has
headroom, else on Claude; …` on a sandboxed node, `… from the host binary <path>
…` with the sandbox off, or `codex: no Codex evaluations: <why>` when neither
has it.
A planner may pin an item's evaluator to a family through the agent model;
a pinned family with no headroom waits (not claimed, not failed).

**One evaluator or two** (ADR-005's and ADR-011's amendments 2026-10-02): an
item gets a second opinion on the other family when its goal is urgent or high,
when it touches a protected path (the item's `protectedFiles`, or a changed file
matching its `protectedRulePaths`, the project's `protectedPaths` or the repo's
`.mesh/config.yml` `protected_paths:`), when its
first verdict is FAIL on a clause that is not a reading of the host, or when the
repo's `.mesh/config.yml` says `second_opinion: always` (default `rule`). Any
other item — a normal or low goal's, touching no protected path — gets one
verdict. After a first PASS a second FAIL fails it; after a first FAIL a second
FAIL fails it on both sets of failed clauses, and a second PASS passes it. The
notes and node.log say which, as `evaluators: one — normal/low goal, no protected
path` or `evaluators: two — <reason>`.

### Install the CLI

**A sandboxed node needs none**: its Codex sessions run in their pods
from the node image's `codex` (ADR-007's amendment 2026-10-17), the
preflight there too, and a host `codex` is never used. Install the CLI
on the host only for a node with `sandbox: off`.

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

### The sandbox on Ubuntu

With `sandbox: off` Codex runs on the host, and its
workspace-write sandbox is its own bundled bubblewrap. (On a sandboxed
node it runs in the session's pod and the probe below runs there, `node`
binding the socket in place of `mesh sandbox-probe`.) On Ubuntu with
`kernel.apparmor_restrict_unprivileged_userns = 1` (the default since
23.10) that bubblewrap cannot set up its network namespace and fails
with `bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted`.
Every command and write in the session is blocked — `npm test` never
runs — and the session still ends "successfully". Claude sessions run
in the node's Podman pod and never use bwrap.

**The network.** Workspace-write has the network off by default, and
then Codex's seccomp filter refuses `socket()` — loopback too — so no
`httptest` test, no `npm test` against a local server and no `gh` can
run. The node runs every Codex session with `-c
sandbox_workspace_write.network_access=true`: a command the evaluator
runs may reach the network, which is the price of tests that run (ADR
011, amendment 2026-09-29).

**The probe.** Before every Codex session the node's preflight
(`Codex.Preflight`) runs, after its context-server check, Codex's
sandbox on a command that binds a loopback socket — the node's own
hidden `mesh sandbox-probe` — in the session's own `CODEX_HOME`,
environment and workspace, under the session's own keys:

```bash
# codex-cli 0.157; exit 0, a 127.0.0.1 address and no `bwrap:` is a pass
codex sandbox -c sandbox_mode="workspace-write" \
  -c sandbox_workspace_write.network_access=true -- mesh sandbox-probe
```

The `-c sandbox_mode="workspace-write"` matters: left alone, `codex
sandbox` runs its command read-only, while the session runs under
`codex exec --sandbox workspace-write`; the probe checks the policy the
session will actually get. Without the `network_access` key the probe
exits 1 with `Error: listen tcp 127.0.0.1:0: socket: operation not
permitted`.

A `bwrap:` line on its stderr or a non-zero exit fails the preflight
with `sandbox: <the bwrap line, else the socket error>`. Then, as for any failed
preflight (ADR 011): an unpinned evaluation runs on Claude and says so
in node.log and its notes (`the Codex preflight failed: sandbox: …`), an
item pinned to a `gpt-…` model waits in evaluating (said once for the
item), and a Codex second opinion is `unavailable: preflight sandbox:
…`. Should a session get past the probe and still have every command
refused by bwrap — or every test it ran refused a socket (`socket:
operation not permitted`) — the stream says so (`Result.SandboxBroken`)
and its text is never read for a verdict: the same job runs on Claude,
and with no other family it is recorded as no verdict.

**What the mini has** (since 12:59Z on 2026-09-29, installed by
Chris): `/etc/apparmor.d/codex-bwrap`, a profile on the bubblewrap
Codex ships beside itself rather than on `codex`:

```
abi <abi/4.0>,
include <tunables/global>

profile codex-bwrap /home/chris/.local/share/codex/codex-resources/bwrap flags=(unconfined) {
  userns,

  include if exists <local/codex-bwrap>
}
```

Its `userns,` line lets that bwrap create its user namespace, so
`bwrap: loopback: Failed RTM_NEWADDR` is gone and commands run. It does
**not** give the sandbox loopback: that is the `network_access` key
above, without which `go test ./...` still dies on `listen tcp6
[::1]:0: socket: operation not permitted`. The path is in the
operator's home, so anything the operator's user runs could replace
that file and gain user namespaces — the same caution as option 1
below; re-check it after a Codex upgrade moves its resources.

**The fix** is one of these, as root. Run the probe as the node's user
afterwards; it should print the address it bound (`127.0.0.1:<port>`)
and exit 0.

1. *An AppArmor profile for Codex alone* (preferred). The bubblewrap is
   inside the `codex` binary, so the profile names that binary's real
   path (`readlink -f "$(command -v codex)"`). Put it somewhere only
   root can write — a profile on a path the node's user can write lets
   anything that user runs create user namespaces — and point node.yaml
   `codex.binary` at it:

   ```bash
   sudo install -m 0755 "$(readlink -f "$(command -v codex)")" /usr/local/bin/codex
   sudo install -m 0755 "$(dirname "$(readlink -f "$(command -v codex)")")/codex-code-mode-host" /usr/local/bin/
   sudo tee /etc/apparmor.d/codex >/dev/null <<'PROFILE'
   abi <abi/4.0>,
   include <tunables/global>

   profile codex /usr/local/bin/codex flags=(unconfined) {
     userns,
     include if exists <local/codex>
   }
   PROFILE
   sudo apparmor_parser -r /etc/apparmor.d/codex
   ```

   What it costs: `codex` (and what it execs) may create unprivileged
   user namespaces, which is what its sandbox is built from; every other
   program on the host stays restricted. Re-install after a Codex
   upgrade (the profile follows the path, not the file).

2. *Lift the restriction for the whole host*:

   ```bash
   sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
   echo 'kernel.apparmor_restrict_unprivileged_userns = 0' | sudo tee /etc/sysctl.d/60-mesh-codex-userns.conf
   ```

   What it costs: every unprivileged process on the machine may create
   user namespaces again, as before Ubuntu 23.10 — the kernel surface
   (namespaced networking, mounts) the restriction closed is open to
   anything a session or the operator runs, not just Codex. Prefer the
   profile on a shared machine.

Until one is in place the node keeps Codex evaluations off a host whose
sandbox cannot start; nothing is judged on a session that could not run
the acceptance.

### Credentials

Every Codex session runs on an OpenAI credential the platform issues for
it at claim time (`itemCredentials`): an admin adds it on the console's
Credentials page, serving `codex`, and attaches it to the project (see
[Credentials come from the platform](#credentials-come-from-the-platform)).
There are two kinds.

**A ChatGPT plan** (Plus, Pro, Business): its `auth.json`, pasted as the
secret. The node writes it into the session's own temporary
`CODEX_HOME/auth.json` (0600) and removes it with the home; a login
Codex refreshed during the session is kept under `<user cache
dir>/mesh/codex/logins/` for the next session on the same secret
version.

**An API key**: it reaches the session only in its environment, as
`CODEX_API_KEY` and `OPENAI_API_KEY`, never logged or written to the
workspace or the home. codex-cli 0.157.0's `exec` reads only
`CODEX_API_KEY` and gets a 401 with just `OPENAI_API_KEY`, so the node
sets both (docs/adr/adr-005-accounts-on-the-node.md).

With no issued Codex credential an unpinned evaluation runs on Claude,
and an item pinned to a `gpt-…` model waits in evaluating. A Codex job
the node did not place a home for is refused by the harness (`codex: no
issued credential on the session; refusing the operator's own login`):
the operator's own `CODEX_HOME`, `CODEX_API_KEY` and `OPENAI_API_KEY`
never reach a session, and every Codex evaluation's usage record carries
its `credentialId`.

Codex has no budget flag, so a job's `MaxBudgetUSD` is **not enforced**
for a Codex session. The cost is known only at the end, from the token
usage at list price. Use the session's timeout and the key's own caps
to bound it.

### node.yaml

The `codex:` block is **optional** and names no account: `codex.accounts`
is retired and refused, as `accounts:` is (see
[Credentials come from the platform](#credentials-come-from-the-platform)),
and the credentials are the console's. It keeps the binary and the model
map:

```yaml
codex:
  binary: codex                 # default: codex on PATH
  models:                       # the Codex model for each Claude tier
    opus: gpt-6-astra           # override — default for Opus is gpt-6-sol
    sonnet: gpt-6-sol           # override — default for Sonnet is gpt-6-luna
    haiku: gpt-6-luna           # default for Haiku
```

**Default model mappings:** when a planner or agent model names a bare tier
(`opus`, `sonnet`, `haiku`) for a Codex evaluator, the node maps it to
`gpt-6-sol` for Opus and `gpt-6-luna` for Sonnet and Haiku unless the
`models:` block overrides it. `gpt-6-astra` costs about three times what
`gpt-6-sol` does for one evaluation, so it never runs unless an item pins
it or `models:` names it. Full model ids (`gpt-6-astra`, `gpt-6-sol`,
`gpt-6-luna`) pass through unchanged.

`models` maps the Claude tiers to Codex models. A session asked for
`opus`, `sonnet` or `haiku` runs on that tier's Codex model, and so does
a full id of that family (`claude-opus-5` → `gpt-6-sol`). A tier left
out of `models` takes its default: `gpt-6-sol` for Opus, `gpt-6-luna`
for Sonnet and Haiku. An empty model, or one of no tier, runs on
Codex's own default.

The session's MCP servers (runsheet) are written to `config.toml` in
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

## Publishing a product's catalogue

A product gets its agents into Mesh by publishing its catalogue, and in no
other way (runsheet ADR 067). From the product's checkout, usually in CI on
a merge to main:

```bash
mesh product publish --dry-run     # print the bundle and the CLI's checks; sends nothing, needs no credential
MESH_PRODUCT_CLIENT_ID=… MESH_PRODUCT_CLIENT_SECRET=… mesh product publish --version v1.4.0
```

It reads:

- `.mesh/product.yml`: `product` (the slug), an optional `display_name`,
  `mcp`, `hooks` and `triggers`.
- `.mesh/jobs/<kind>.yml`: one kind of job each, with `harness`, `finish`,
  `access`, `needs`, `pods`.
- the manifest's `roles` directory (`.mesh/roles/` by default): one role
  each, a kind with a `trigger`, an `identity` and `powers`.

A kind's or role's `guidance:` names a markdown file in the checkout
(`.mesh/agents/review.md`); the CLI sends its text, at most 64 KiB. A kind
with `harness: none` needs none.

A kind's or role's `finish_schema:` names a JSON file in the checkout
(`.mesh/schemas/triage.json`): the product's JSON Schema for its part of
a session's result (runsheet ADR 067 decision 10), which the node's
`mesh_result` and Mesh's `finishJob` validate the result against. The CLI
sends its text as the bundle's `finish_schema`, at most 64 KiB; a path
outside the checkout, a file that is not a JSON object, or one that does
not resolve as a JSON Schema is refused before anything is sent, and
`--dry-run` prints it with the rest of the bundle. It is optional.

It signs in as the product's own Latchkey client (`client_credentials` at
`MESH_PRODUCT_ISSUER`, default `https://auth.runsheet.dev`) and sends
everything to `PUT /mesh/v1/products/<product>/catalogue`:

- Mesh checks the bundle whole: one bad kind or role refuses all of it,
  and the error names every refused field.
- A kind or role the bundle no longer names is retired.
- The first publish registers the product to the client; after that only
  that client publishes it.

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
`GoalIndex`, `ProjectIndex`, `DirectionIndex`, `NodeIndex`, `CredentialIndex` — is not
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

The same pull request carries the node mutations' rule fixture,
`internal/gateway/gatewaytest/testdata/node-mutations.json`: per
`registerNode`, `heartbeatNode` and `claimWorkItem` and per bearer kind
(a person's `rs_*` token or a machine credential), the input fields the
gateway requires and refuses. runsheet generates it
(`specs/contracts/node-mutations.json`, checked by its TestContracts)
and `go run ./internal/genfields -refresh-rules` rewrites the committed
copy. The `gatewaytest` fake refuses any of those inputs that breaks
the rule for its bearer, naming the field and the kind, so a field the
gateway starts to require fails `go test` here rather than refusing a
node in production (docs/adr/adr-026). A rule change arrives only
through that pull request: never hand-edit the copy to make a test pass.

## Release

Releases are cut on merge. Every merge to `main` that changes the node
— anything under `cmd/`, `internal/` or `images/node/`, or `go.mod` /
`go.sum` — is tagged by `.github/workflows/release-on-merge.yml`, and
the tag runs `.github/workflows/release-tag.yml` as below: signed
binaries, the node image and the mirror, then the console offers the
release. A merge of docs, specs, the README or `.github` alone cuts
none. `scripts/release-on-merge.sh <repo-dir> <sha> [--dry-run]`
decides, printing `tag=vX.Y.Z` or `no release: <why>`:

- the next tag is the patch after the highest `vX.Y.Z` on this repo or
  the mirror `runsheet/mesh` (tags with a `-` are test prereleases and
  never count; none at all is `v0.1.0`);
- it is the **minor** (patch 0) when the merged PR carries the label
  `release:minor`, or a `specs/goal-*.md` the merge touches has
  `release: minor` in its frontmatter;
- a commit already tagged `vX.Y.Z` is no release.

Run it by hand from Actions → *Release on merge* → *Run workflow*: with
`dry_run` (the default) it prints the next tag and pushes nothing.
`scripts/release-on-merge_test.sh` covers the cases.

The operator sets up two things once:

- the repository secret **`RELEASE_TAG_TOKEN`**: a fine-grained token
  with Contents: read and write on `runsheet/mesh-cli`. The workflow
  pushes the tag with it and fails naming it when it is unset. It cannot
  be `GITHUB_TOKEN`: a tag pushed with that starts no workflow, so
  `release-tag.yml` would never run. Whoever holds it can ship to nodes.
- the label: `gh label create release:minor -R runsheet/mesh-cli
  --description "release-on-merge bumps the minor version"`.

Nodes update themselves from the GitHub releases of the public mirror
`runsheet/mesh`: `release-tag.yml` cuts a release
from a tag on `runsheet/mesh-cli` (docs/adr/adr-002-signed-tag-releases.md),
and the same tag is published, identical, on the mirror — same tag,
title, notes and five assets, via `scripts/mirror-release.sh`. The
mirror step exchanges this run's OIDC identity (audience
`runsheet-rigger`) at the runsheet gateway for a contents:write token
on `runsheet/mesh` alone, against the binding the mirror carries,
`.runsheet/bindings/mesh-cli-release.yaml` (docs/adr/adr-015-workflow-tokens-exchanged-through-rigger.md).
`RELEASE_TOKEN` is only the transitional fallback, kept until the
runsheet half (goal `rigger-binds-a-workflow-to-a-scoped-github-token`)
answers the exchange, at which point the operator runs
`gh secret delete RELEASE_TOKEN --repo runsheet/mesh-cli` to retire it
— no PR needed; a cleanup PR removing the fallback code follows
separately. The step logs which path it took: `mirror token: exchanged
through rigger, …` or `mirror token: RELEASE_TOKEN fallback (…)`. A
re-run of the publish job after a mirror
failure keeps an existing release of the tag whose assets are identical
(`scripts/ensure-release.sh`) and deletes nothing. See the amendment "the public
mirror on runsheet/mesh" in ADR 002 for why and how it is checked.

**The fallback: a tag by hand.** When the workflow cannot run (no
`RELEASE_TAG_TOKEN`, a major version, a release of a commit it
skipped), tag the next number one above the latest on the mirror
(`gh release list -R runsheet/mesh`, `v0.2.x`) yourself:

```sh
git tag vX.Y.Z && git push origin vX.Y.Z
```

Any tag starting `v` runs `release-tag.yml`; a tag with a `-`
(`v0.4.0-test.1`) is published as a prerelease. The version is stamped
from the tag (`internal/version.Version`), so `mesh --version` and the
node's heartbeat report exactly the tag. `release-tag.yml` itself has
no manual trigger — tags only.

The release notes are the PRs merged since the previous plain
`vX.Y.Z` tag, grouped by goal slug — `scripts/release-notes.sh
<previous-tag> <tag>` reads each PR's title and branch (`gh pr view`)
and, for a `mesh/<slug>-<seq>` branch, prints a `### <slug>` section
with the goal doc's link and one bullet per PR in merge order; a PR
from any other branch lands under `### Other`, and a commit with no PR
under `### Commits`. The publish job writes them to `release/NOTES.md`
and `gh release create` publishes it with `--notes-file`.

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
gh release download "$TAG" -R runsheet/mesh \
  -p SHA256SUMS -p SHA256SUMS.sigstore.json -p mesh-linux-amd64
cosign verify-blob \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
```

There is no other release channel on `runsheet/mesh-cli`:
`cli-release.yml` (main builds, published unsigned to the old public
repo) was dropped when the repo moved to the runsheet org; its public
line continues, signed, as the `runsheet/mesh` mirror above.
`release.yml` is the Shipper's `workflow_dispatch` stub and publishes
nothing.

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
a sandboxed node too: its sessions' pods run on and are adopted).
systemd (`Restart=always`) or launchd
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
how long a new process keeps trying to confirm, from its start — default
5 minutes — so a platform blip does not by itself roll an upgrade back:
a registration or heartbeat that fails is logged with its error (`self-update:
confirming <to>: … retrying for <left> more`) and tried again. When the
window runs out the process logs `<to> did not confirm within <window>:
<the error>` and exits 1 for its service to start it again; on the third
such start `update.json` names it `'<to> did not confirm within
<window>: <the heartbeat error>'`.

```yaml
update:
  confirm_within: 5m   # how long a new process keeps trying to confirm; default 5m
```

**By hand — the fallback.** The ordinary way a node gets a new
version is the one above: a merge cuts a release and the platform asks
the node for it. Upgrading by hand is for when that path cannot run —
a platform that cannot reach the node, a build that is not a release,
a sandboxed node. `mesh node upgrade` installs a release (or a local build)
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

It verifies first and refuses anything not signed by `runsheet/mesh-cli`'s
`release-tag.yml` on `refs/tags/<version>` with issuer
`https://token.actions.githubusercontent.com`,
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

**The mirror carries the signed line from v0.2.188.** The node and
`mesh node update` fetch anonymously from `runsheet/mesh`, with no token
(ADR-002's amendment). `v0.2.188` is the first signed release there,
identical to the one on `runsheet/mesh-cli`. `mesh node update` with no
version now takes `v0.2.188` or later. On the mirror, `v0.2.1`–`v0.2.186`
are unsigned old-channel builds from before signed releases. They have
no Sigstore bundle signed by `runsheet/mesh-cli`'s `release-tag.yml`, so
the updater refuses them. `v0.2.187` exists on `runsheet/mesh-cli` only,
because its mirror step failed before #336. To check a release by hand,
download it and verify it from the directory
(`docs/releases-after-transfer.md` has the record):

```sh
TAG=v0.2.188
gh release download "$TAG" -R runsheet/mesh -D rel
mesh node update --verify-only --from-dir rel "$TAG"
```

## Known Limitations

- Live browser-login and work-item testing still depends on the deployed `mesh-portal` and `mesh-api` carrying the matching session-auth changes.
- `mesh node login`'s device flow needs the org's public `mesh-cli` client id; there is no built-in default, and the runsheet gateway accepts machine tokens only from its own rollout phase on.

## Contribution

Contributions and fixes are welcome.
