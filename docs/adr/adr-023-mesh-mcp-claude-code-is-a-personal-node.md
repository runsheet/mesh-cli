---
summary: `mesh mcp` is the client half of Mesh inside Claude Code — a stdio MCP server on the person's own credential that shares the channel's server, proxies the hosted reads and names every work tool from its first release; later items of the goal amend it.
status: accepted
---

# ADR-023: `mesh mcp` — Claude Code is a personal node

2026-10-03, goal `mesh-mcp-joins-an-item-from-claude-code` (item 1).
Numbered 023 beside `adr-023-the-planner-splits-a-broad-goal.md`, which
landed the same day; cite this one as "ADR-023, mesh mcp".

## Context

A person who wants to work one of a goal's items in their own Claude
Code session has no way in today: the node's sessions are `claude -p`
units it starts itself, and channel mode (`internal/channel`) is a
server the node's daemon serves to its own persistent session, pushing
events through Claude Code's experimental `claude/channel` capability
on the node's credential. The goal asks for the other direction: Claude
Code adds Mesh once (`claude mcp add mesh -- mesh mcp`) and, on the
person's own login, sees the goals, joins an item, works it in a
checkout made locally and reports through the same tools a node uses.

## Decision

1. **One stdio server, two users.** The JSON-RPC server, `Tool` and
   `ObjectSchema` move from `internal/channel/mcp.go` to
   `internal/meshmcp` (`server.go`); `internal/channel` keeps aliases.
   `Server.Channel` advertises `experimental.claude/channel` and is set
   by channel mode alone — `mesh mcp` is a plain tools server and
   never pushes.
2. **`mesh mcp` on the person's credential.** `internal/commands/mcpcmd.go`
   opens `platformClient` — `~/.mesh/token`, the Latchkey credential
   `mesh node login` saved (refreshed under the lock) or an `rs_*` token
   — and serves `meshmcp.NewServer` on stdin/stdout. Nothing else is
   written to stdout.
3. **The tool list is whole from the first release.** `mesh_goals`
   (the live goals of every org `myOrgs` names, and their open items,
   from the gateway's goal and item reads), `mesh_status` (the items
   this server holds), and `mesh_join`, `mesh_switch`, `mesh_progress`,
   `mesh_ask`, `mesh_finish`, `mesh_release`, which answer `not yet
   available` until the goal's later items fill them in, so Claude
   Code's tool list and a `/mesh` skill written against it do not
   change shape as they land.
4. **One personal node, registered once and reused.** Before `Serve`
   answers its first request, `internal/node/personal.go`'s
   `RegisterPersonal` registers `mesh mcp`'s own node — ADR-015's owner
   form (`Owner "person"`, sent only when the platform's
   `RegisterNodeInput` takes an owner field), `Kind "personal"` (sent
   only when it takes `kind`), `Capacity 1`, named `<hostname> (Claude
   Code)`. The first call's id is saved to `~/.mesh/personal-node.json`
   (0600); every later start reads it back and sends it as
   `RegisterNodeInput.NodeID`, so the platform updates that one row
   instead of registering a new node each time Claude Code starts the
   server. `mesh node status` names it: `personal node: <name> (<id>)`.
   A failed registration is logged to stderr and does not stop the
   server from serving its tools.

## `mesh_join`

Amended 2026-10-03 (item 3). `mesh_join(goal, item?)`
(`internal/meshmcp/join.go`, `Join`) resolves the goal by slug among
the orgs `myOrgs` names and the item by seq — the goal's first `ready`
item, lowest seq, when none is given — and claims it with
`claimWorkItem(itemId, <personal node>, "claude-code")`, or the hosted
`mesh_claim` when the platform serves it (Hosted path, below). A refused claim is the
tool's answer, `claim refused: <reason>`, not a transport error. Then
it does what channel mode's `readyChannelItem` does:

- **Where the checkout lands.** `~/.mesh/work/<goal>/<seq>` on branch
  `mesh/<goal>-<seq>` from the item's base ref (`node.WorkspaceSpec`,
  `node.BranchName`, `node.PrepareWithRetry`), unless the cwd rule
  applies.
- **The cwd rule.** When the server's working directory is inside a git
  checkout whose `origin` names the item's repo (`OriginNames`: an
  https, ssh or path URL ending in `<owner>/<name>`, `.git` or not, any
  case), that checkout's top is the workspace instead: fetched, the
  branch checked out there, the context written under its
  `.mesh/context`. Its `origin` URL is left as the person set it.
- **The credential rule.** The item's own token (`issueItemRepoToken`,
  `node.NewItemCred`) goes on the checkout's own credential helper for
  GitHub (`node.WriteCredentialHelper`: in `.git/config`, an empty
  helper first, then the token's) — never the person's global config.
  A platform without item tokens falls back to the person's own `gh`
  and `git`, said once (`node.TokenFallback`), as a node's does.
- **What it answers.** After `node.FetchContext` (the item's repo, its
  goal, the checkout) and `node.WriteContext`: JSON `{path, branch,
  brief, acceptance, context: [files written], item: {id, goal, seq,
  title}}`. The held item is recorded in `~/.mesh/mcp-state.json`
  (`{held: [{itemId, goal, seq, path, branch, base, claimedAt}]}`), which
  `mesh_status` lists. A workspace that cannot be made releases the
  claim (`releaseWorkItem`) and answers the error.

The personal node the claim names is node.yaml's `node_id` until the
personal node's registration (item 2) answers its own.

## Work tools

Amended 2026-10-03 (item 4). The rest of the list is served
(`internal/meshmcp/work.go`), each on the items `mcp-state.json` holds.
A tool's `item` names the held item — its id, its seq, or `goal/seq` —
and defaults to the one held item; with several held it is required.
`mesh mcp` commits nothing itself. Every refusal from the gateway, and
a push that fails, is the tool's answer `<tool> refused: <reason>`, not
a transport error, and leaves the item held — except a push git refuses
because the branch moved on origin, which is answered `push refused:`
(Pairing, below).

| Tool | What it does | Gateway call |
|---|---|---|
| `mesh_join(goal, item?, pair?)` | claims and prepares the checkout (above); `pair` joins a held item's branch with no claim (Pairing) | `claimWorkItem`, `issueItemRepoToken`, the context reads |
| `mesh_status()` | each held item's goal, seq, path, branch, base, `uncommitted` (`git status --porcelain` non-empty) and `commitsPastBase` (`git rev-list --count origin/<base>..HEAD`) | none: the state file and the checkouts |
| `mesh_progress(item?, note, costCents?)` | a progress note on the item | `reportWorkItemProgress` |
| `mesh_ask(item?, question)` | the question as a progress note opening `question: ` when the platform serves no hosted `mesh_ask` (Hosted path) | `reportWorkItemProgress` |
| `mesh_finish(item?, outcome?, evidence?)` | pushes the branch as committed (`node.Git.Push`, `-u --force-with-lease`), reads its head and finishes the item, then drops it from the state; `outcome: already_done` pushes nothing and sends the evidence | `finishWorkItem(itemId, branch, head, 0)`, or `finishWorkItem` with `outcome: already_done` |
| `mesh_release(item?, reason)` | gives the item back and drops it | `releaseWorkItem(itemId, nodeId, reason)` |
| `mesh_switch(goal, item?, note?, from?)` | leaves the held item (`from`, defaulted as `item` is): pushes its branch when it has commits past base, reports the note (default `switching to <goal>[/<seq>]`), releases it with reason `switched`, then joins the target — never the item it just left — and answers the join | `reportWorkItemProgress`, `releaseWorkItem`, then mesh_join's |

The tests (`internal/meshmcp/flow_test.go`) run join → work → finish and
join → switch → join against `gatewaytest.New` and a bare origin served
by `gatewaytest.GitHub`.

## Pairing

Amended 2026-10-03 (item 5). `mesh_join(goal, item?, pair: true)` (and
`mesh_switch`'s `pair`) joins an item another node holds — its status
`claimed`, `running`, `in_progress`, `evaluating` or `review` and its
`nodeId` not the personal node's (`heldByOther`) — as a second pair of
hands on the same branch:

- **Same branch.** The checkout is prepared exactly as a holder's
  (`node.PrepareWithRetry`): origin fetched and `mesh/<goal>-<seq>`
  checked out at its remote head when the holder has pushed it (from the
  base when not yet), the item token on the checkout's own helper via
  `issueItemRepoToken`. The platform refusing the pair that token — it
  hands an item's token only to its holder today — is the answer `pair
  refused: <reason>`; nothing is claimed or released.
- **No second claim.** No `claimWorkItem` is sent; the holder's claim
  stands, until the hosted `paired` field exists to record a pair on
  the item. The state record carries `paired: true`, which
  `mesh_status` shows and the join's answer carries. `pair` on an item
  nobody holds is an ordinary join: it is claimed. Because the claim is
  not the pair's, `mesh_release` and `mesh_switch` on a paired item drop
  it from the state and send no `releaseWorkItem` (`left <goal>/<seq>:
  <reason> (paired: its holder keeps the claim)`).
- **Lease semantics.** `mesh_finish` and `mesh_switch` push with
  `node.Git.Push`'s `-u --force-with-lease` as before: the lease is the
  checkout's own `origin/<branch>`, so a push over commits the checkout
  has not seen — the holder pushed first — is refused by git, never
  forced over them. `Push` tells that refusal (git's output naming
  `stale info` or `rejected`) from a transport failure by a typed error,
  `*node.PushRefusedError` (its message `git push --force-with-lease
  origin <branch> refused: the branch moved on origin: <git's words>`),
  and the tool answers `push refused: the branch moved on origin; run
  git pull --rebase in <path> and finish again` (`switch again` for
  `mesh_switch`): the item stays held and no `finishWorkItem` is sent.
  After the rebase the push fast-forwards and the finish goes through.
  A pair's `finishWorkItem` after the holder's lands on an item already
  `evaluating`; the platform takes it once the hosted `paired` field
  does (the fake's `PairedFinish`) and refuses it today, which is the
  answer `mesh_finish refused: …` with the branch pushed.

The tests (`internal/meshmcp/pair_test.go`) run holder and pair against
one `gatewaytest.New` and bare origin: the pair's join sends no claim
(`Fake.Claims`) and lands on the holder's head, a stale finish is
refused with origin unchanged, and the finish after `git pull --rebase`
is sent; `internal/node`'s `TestPushRefusedWhenTheRemoteMoved` checks
the typed error.

## Hosted path

Amended 2026-10-03 (item 6). The platform's `/mcp` — the URL
`node.MeshContextMCP` builds, Bearer the person's own token — is to
serve the work tools itself (runsheet goal
`mesh-work-tools-on-the-hosted-mcp-server`). `internal/meshmcp/hosted.go`
(`Hosted`) is an HTTP MCP client to it: `initialize`, then `tools/list`,
and whether all five of `mesh_claim`, `mesh_progress`, `mesh_ask`,
`mesh_finish` and `mesh_release` are named (`Hosted.Serves`). `mesh mcp`
asks at its start and a work tool asks again once the answer is an hour
old (`HostedProbeEvery`); a probe that fails is a no.

- **Served.** mesh_join's claim is `mesh_claim {goal, item, pair,
  node}` — the goal's slug, the item's seq, whether this join pairs with
  the item's holder (the platform records the pair and claims nothing),
  and the personal node, which the platform checks the bearer owns,
  since one person may run several. `mesh_progress {item, note}`,
  `mesh_ask {item, question}`, `mesh_finish {item, branch, headSha}`
  (after the push, as before; `{item, outcome: "already_done",
  evidence}` with no push), and `mesh_release {item, reason}` — mesh_switch's
  note and release too — name the item by its id and relay the tool's
  text as the answer (a finish's prefixed `pushed <branch> at <sha>; `).
  A result with `isError` is the refusal: `<tool> refused: <text>`,
  `claim refused: <text>`, or `pair refused: <text>` for a pair's.
  `specs/contracts/hostedworktools.json` records each tool's arguments
  and an example answer for the platform to build against;
  `hosted_test.go` holds the client to its keys.
- **Not served.** The GraphQL mutations of the sections above stay, and
  one line, `mesh mcp: the platform serves no hosted work tools; using
  claimWorkItem`, goes to stderr once per run (`HostedFallbackLine`) —
  the second fallback beside the item token's `TokenFallback`, so the
  rollout is not a flag day either.

`gatewaytest.Fake` serves `/mcp` (`mcp.go`): the context tools always,
and with `HostedWorkTools` the five, each applied as its mutation is
(`mesh_claim` → `claimWorkItem`, `mesh_ask` → a `question: ` progress
note) and recorded under its own name, never the mutation's.

## Skill

Amended 2026-10-03 (item 7). `skills/mesh/SKILL.md` ships with
mesh-cli: a Claude Code skill (frontmatter `name: mesh`, one-line
`description`) covering the five everyday forms —
`/mesh goals`, `/mesh join <goal> [seq] [--pair]`,
`/mesh switch <goal> [seq]`, `/mesh status` and `/mesh finish` — each
naming the `mesh_*` tool it calls, with what to tell the person: a
join or switch answers `path`, `brief`, `acceptance` and `context`, so
`cd` to `path`, read the brief and acceptance, and work on the branch
it checked out; a finish is committed first, then `mesh_finish`, and a
`push refused:` answer (Pairing, above) is relayed to the person
verbatim.

It is embedded, not read from disk at runtime: `skills/mesh/embed.go`
(package `meshskill`, colocated with the file, as `images/node`'s
`embed.go` carries the node image's Containerfile) holds
`//go:embed SKILL.md`, and `internal/meshmcp/skill.go`'s `Skill()
[]byte` returns it — `go:embed` cannot name a file outside its own
package's directory, so the embed lives beside the file it embeds
rather than beside its caller.

`mesh mcp install` (`internal/commands/mcpcmd.go`) writes `Skill()` to
`$CLAUDE_CONFIG_DIR/skills/mesh/SKILL.md` when that is set, else
`~/.claude/skills/mesh/SKILL.md`, making the directories, mode 0644; a
file already there that differs from `Skill()` is left alone unless
`--force`, and then overwritten. It prints the path written and the
`claude mcp add mesh -- mesh mcp` line, so installing the tool and the
skill is one two-line routine.

Amended 2026-10-05: `mesh mcp install` is now one table of clients —
Claude Code, Codex and Cursor — writing each one's MCP entry and
instructions; see ADR-025 (`adr-025-mesh-mcp-install-is-one-table-per-client.md`).
The skill is the one source every client's instructions render
from (ADR-025's item 3 amendment): Claude's is `Skill()` whole,
unchanged; Codex's prompt and AGENTS.md section and Cursor's user
skill, command and project rule are `mcpinstall.Render(client,
Skill())`, the client-specific lines — the add command, "from Claude
Code", every `/mesh <form>` mention, heading or inline — substituted
rather than copied, so the five everyday forms and the `mesh_*` tools
they name can never drift between clients.

README's "Work a goal from Claude Code, Codex or Cursor" section (the
first `## ` after the title, from item 1, renamed by item 3) walks
both lines, the checkout location, the personal node, pairing and the
`push refused` message with one example transcript line per `/mesh`
form.

## The server is named runsheet

Amended 2026-10-17, goal `the-node-calls-the-context-server-runsheet`.
`meshmcp.ServerName` (`internal/meshmcp/server.go`) is `"runsheet"`,
not `"mesh"`: the `name` its `initialize` response's `serverInfo`
carries, for `mesh mcp` and for channel mode alike, since both share
the one `Server`. `internal/mcpinstall.ServerName` follows it for the
entry a client's config keeps the server under (ADR-025's amendment of
the same date) — the two are read independently, so a platform that
renames one is not required to rename the other on the same day, but
today they agree. `channel.ServerName`, the unrelated
`--dangerously-load-development-channels server:mesh` flag, is not
derived from `meshmcp.ServerName` and stays `"mesh"`: it names Claude
Code's channel capability, a flag value, not the server's identity.

## Consequences

- Channel mode is unchanged on the wire but for its `serverInfo.name`
  (now `runsheet`, above): it still advertises the channel capability
  and its tools are the same.
- `mesh mcp` is the client half; mesh_join's checkout on the item
  token and pairing are later items of the goal, each amending this
  ADR (pairing: item 5, below).
- Usage of a `mesh mcp` session is the person's Claude Code's own; it
  goes to their personal subscription credential once the node is
  registered.

## Amendment 2026-10-27: no hosted claim, and the Mesh server is no person's

runsheet ADR 069 split the platform's MCP in two. `mesh_claim` is no MCP
tool any more, so `mesh_join` always claims with `claimWorkItem` (a
pair's join claims nothing), and `HostedWorkTools` is mesh_progress,
mesh_ask, mesh_finish and mesh_release, sent with the argument names
runsheet publishes (`job_id` for progress and release, `item_id` for ask
and finish; specs/contracts/hostedworktools.json). The job's work tools
live on the Mesh server (`/mesh/mcp`), which serves apps and sessions
alone: once runsheet's `/mcp` stops listing them, a person's hosted path
is not served and the mutations are used, as before the hosted tools.
A node's own session reaches the Mesh server as `mesh`, beside
`runsheet`, through its session proxy on its own DPoP-bound session
token (tools.Mesh, tools.WithMesh); a session on the operator's bearer
gets no `mesh` server, since `/mesh/mcp` refuses that bearer.
