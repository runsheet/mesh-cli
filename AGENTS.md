# mesh-cli — the node

The operator tool for Mesh. `mesh node start` makes this machine a node of
the runsheet platform: it speaks to the gateway (`api.runsheet.dev/graphql`,
`/prologue/v1`, `/mcp`) on the credential in `~/.mesh/token` — the Latchkey
machine credential `mesh node login` registers for this computer, or the
operator's `rs_*` console token — claims work items, builds each in a fresh `claude -p`
session, evaluates it in a second, plans goals in a third. The platform
side (goals, items, nodes, the scheduler, the integrator) is
`runsheet/runsheet`; the design is `projectmesh-io/mesh-specs` (prd, trd,
adr-001, adr-002) and it is binding.

```sh
go vet ./... && go build ./cmd/mesh/ && go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh   # the mini runs linux/amd64
```

## How it is shaped

- `internal/harness/prompt.go`: the five prompts — `BuildPrompt`,
  `EvaluatePrompt`, `PlanPrompt`, `ResearchPrompt`, `RebasePrompt`. Their
  contract lines are load-bearing: the evaluator ends with
  `VERDICT: PASS|FAIL`, the planner ends with the JSON graph in a fenced
  block, the director ends with its proposals in one, the rebase session
  ends with `REBASED: <sha>` and `RESOLVED: <files>` or
  `REBASE FAILED: <why>`, the builder never pushes. Change wording
  freely; never the contract. (A later goal moves the guidance onto the
  platform as agent definitions.)
- `internal/latchkey/` + `internal/auth/token.go`: the machine
  credential — Latchkey's device flow, the rotating refresh grant, and
  the `~/.mesh/token` that holds either shape behind one `TokenSource`
  (latchkey `adr-machine-identity`).
- `internal/node/`: the loop (`runner`), the workspace and Prologue
  context (`workspace.go`: `FetchContext`, `RenderIndex`, the size budget),
  evaluate, plan and research sessions, the service install (`launchd` /
  `systemd --user`). `internal/gateway/client.go`: the platform client.
  `internal/channel/`: channel mode (one session per org, turns).
  **A draining node claims nothing** (`Loop.Drain`): see the drain
  below.
- **The director** (`internal/node/research.go`, runsheet trd §6c): a
  project carries **directions** — several, each its own aggregate with
  its own words, window, timezone, gap, ceiling and research claim
  (`Directions(org, project)` lists them; the id is
  `direction:<project>:<slug>`). On every tick the loop offers each live
  direction with room to spare — `ClaimDirectionResearch`; the platform
  refuses it outside the hours, inside the gap, when the direction is
  off or archived, or when another node holds it, so *a refusal is the
  ordinary answer and is not logged as an error*. A session that wins
  the claim reads the direction, the repos and the context and proposes
  goals; `RecordDirectionResearch` hands them in with the cost, and the
  platform decides how many become goals. A session whose output cannot
  be read hands in an empty research anyway — otherwise the claim holds
  the direction for 45 minutes and the night is lost. **Two ceilings
  hold and `RoomFor` takes the smaller**: the direction's own
  `maxLiveGoals` less the live goals it set (`Goal.DirectionID`), and
  the project's `maxLiveGoals` — its total over every direction, on the
  autonomy policy beside `replanCap` — less every live goal on it. A
  direction with no room is not offered at all. Each direction works in
  its own checkout, `ResearchDir(root, project, direction)`, because two
  of one project may be inside their hours at once.
- **The drain** (`Loop.Drain`, trd §3 Drain): `SIGUSR1` or `mesh node
  drain` (which signals the pid in `~/.mesh/node.pid`) stops every new
  start — items, plans, research, evaluations, rebases — and `Run` returns nil
  once no session of *any* kind is left (planners and evaluators do not
  count against capacity, so `Running()` is not the exit test); the
  process exits 0 and its service starts the new binary.
  `~/.mesh/node.draining` marks it for `mesh node status`, and every
  heartbeat says `draining`. `DrainAndWait(ctx)` is the drain a
  self-update blocks on; `Resume` undoes a drain that has not yet ended
  `Run`. The heartbeat's extras (`gateway.HeartbeatExtras`: `draining`,
  `updateError`, `limits` — the latest Claude plan limits per
  account — and `scaling` — a ranged node's `{capacity, min, max,
  reason, accounts}`, the capacity in force, node.yaml's range, why
  and each account's slots, sent only when the capacity is a range
  or the node has several accounts, and only when the gateway's
  schema has the field (introspected, kept an hour: adr-005
  §Heartbeat); both for runsheet goal
  `limits-on-the-console` — and `runtimes` and `adapters` — the tool
  runtimes (npx, uvx, docker) and agent CLIs (claude-code, codex)
  `Loop.LookPath` finds, probed every tick, for `tools-in-the-console`)
  are sent only when set,
  because they are fields the platform half adds. Capacity, the
  limits thresholds and the orgs need no drain: `Reload` re-reads them
  every tick.
- **Self-update** (`internal/node/selfupdate.go`, `internal/selfupdate/`,
  adr-002): after each heartbeat the loop reads the node's desired
  version (`GetDesiredVersion`; empty, the running one or `dev` does
  nothing). A different one is fetched and verified (the signed
  `SHA256SUMS` of the tag release) *before* the drain, then the node
  drains, swaps its resolved executable keeping `<path>.prev`, and `Run`
  returns `ErrRestartForUpdate` so the process exits 0 for its service
  to start the new binary. A refusal resumes, heartbeats `updateError`
  and waits an hour on that version. `mesh node start` counts starts in
  `~/.mesh/update.json` (`CheckStart`): the third unconfirmed one rolls
  back to `.prev`, reports it and exits 0; the first heartbeat confirms.
  The loop's `Update` is an interface; tests inject a fake.
  `mesh node update [version]` (`internal/commands/nodeupdate.go`) does
  it by hand on the same `ReleaseUpdater`: verify, drain the pidfile's
  node, swap; `--verify-only` verifies into a temp dir.
- **The pace** (`internal/node/limits.go`, trd §3 Pacing by the plan's
  limits): every session's `rate_limit_event` lands in `Loop.Limits`
  (latest per account — its `MESH_ACCOUNT` label, else
  `CLAUDE_CONFIG_DIR` or `default` — fed through `ObserveLimits` around
  each one-shot harness and the channel session's `OnRateLimit`), and
  `pace` judges each account: *paced* at `limits.pace_five_hour` of the
  five-hour window (urgent/high work only, no research), *stopped* at
  `limits.stop` of either window or a status not `allowed` (nothing on
  it) until the window's reset. Every start picks its account
  (`ChooseAccount`, below); the node's own mode (`nodePace`, the most
  lenient subscription) halves the capacity when paced, and a crossing
  is logged once. A Codex session's readings land under
  `codex:<account>` (`CodexLimitsAccount`, `ObserveLimitsAs`):
  `Limits.Readings` leaves them out, so they never pace, stop, choose
  or scale a Claude session; the heartbeat carries them.
- **The scale** (`internal/node/headroom.go`, `scaling.go`, adr-004):
  a node.yaml `capacity: {min, max}` runs the `Scaler` once a minute
  inside the tick (`Loop.scale`, by `Loop.Now`) on the highest
  utilization across accounts per window, stepping the capacity in
  force one slot per 5 minutes; a shrink only stops claiming, at min
  only urgent/high items are claimed, and Reload clamps into a new
  range at once. `Loop.Scaling()` is what the heartbeat's `scaling`
  carries, and `Loop.Scaled` leaves it in `~/.mesh/node.scaling` for
  `mesh node status` (`in-force/min–max (reason)`). A
  plain `capacity: N` runs none of it. The hard stops of the pace stay
  above it. A ranged node that lists `accounts:` runs the
  `AccountsScaler` instead (`capacity.go`, `spending.go`, adr-005
  capacity across accounts): each subscription by its own windows, each
  api key by its money left under `daily_cap`/`monthly_cap` and the
  project's `project_budgets`, from the spend ledger
  (`ledger.go`, `~/.mesh/ledger.json`, local time, fed by
  `ObserveSpend`); `Loop.spendable` narrows every choice — no key at
  its cap, the last 10% of a key's day for urgent/high, and for an item
  only an account with a slot free.
- **The rebase** (`internal/node/rebase.go`, runsheet ADR 008): an item
  whose pull request conflicts in a way the integrator could not
  resolve itself carries `rebase_state: requested`. Every tick the loop
  lists them in each served org (`RequestedRebases`), in goal priority
  then seq (`rebaseOrder`), and claims each with `ClaimWorkItemRebase`
  — a refusal is the ordinary answer, not an error. The session works
  in the item's own checkout on its token, rebases onto the base,
  re-runs the acceptance and pushes with `--force-with-lease`; the node
  reads the pushed head back (`Workspacer.PushedHead`) and hands it in
  with `FinishWorkItemRebase`, or `FailWorkItemRebase` with the reason
  for *anything* else — a held claim waits out the platform's
  15-minute timer. Its heartbeat and its cost are
  `ReportWorkItemProgress`. Like evaluations, rebases do not count
  against capacity. **The platform does not yet issue the item's token
  to the rebase holder** (runsheet `gateway/itemtoken.go` refuses an
  item in `review`; ADR-001 asks for it), so until it does every
  rebase fails at `github token:` and goes to its builder; the fake
  mirrors that unless a test sets `RebaseTokens`.
- **Accounts** (`internal/node/accounts.go`, adr-005): node.yaml's
  `accounts:` ({label, kind subscription|api, token_file|api_key_env})
  are the Claude accounts sessions run on; none is the operator's own
  login, unchanged. `OpenAccount` → `PrepareAccount` gives a session
  the secret in its env only, a temp `CLAUDE_CONFIG_DIR` removed by
  `Cleanup`, `Job.Unset` of the inherited auth variables and
  `Job.Secrets` to redact; `MESH_ACCOUNT` makes `Account` key limits by
  label. **Which account** (`internal/node/select.go`, adr-005): before
  every claim — item, plan, research, rebase — and every evaluation the
  loop calls `ChooseAccount` (subscriptions first, the most headroom;
  an api account only for urgent/high work or when every subscription
  is stopped and work waits) and hands the choice to the session on its
  context (`WithAccount` → `OpenAccount`). **No account with headroom
  means no claim**: the item stays ready, not failed or released, and
  the tick logs one `accounts: no account has headroom …` line.
  **The claim-time read** (`internal/node/credentials.go`, adr-005):
  every one-shot session opens its account through
  `Credentials.Open`, which asks the platform (`SessionCredentials`)
  for the claimed session's credentials and chooses among them by the
  loop's own rule (`withChooser`); an api key at its cap is never
  chosen, and none left releases a build. A platform without the read
  or issuing none leaves node.yaml, said once per run. At the end
  `SessionAccount.Finish` hands in the session's usage record
  (`RecordSessionUsage`: credential, tokens, cost, windows before and
  after), or logs it as one `usage: {…}` line on a platform without
  the ledger.
- **Codex** (`internal/harness/codex.go`, goal `codex-adapter`): the
  `codex` harness, `codex exec --json --sandbox workspace-write` in the
  workspace, the prompt on stdin, the stream read by
  `ParseCodexStream` (`docs/codex-events.md`). Each run gets a fresh
  temporary `CODEX_HOME`, removed after it: a plan account's
  `auth.json` copied in (and written back when Codex refreshed it), or
  an API key in the environment only (`CODEX_API_KEY` and
  `OPENAI_API_KEY`), plus the MCP servers as `config.toml`, secrets by
  variable name (mesh-specs ADR 006). node.yaml's `codex:` block
  (`CodexConfig`: binary, accounts, `models {opus, sonnet, haiku}`)
  and `CodexModel` map the Claude tiers to Codex models. Codex has no
  budget flag, so `MaxBudgetUSD` is not enforced.
- **The second opinion** (`internal/node/evaluate.go`,
  `docs/adr/adr-005-codex-second-opinion.md`): `Evaluator.Second` is
  the Codex harness (`codexSecondOpinion` in `nodecmd.go`: built when
  node.yaml has a `codex:` block and its binary is found, on its first
  account; else none and the node evaluates as before) and
  `SecondModel` is `CodexModel` of the evaluate model. An item of an
  `urgent` or `high` goal whose Claude verdict passes is evaluated again
  on it with the same prompt and workspace: both pass is passed, a
  Codex fail is failed; the notes carry both, `Claude (first
  evaluation): VERDICT …` and `Codex (second opinion): VERDICT …`,
  truncated together to 4000, and the cost is the sum — all in the one
  `EvaluateWorkItem`. A Codex session that errors, times out or reaches
  no verdict, or whose `codex:<account>` is paced or stopped, keeps the
  first verdict with `Codex second opinion unavailable: <reason>`. A
  first fail or a normal/low goal never runs Codex. Builders on Codex
  come later, behind a per-project setting.
- **Tools** (`internal/tools/`, ADR-006, trd §3 Tools): the
  claim-time read `gateway.Session` hands a session its credentials and
  its `tools` (`[]tools.Tool`, secrets decrypted for the claiming
  node); a platform without `tools` gets none and is asked for
  credentials alone from then on. `Loop.Runtimes()` is what the last
  probe found. Build and evaluate sessions get theirs
  (`SelectTools` by role and runtimes, `SessionTools.Apply`: Prologue
  plus each tool in `Job.MCPServers`, secret values in `Job.Env` and
  `Job.Secrets` only, the tools on `Job.Tools` for Codex); a
  nice-to-have tool the node cannot start is omitted and noted on the
  item, a required one releases the item and `Declines` keeps the loop
  off it while the runtimes are unchanged. Plan, research, rebase and
  channel sessions keep Prologue alone.
- Every session gets Prologue as an MCP server with the node's token and
  the project's context on disk under `.mesh/context/` with `INDEX.md`:
  the documents, then `## Live goals and items` — each live goal's items
  (seq, title, status, branch, PR) and the finished goals one line each,
  read per session through the gateway (`AddLive`, adr-003 decision 3).
  A failed read leaves the documents only, with a warning; `.mesh` is
  excluded from git, so none of it is ever committed.
- Costs: each session's `total_cost_usd` is reported to the platform per
  item (`ReportWorkItemProgress`, `EvaluateWorkItem`, `PlanGoal`).
  Priced tool calls are tool usage records: the harness counts each
  session's MCP calls per server (`Result.ToolCalls` — Claude's
  `tool_use` named `mcp__<server>__…`, Codex's completed
  `mcp_tool_call`), and after a build or evaluate session
  `SessionAccount.Finish` hands in one `RecordUsage`
  (`gateway.ToolUsage{ItemID, Kind, Tool, Calls, ListPriceCents}`,
  mutation `recordUsage`) per rendered tool with `pricePerCallCents > 0`
  that was called — calls × price; Prologue and unpriced tools are none.
  A platform without `recordUsage` is said once per run and each record
  logged instead; it never fails the item.
- **Models are per kind of session.** `Models` in `~/.mesh/node.yaml`
  (`default`, `build`, `evaluate`, `plan`, `channel`, `research`,
  `rebase`) or the matching
  `--model*` flags on `node start`; `mesh node model` shows and sets
  them. A kind with none takes `default`; no default leaves the CLI's
  own — except the planner, which without an explicit `plan` runs on
  the strongest configured (`Models.PlanModel`), else `opus`. The
  planner names a `model` per item by the rubric in `PlanPrompt`; the
  node drops one outside haiku/sonnet/opus/`claude-*` with a log line
  and hands the rest in on `PlannedItem.Model`. The value is an alias the CLI knows (`opus`, `sonnet`, `haiku`)
  or a full id (`claude-opus-5`). An item's build and rebase run on
  the item's own `model`, then its agent's, then node.yaml's `build`
  (or `default`) (`BuildModel`); the evaluator on the stronger of that
  and `sonnet` (`EvaluateModel`); node.yaml is the fallback. The model
  used is in node.log and on the item's events.

## Rules

- No third-party integrations in this repo (see CONTRIBUTING.md): no
  Discord, no email, no API keys. A session reaches GitHub only with its
  item's own token: a GitHub App installation token the platform mints
  per item (`issueItemRepoToken`), scoped to the item's one repo,
  contents write + pull requests read, an hour, renewed near expiry. The
  node puts it on the checkout's own git credential helper (clearing the
  operator's `gh auth git-credential`) and gives the session `GH_TOKEN`
  and an empty, per-session `GH_CONFIG_DIR`, so the operator's gh login
  is out of reach (adr-001-item-scoped-github-token). On a platform that
  does not know the mutation the node falls back to the operator's own
  `gh`/`git` credentials and logs that once per run (`TokenFallback`) —
  the rollout is not a flag day; a refused token fails the item instead.
  The planner's and the director's project-wide fetches stay on the
  operator's `gh` and `git`.
- A change to what the node reports or claims changes the platform's
  contract too; land the platform side first (a process reacts only to
  events after its deploy).
- Tests that render the context index or the prompts assert exact rows
  and lines; update them with the change, not around it.
- Commits carry no Co-Authored-By trailer in this repo.
- Deploying to the mini is by hand: build for linux/amd64, `scp` to
  `~/bin/mesh.new`, `mv` it over `~/bin/mesh` and `mesh node drain`:
  the node finishes its sessions, exits, and systemd starts the new
  binary. Never `systemctl --user restart` it while a `claude -p`
  session runs (a restart re-pushes running items; an unpushed build is
  lost).
