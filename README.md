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
token a minute before it expires — by `mesh node run` and by the goal
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

2. run `mesh node register` again (or restart `mesh node run`, or
   `systemctl --user restart` / `launchctl kickstart -k` of its service,
   for a change the running process itself needs to pick up);
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

### What a session can reach

A `mesh node run` lease never holds a bare credential of its own: the
relay injects what the job offer carried, live only over the hosts the
job kind's egress allows, and routes a lease's git traffic to the code
host its brief names rather than the operator's own `gh`/`git` login
([ADR-045](docs/adr/adr-045-the-relay-terminates-tls-for-the-hosts-a-rule-names.md),
[ADR-046](docs/adr/adr-046-the-relay-injects-the-credentials-a-job-carried.md),
[ADR-047](docs/adr/adr-047-a-jobs-git-reaches-the-code-host-its-brief-names.md)).
AGENTS.md has the detail; this file does not restate it.

The legacy gateway client's own work tools (`mesh mcp`'s `mesh_join`,
`mesh_finish`, and `mesh goal plan`) still run on the operator's own
signed-in session and `gh`/`git` login, each scoped to the one item or
project it is working — see
[Work a goal from Claude Code, Codex or Cursor](#work-a-goal-from-claude-code-codex-or-cursor)
above.

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
Start it with: mesh node run
```

```
Registered node mini (runsheet-mini) in runsheet, wardroomid, capacity 1. Saved ~/.mesh/node.yaml.
Start it with: mesh node run
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
capacity too.

The node id and owner contract above are the legacy gateway client's
own — what `mesh mcp`'s `mesh_join` claims items under (see
[Who owns a node](#who-owns-a-node) above), not anything `mesh node
run` claims or reports on. `mesh node status` reports only the Mesh
v1 node running on this machine (see Node above); AGENTS.md has its
shape.

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

### How a lease gets them

A `mesh node run` lease's session and its services run in one pod of
the lease's own: the services are what the checkout's declaration
above resolves, plus the brief's own, each a sidecar health-checked
before the session starts. A required one that is not ready hands the
lease back `cannot_run` rather than starting a session without its
database; the node tools' suites (`run_suite`) run in the same pod,
beside the services. See ADR-044 and `internal/nodev1` for the detail.

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
[ADR-044](docs/adr/adr-044-a-v1-node-runs-each-lease-as-a-job-actor.md);
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
and restarts at once, as the platform's update does (see
[ADR-044](docs/adr/adr-044-a-v1-node-runs-each-lease-as-a-job-actor.md)).
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
verified release drains the node running here, as `mesh node drain`
does (with none running there is nothing to drain), swaps the executable keeping
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
