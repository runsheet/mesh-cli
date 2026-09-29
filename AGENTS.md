# mesh-cli — the node

The operator tool for Mesh. `mesh node start` makes this machine a node of
the runsheet platform: it speaks to the gateway (`api.runsheet.dev/graphql`,
`/mesh/v1`, `/mcp`) on the credential in `~/.mesh/token` — the Latchkey
machine credential `mesh node login` registers for this computer, or the
operator's `rs_*` console token — claims work items, builds each in a fresh `claude -p`
session, evaluates it in a second, plans goals in a third. The platform
side (goals, items, nodes, the scheduler, the integrator) is
`runsheet/runsheet`; the design is `runsheet/mesh-specs` (prd, trd,
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
  block, the director ends with its pitches and answers in one for a
  propose-mode direction, its proposals in one for a set-mode one, the rebase session
  ends with `REBASED: <sha>` and `RESOLVED: <files>` or
  `REBASE FAILED: <why>`, the builder never pushes and checks main
  first, ending with `ALREADY DONE: <evidence>` when the acceptance
  already holds there (`ParseAlreadyDone`; the runner then finishes the
  item `already_done` with no push, adr-003). Change wording
  freely; never the contract. (A later goal moves the guidance onto the
  platform as agent definitions.)
- `internal/latchkey/` + `internal/auth/token.go`: the machine
  credential — Latchkey's device flow, the rotating refresh grant, and
  the `~/.mesh/token` that holds either shape behind one `TokenSource`
  (latchkey `adr-machine-identity`).
- `internal/node/`: the loop (`runner`), the workspace and the project's
  context (`workspace.go`: `FetchContext`, `RenderIndex`, the size budget — node.yaml `context:`, `ContextConfig`, re-read each tick, `Loop.ContextBudget`; every document counts against `bytes_max`, instructions and ADRs are exempt from `doc_bytes_max` only, placed first those of the session's own repos (`ContextOptions.Repos`), then the rest by recency, then the PRD and TRD, and one that does not fit is left whole to the index (`Context.Dropped`, ADR-015); INDEX.md is inside `bytes_max` too, its length with nothing on disk reserved before placing and the last-placed document unplaced while the written index would pass it, in two tables — `Documents a session reads` (the `ContextKinds`, summary when not on disk) and `Other documents` (`kind | title | slug`) — the live section outside the budget (`Context.IndexBytes`, ADR-015's amendment 2026-09-29)),
  evaluate, plan and research sessions, the service install (`launchd` /
  `systemd --user`). A project's repos — for the planner, the director
  and `mesh goal plan` — are read from `project.repoIds` through
  `ProjectRepos`, each owned by its own account's login, never the
  org's account: an org links several GitHub accounts (the runsheet
  org's loom project lives at go-apis/loom). `internal/gateway/client.go`: the platform client.
  Its entity selections are generated (`fields_gen.go`, never hand-edited)
  from `schema.json` and `fields/<Type>.txt` (one name per line, sorted,
  `?` for optional): add a line, `go generate ./internal/gateway/...`
  (mesh-cli ADR-010).
  `internal/channel/`: channel mode (one session per org, turns).
  **A draining node claims nothing** (`Loop.Drain`): see the drain
  below. node.yaml's `claim: {unmerged_per_repo: N}` (`ClaimConfig`,
  re-read each tick, `Loop.UnmergedPerRepo`; absent or 0 is no limit
  and no read) holds fresh claims for a repo with N items in `review`,
  counted from the node's own listing, never `RepoIndex.reviewPile`;
  started work there is still claimed (ADR-017).
- **A death names itself** (`internal/crash/`): `defer crash.Guard()`
  at the top of `mesh node start` and of every goroutine the node
  starts turns a panic into one node.log line, `mesh node: fatal:
  <value> (version <v>)`, the stack, and exit 70 (`crash.ExitPanic`),
  never Go's 2 (systemd's `INVALIDARGUMENT`); a new node goroutine
  defers it first (goal node-yaml-takes-effect-or-says-why-not, The
  04:00:22Z exit).
- **The director** (`internal/node/research.go`, runsheet trd §6c): a
  project carries **directions** — several, each its own aggregate with
  its own words, window, timezone, gap, ceiling and research claim
  (`Directions(org, project)` lists them; the id is
  `direction:<project>:<slug>`). On every tick the loop offers each live
  direction with room to spare — `ClaimDirectionResearch`; the platform
  refuses it outside the hours, inside the gap, when the direction is
  off or archived, or when another node holds it, so *a refusal is the
  ordinary answer and is not logged as an error*. A session that wins
  the claim reads the direction, the repos and the context; a `propose`
  direction's session pitches and answers (`ParsePitches`: problem,
  evidence, bet, scope, estimate, risks, alternatives, signal, and its
  answers to open questions on earlier pitches) and a `set` one
  proposes goals (`ParseProposals`); `RecordDirectionResearch` hands
  them in with the cost — `pitches` and `answers`, or `proposals` — and
  the platform decides what becomes of them. An older platform without
  `pitches` gets the pitches as proposals (`ErrNoPitches`), no error,
  said once per run (`takes no pitches`). A session whose output cannot
  be read hands in an empty research anyway — otherwise the claim holds
  the direction for 45 minutes and the night is lost. **Two ceilings
  hold and `RoomFor` takes the smaller**: the direction's own
  `maxLiveGoals` less the live goals it set (`Goal.DirectionID`), and
  the project's `maxLiveGoals` — its total over every direction, on the
  autonomy policy beside `replanCap` — less every live goal on it. The
  guard is by the direction's `mode` (runsheet ADR 022,
  `Direction.Proposes`): a `set` direction — or one with no mode, as an
  older platform serves it — with no room is not offered at all, since
  every goal it set would be dropped; a `propose` direction is offered
  whatever the room, since its pitches (or proposals) wait for a person
  and take no live-goal slot, and its session is told the room as
  information (`ResearchInput.Proposes`). Each direction works in
  its own checkout, `ResearchDir(root, project, direction)`, because two
  of one project may be inside their hours at once.
- **The drain** (`Loop.Drain`, trd §3 Drain): `SIGUSR1` or `mesh node
  drain` (which signals the pid in `~/.mesh/node.pid`) stops every new
  start — items, plans, research, evaluations, rebases — and `Run` returns nil
  once no session of *any* kind is left (planners and evaluators do not
  count against capacity, so `Running()` is not the exit test); the
  process exits 0 and its service starts the new binary.
  `~/.mesh/node.draining` marks it for `mesh node status`, and every
  heartbeat says `draining`. Drain is for taking a machine out of
  service, not for upgrades: **an upgrade restarts at once and adopts**
  (`Loop.Restart`, adr-002's amendment) — `SIGHUP` or `mesh node
  upgrade` cancels every context the node runs sessions in
  (`Loop.Context`), each `Job.Outlive` session detaches, `Run` returns
  `ErrRestart` without waiting and `mesh node start` execs its binary
  again; the new process adopts. `DrainAndWait(ctx)` is the drain only a
  sandboxed node's self-update still blocks on; `Resume` undoes a drain
  that has not yet ended `Run`. The heartbeat's extras (`gateway.HeartbeatExtras`: `draining`,
  `updateError`, `limits` — the latest Claude plan limits per
  account — and `scaling` — a ranged node's `{capacity, min, max,
  reason, accounts}`, the capacity in force, node.yaml's range, why
  and each account's slots, sent only when the capacity is a range
  or the node has several accounts (node.yaml's and the issued ones it
  knows, `Loop.Known`), and only when the gateway's
  schema has the field (introspected, kept an hour: adr-005
  §Heartbeat); both for runsheet goal
  `limits-on-the-console` — and `runtimes` and `adapters` — the tool
  runtimes (npx, uvx, docker) and agent CLIs (claude-code, codex)
  `Loop.LookPath` finds, probed every tick, for `tools-in-the-console`
  — and `commit` — the short sha the binary was built from
  (`Loop.Commit`, `version.Commit()`), beside `version`, which is
  `version.Resolved()`: the release tag when built from one, else that
  sha; sent only when the gateway's `HeartbeatNodeInput` has the field,
  introspected and kept an hour like `scaling` (mesh-specs ADR 013,
  adr-002's tag-or-sha amendment)),
  `adopted` (below) and `holding` — the node's holding decision as data,
  not a log line: each account's status, reset time and what the node
  still takes on it, and the summary judgement (`HoldingReport`,
  adr-005's amendment, for runsheet goal `node-says-why-work-waits`) —
  are sent only when set,
  because they are fields the platform half adds. Capacity, the
  limits thresholds and the orgs need no drain: `Reload` re-reads them
  every tick.
- **Adoption** (`internal/node/adopt.go`, mesh-cli ADR-009, mesh-specs
  ADR 008): a session's unit and output file own it, not the node
  process. Build, evaluate, plan, research and rebase jobs are
  `Job.Outlive`: a node shutting down detaches (the unit runs on, its
  `<workdir>.session` and temp dirs stay) and only the job's own
  timeout stops it. On `mesh node start`, before the first tick,
  `FindSessions` walks the work root for `state.json` naming one of the
  node's ids — then `SweepScopes` (`sweep.go`, ADR-008 The start-up
  sweep) stops every other active mesh-session scope whose item or goal
  is terminal or unknown on the platform (`SessionLive`, `GoalLive`; a
  read error leaves it; `mesh node status` lists the scopes the same way
  by `ScopeReport`, `sessions: <n> live scope(s), <m> leaked`) — and `Loop.AdoptSessions` takes each up: marked as the
  loop's own (a build in `Running()`, every kind in the drain's
  `sessions()`, never claimed or started again, and `Loop.Adopt` skips
  it), the item heartbeated at once so the stall window never elapses,
  then `AdoptWith` → `Runner.AdoptItem`, `Evaluator.AdoptEvaluation`,
  `Planner.AdoptPlan`, `Director.AdoptResearch`, `Rebaser.AdoptRebase`:
  `ClaudeCode.Attach` follows `output.jsonl` from the saved offset (the
  offset is saved after every event) with the usual heartbeat ticker,
  and the end goes through the *same* tail as a live session —
  `afterBuild`, `afterEvaluate`, `readPlan`/`handIn`, the director's
  `handIn`, `afterRebase`. A session whose pid is gone is read from its
  file: a result finishes it, none fails it with its last text. The
  session dir goes (`harness.RemoveSession`) only once the end is
  handled. The heartbeat's optional `adopted [{itemId, kind, since}]`
  lists them while they run. Sandboxed and Codex sessions still end
  with the node.
- **Self-update** (`internal/node/selfupdate.go`, `internal/selfupdate/`,
  adr-002): after each heartbeat the loop reads the node's desired
  version (`GetDesiredVersion`; empty, the running one or `dev` does
  nothing). A different one is fetched and verified (the signed
  `SHA256SUMS` of the tag release) first, then the node swaps its
  resolved executable keeping `<path>.prev` and `Run` returns
  `ErrRestartForUpdate` at once — no drain, its sessions left for the
  new binary to adopt — so the process exits 0 for its service to start
  it (a sandboxed node drains before the swap: its pods end with it). A refusal resumes, heartbeats `updateError`
  and waits an hour on that version. `mesh node start` counts starts in
  `~/.mesh/update.json` (`CheckStart`): the third unconfirmed one rolls
  back to `.prev`, reports it and exits 0; the first heartbeat confirms.
  The loop's `Update` is an interface; tests inject a fake.
  **`update.json`'s `State` carries why, not just that**: `startError`
  (`NoteStartFailure`) is why the last start of the pending update ended
  before it could confirm — a config it could not read, a panic, an
  exited process — and `confirmError` (`NoteConfirmError`) is the last
  error a heartbeat hit while unconfirmed — a refusal or a timeout, the
  platform being slow rather than the binary failing to start. Neither
  affects `Pending()`, and `Confirm` clears both. `Rollback` takes the
  confirm window as a parameter and names the real cause when one is
  recorded — `'<to> did not start: <startError>'`, or `'<to> did not
  confirm within <window>: <confirmError>'` — falling back to the old
  `'<to> did not start; rolled back to <from>'` when neither is set.
  `node.StartCheck` (from `CheckStart`) carries `Pending` and `Attempts`
  so the caller can say what it is confirming: node.log's first lines,
  logged by `node.StartLines` immediately after `startUpdates` and
  before `LoadConfig`, `platformClient` or any platform call, name the
  version, the pid, the config path and either `'confirming update <from>
  → <to>: start <n> of 3, rolls back on the third'` or `'no update
  pending'` — so a silent exit before registration can never happen
  again. `internal/commands/nodecmd.go`'s `registerNode` is everything a
  start needs before the loop exists (`LoadConfig`, the accounts'
  secrets, `platformClient`, the work root, registration); an error or a
  panic there while an update is pending is recorded with
  `NoteStartFailure` (a panic is re-raised after recording). node.yaml's
  `update.confirm_within` (`Config.Update.ConfirmWithin`,
  `Config.ConfirmWithin()`) is how long a new process keeps trying to
  confirm before giving up — default 5 minutes
  (`selfupdate.DefaultConfirmWithin`). **The confirm window** opens at
  process start (`Loop.Started`): while an update is pending
  (`Loop.Confirming` = `StartCheck.To`), `registerNode` retries a failed
  registration (60 s per attempt) on the heartbeat interval
  (`confirmWindow.retry`) and the loop keeps heartbeating, each failure
  logged `'self-update: confirming <to>: heartbeat <org>: <err>; retrying
  for <left> more'` (a timeout adds `(the platform is slow)`,
  `PlatformSlow`) and recorded by `Updater.NoteConfirmError`. The first
  heartbeat through confirms. The window elapsing with none through is
  `ErrUnconfirmed` (`*UnconfirmedError`, naming the last error) from
  `registerNode` or `Run`; `endRun` logs `'self-update: <to> did not
  confirm within <window>: <err>; exiting for the service to start it
  again'` and the process exits 1 — the start count rolls back on the
  third, naming that error. Only a failure to *run* (`NoteStartFailure`)
  is "did not start"; a slow platform never is. With no update pending
  a failed heartbeat or registration is as before. `mesh node upgrade`
  prints the window and `~/.mesh/node.log` after the restart.
  `mesh node update [version]` (`internal/commands/nodeupdate.go`) does
  it by hand on the same `ReleaseUpdater`: verify, drain the pidfile's
  node, swap; `--verify-only` verifies into a temp dir. `mesh node
  upgrade` (`internal/commands/nodeupgrade.go`) is the upgrade without
  a drain: `--version`, the latest release, or `--binary <path>` (once
  `<path> version` answers), swapped keeping `.prev` for the rollback,
  then `systemctl --user restart` of the unit, `launchctl kickstart -k`,
  or `SIGHUP` to the pidfile's node; a sandboxed node only with
  `--force`. The systemd unit's `KillMode=process` and the
  LaunchAgent's `AbandonProcessGroup` keep a service stop off the
  sessions. **The node
  image** (`images/node/Containerfile`, embedded as
  `nodeimage.Containerfile`; `internal/sandbox/image.go`, mesh-cli
  ADR-007): both ensure the *new* version's image after the verify and
  before the drain (`Loop.Image`, `nodeUpdate.Image` →
  `SandboxConfig.EnsureImage` → `sandbox.EnsureImage`/`BuildImage` on
  an injected `Runner`): pull `ghcr.io/runsheet/mesh-node:<version>`
  (node.yaml `sandbox.image`: empty that, `build` built here, else as
  given), prune older tags but the running one's. A failed pull is an
  `updateError`: no drain, no swap, never a silent build. `sandbox:
  off` pulls nothing. release-tag.yml's `image` job pushes and
  cosign-signs it before `publish`.
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
  `Credentials.Open`, which asks the platform (`gateway.Session`: the
  platform's `itemCredentials`/`itemTools`, `planCredentials`/`planTools`
  or `researchCredentials`/`researchTools`, by what the session works on)
  for the claimed session's credentials and chooses among them by the
  loop's own rule (`withChooser`); an api key at its cap is never
  chosen, and none left releases a build. A platform without the read
  or issuing none leaves node.yaml, said once per run. At the end
  the session's `SessionAccount.Report` (`gateway.UsageReport`:
  credential id and kind, tokens, tool calls, wall time, list and
  actual cents, windows before and after) rides its finishing command
  as `usage` — `FinishWorkItem`/`FinishWorkItemAlreadyDone`,
  `EvaluateWorkItem` (the Codex second opinion folded in), `PlanGoal`,
  `FinishWorkItemRebase`, `RecordDirectionResearch` — and
  `SessionAccount.Finish` logs every session's record, carried or not,
  as one `<label>: usage <kind> {…}` line (`SetLabel`; a carried
  session's is in its carrier's; `ErrNoUsageLedger` said once per run),
  and `mesh node usage` (`ReadUsage`, `usagereading.go`) reads them back
  into sessions, input tokens, cache reads and context bytes by kind for
  a window (adr-016's amendment).
- **Codex** (`internal/harness/codex.go`, goals `codex-adapter` and
  `codex-sessions-reach-the-context-tool`): the
  `codex` harness, `codex exec --json --sandbox workspace-write` in the
  workspace, the prompt on stdin, the stream read by
  `ParseCodexStream` (`docs/codex-events.md`). Each run gets a fresh
  temporary `CODEX_HOME`, removed after it: a plan account's
  `auth.json` copied in (and written back when Codex refreshed it), or
  an API key in the environment only (`CODEX_API_KEY` and
  `OPENAI_API_KEY`), plus the MCP servers as `config.toml`, secrets by
  variable name (mesh-specs ADR 006): the built-in by `CodexMCPConfig`
  (a `${VAR}` resolved from the job's env or refused), console tools by
  `tools.CodexConfig`, every table `default_tools_approval_mode =
  "approve"` — `exec` under `workspace-write` refuses an unapproved
  MCP call (docs/codex-events.md, "Why the context tool failed").
  The sandboxed commands get the network (`-c
  sandbox_workspace_write.network_access=true`, `harness.SandboxNetwork`,
  on `exec` and on the probe alike): without it Codex's seccomp filter
  refuses `socket()` and no test that binds loopback runs — the price
  is a sandbox that can reach the network (ADR-011's amendment
  2026-09-29, item 9).
  A **provider** (docs/adr/adr-015) is an OpenAI-compatible endpoint an
  account runs against instead of OpenAI: `codex.providers.<name>` in
  node.yaml gives a `base_url` (vLLM and SGLang serve OpenAI's API under
  `/v1`), an optional `wire_api` (`chat`, the default, or `responses`)
  and the list prices of the models it serves; an account names one with
  `provider:` and must be an api-key account, because the key rides
  `OPENAI_API_KEY` and a plan login has none to send. The provider is
  declared first in `config.toml` as `model_provider` and its table, and
  on its own makes the file worth writing. Its price table is the only
  one its models are costed at — an unpriced model records tokens and no
  money — so `CodexPrices` stays OpenAI's alone. An adapter is the CLI
  and a provider is where the tokens come from: Kimi behind vLLM is a
  provider, not a third adapter.
  Each session's event stream and stderr are kept for 24 hours under
  `<work root>/kept-sessions/<name>/` (`output.jsonl`, `stderr.log`;
  `KeepSessionOutput`), as a Claude session's output is. Before a Codex
  evaluation starts it is preflighted (`Codex.Preflight`, below) and
  falls back to Claude when the context server cannot be reached.
  node.yaml's `codex:` block
  (`CodexConfig`: binary, accounts, `models {opus, sonnet, haiku}`)
  and `CodexModel` map the Claude tiers to Codex models, by default
  opus → `gpt-6-sol` and sonnet/haiku → `gpt-6-luna`, astra only when
  pinned. A session whose context tool failed on every call gives no
  verdict: a tool failure is never a verdict (the evaluator's
  `toolBroken`, below). Codex has no
  budget flag, so `MaxBudgetUSD` is not enforced. An issued openai
  credential serving `codex` is a Codex account too (adr-005,
  `PrepareCodexAccount`): `Credentials.Open` with adapter codex chooses
  among those alone, the login lands as the session's
  `CODEX_HOME/auth.json` (0600, gone with the home; a refreshed one
  kept at `CodexLoginCache` per id and secret version), a key as
  `CODEX_API_KEY`/`OPENAI_API_KEY`, and the harness writes only
  `config.toml` into a home the job names.
- **The evaluator's family** (`internal/node/evaluate.go`,
  `docs/adr/adr-011-evaluations-run-on-the-family-with-headroom.md`,
  amending `adr-005-codex-second-opinion.md`; goal
  `codex-sessions-reach-the-context-tool`): `Evaluator.Codex` is the
  Codex harness (`codexSecondOpinion` in `nodecmd.go`: built whenever
  the codex binary is found, with or without a `codex:` block, on the
  block's first account) and `CodexAccounts` is
  `Loop.CodexCandidates` — the issued Codex accounts `Known` holds and
  node.yaml's (`CodexOwnAccount`, no ID). Before an evaluation starts
  the evaluator judges them with `ChooseAccount` over
  `Limits.CodexReadings` for the goal's priority: one with headroom
  makes it a Codex session on `CodexModel` of the evaluate model
  (defaults gpt-6-sol for opus, gpt-6-luna for sonnet and haiku — astra
  costs $1.29 for one evaluation where sol costs about $0.45, so it
  runs only when pinned), opened by `Credentials.Open`
  (adapter codex) or `OpenOwn`; else Claude as before. A full `gpt-…`
  evaluate/item/agent model pins Codex (`EvaluatorPin`; no headroom is
  `ErrFamilyWaits`, the item stays in evaluating, said once per item by
  `Waits`), a `claude-…` one pins Claude; a bare tier pins nothing. The
  loop starts an evaluation on Codex room (`Loop.CodexRoom`) when no
  Claude account has any. Before any Codex session starts,
  `Evaluator.CodexPreflight` (`Codex.Preflight`: `codex mcp get
  mesh-context` and an MCP tools/list handshake in the session's own home
  and env, then the sandbox probe `codex sandbox -c
  sandbox_mode="workspace-write" -c
  sandbox_workspace_write.network_access=true -- mesh sandbox-probe`, the
  session's own policy on the hidden subcommand that binds 127.0.0.1:0 —
  a `bwrap:` line or a non-zero exit is `sandbox: <why>`, the bwrap
  line, else the socket error) must pass: a failure moves an unpinned evaluation to Claude
  (`Codex preflight failed (<why>); evaluating on Claude`), keeps a
  pinned one waiting like `ErrFamilyWaits`, and makes a Codex second
  opinion `unavailable: preflight …`. An item of an `urgent` or `high` goal whose
  first verdict passes is evaluated again on the other family with the
  same prompt and workspace: both pass is passed, a second fail is
  failed; the notes carry both, `Codex (first evaluation, gpt-6-sol):
  VERDICT …` and `Claude (second opinion, opus): VERDICT …` (or Claude
  first, Codex second), truncated together to 4000, and the cost and
  tokens are the sum — all in the one `EvaluateWorkItem`, whose usage
  names `adapter: codex` and the gpt-6 model for a Codex evaluation. A
  second session that errors, times out or reaches no verdict, or no
  Codex account with headroom, keeps the first verdict with `<Family>
  second opinion unavailable: <reason>`. A first fail or a normal/low
  goal never runs a second opinion. **A tool failure is never a
  verdict** (`toolBroken`): a session whose context server (or a
  required console tool) errored and was never reached — no call it
  answered, clean (`ToolStat.Clean`) or with its own error result
  (`ToolStat.Answered`: a Codex `failed` call with `error` null and a
  result, a Claude `is_error` result from a server init named
  `connected` that is not Claude Code's own refusal) — or, on Codex,
  every command of which bubblewrap refused, or whose test commands
  that executed every one refused a socket — a runner's name in quoted
  text is no test, a declined one or one with no exit code never
  executed (`Result.SandboxBroken`, `sandbox: <bwrap or socket line>`) — has its verdict discarded; a
  tool that answered was reached (`ToolStat.Reached`), so the
  built-in's `not found` for a slug it does not have is the tool
  working, never a tool failure (ADR-011's amendment 2026-09-29, a tool
  that answered was reached). For a broken one the same job runs on the other family
  as the evaluation proper, its notes opening `<Family> (first attempt,
  <model>): no verdict — <tool>: <error>`; with no other family it
  is **no verdict** (`Evaluator.noVerdict`, ADR-011's amendment
  2026-09-29): `EvaluateWorkItemNoVerdict` sends `evaluateWorkItem`
  `outcome: no_verdict` (`gateway.EvaluationNoVerdict`) with the reason
  in `notes` and the cost and usage, never a fail — as does a deciding
  session with no `VERDICT:` line. A gateway whose
  `EvaluateWorkItemInput` lacks `outcome` (introspected, kept an hour)
  gets nothing (`ErrNoVerdictOutcome`, said once per run): the item
  stays in evaluating (`ErrNoContext`, said once per item by `Blind`).
  A family blind on the first attempt is not asked for the second
  opinion: the other family's verdict stands. With neither a `codex:` block nor
  an issued Codex account the node evaluates as before. Builders on
  Codex come later, behind a per-project setting.
- **Tools** (`internal/tools/`, ADR-006, trd §3 Tools): the
  claim-time read `gateway.Session` hands a session its credentials and
  its `tools` (`[]tools.Tool`, secrets decrypted for the claiming
  node); a platform without `tools` gets none and is asked for
  credentials alone from then on. `Loop.Runtimes()` is what the last
  probe found. Build and evaluate sessions get theirs
  (`SelectTools` by role and runtimes, `SessionTools.Apply`: mesh-context
  plus each tool in `Job.MCPServers`, secret values in `Job.Env` and
  `Job.Secrets` only, the tools on `Job.Tools` for Codex); a
  nice-to-have tool the node cannot start is omitted and noted on the
  item, a required one releases the item and `Declines` keeps the loop
  off it while the runtimes are unchanged. Plan, research, rebase and
  channel sessions keep mesh-context alone. `tools.IsBuiltin(name)`
  knows the built-in by its current name and, until 2026-10-28, its old
  one, `prologue` — read only from a session an older node started and
  this one adopted (`Evaluator.toolBroken`, `UseNote`); a console tool
  named `prologue` is an ordinary tool, one named `mesh-context` clashes.
- **Tool results** (`internal/harness/trim.go`, node.yaml
  `tool_results.max_kb`, default 16, 0 off): a host build session gets
  a `PreToolUse` hook on `Bash` in its `--settings` (`Job.Settings`,
  `ToolTrimSettings`) — `mesh tool-trim --max-kb N --hook` rewrites the
  command to `( <cmd>\n) 2>&1 | mesh tool-trim --max-kb N`, which keeps
  the first 60% and last 40% of the cap (`TrimToolResult`) — and every
  build session `MAX_MCP_OUTPUT_TOKENS` and `BASH_MAX_OUTPUT_LENGTH`
  (`ToolResultsEnv`). Channel mode gets neither; a sandboxed one the env.
- **The hand-back** (`internal/harness/handback.go`, adr-012
  Hand-back): a host build's turns are priced by tier (`listPrices`,
  dated); past `Job.HandBackAt` (the runner's
  `harness.DefaultHandBackAt`, 0.6) of `MaxBudgetUSD` with
  `Job.HasCommit` false (`HasCommitSince`: `git rev-list --count
  origin/<base>..HEAD`) the unit is stopped and `--resume`d once on the
  budget left with `HandBackPrompt`. `Result.HandedBack`/`SpentOn`; the
  runner notes `SpentOn` on the item, keeps it in
  `~/.mesh/items/<item>.last-session` (`RunState.RecordLastSession`)
  and the next claim's `BuildPrompt` renders it as "The last session on
  this item". No commit still is an empty finish, back to `ready` on
  the platform. Sandboxed, adopted and Codex sessions do not hand back.
- **Validation** (`internal/node/validation.go`, `internal/validate/`,
  docs/adr/adr-013 and adr-014, runsheet ADR 032): after a successful,
  not already_done build and before the push, `.runsheet/validate.yml`'s
  steps the diff selects (`Workspacer.ChangedPaths`) plus the always
  ones run on HEAD — in the session's pod (`sandboxRunner`, runner
  image by `sandbox.ImageDigest`) or on the host (`host:<hostname>`,
  the session's env less its secrets). The attestation goes to
  `<workdir>.session/attestation.json` and, after the push, to
  `RecordValidationAttestation` on the item's token. A failing step
  gets one retry turn (`harness.ValidationRetryPrompt`, the session
  resumed when it can be), then `FailWorkItem` with `validation failed:
  step <name> exited <code>`; per-step ms ride the usage report's
  `validation`. No validate.yml finishes as before.
- **Services** (`internal/sandbox/services.go`, mesh-specs ADR 007):
  `Resolve(repoDir, allowed)` reads what a repository declares, first
  found wins — `.mesh/services.yml`, else the `services:` of its
  workflows' test jobs (merged by name, not required), else none — into
  one `Service` shape with a parsed `HealthCheck`, test tuning for
  Postgres and MySQL/MariaDB, and a disallowed registry returned
  `Refused` with a note. Pure parsing; the fixtures are under
  `internal/sandbox/testdata/` (their `.mesh/` needs `git add -f`).
- **Session units** (`internal/sessionunit/`, mesh-specs ADR 008,
  `docs/adr/adr-008-sessions-in-their-own-unit.md`): a host `claude -p`
  is not the node's child. `ClaudeCode.Run` starts it through a `Unit`
  (`Start` → `Handle{PID, UnitName}`, `Alive`, `Stop`):
  `SystemdScope` (`systemd-run --user --scope
  --unit=mesh-session-<kind>-<subject>-<rand> --collect`, outside the
  node service's cgroup) when `systemctl --user` answers, else
  `Detached` (setsid, its own process group); the sandbox's pod becomes
  a third later. Its stdout goes to `<workdir>.session/output.jsonl`,
  stderr to `stderr.log`, and `state.json` (`State`, written
  atomically: node, item/goal/direction, claim, kind, model, account,
  pid, unit, startedAt, offset — never the prompt or a secret) from
  `Job.Meta`; the prompt is an unlinked file on stdin. `Follow` tails
  the output a line per read until the process is gone and the file
  stops growing, `ParseStream` reads it, and the offset is saved as it
  goes. A timeout or cancelled ctx `Stop`s the unit, and every end
  but a detach `Kill`s it: `Result.Killed` names what the agent left
  running there (a failed kill goes to Stderr, else `KillError`,
  never failing the session), and the node logs what it killed, a
  line per session (`logKilled` on `Result.KilledPerSession()`:
  `<subject>: session end killed <n> process(es) the agent left
  running: <pid> <comm>, …`, and `kill: <KillError>`); a build and a
  rebase also send it as a progress note, an evaluation puts it in the
  verdict's notes when it records one and only logs it (held for the
  next verdict, `LeftRunning`) when it records none, a plan and
  research log only (ADR-008, The session's end) — an adopted session
  whose subject is no longer the node's is `FoundSession.Kill`ed and
  logged the same way; the directory is
  removed when `Run` returns — unless the job is `Outlive`: see the
  adoption.
- **The sandbox** (`internal/sandbox/`, mesh-specs ADR 007,
  `docs/adr/adr-007-the-sandbox-on-the-node.md`): with a sandbox,
  `harness.ClaudeCode` runs `claude` in the session's own rootless
  Podman pod (`Sandbox.Open` → `Session.Run` → `Close`) from the node
  image: mounts only the workspace and a temporary HOME (the MCP config
  file, `CLAUDE_CONFIG_DIR`, `GH_CONFIG_DIR`, `.gitconfig`), an env
  from the allow-list (`AllowedEnv`, `MESH_TOOL_*`, `BaseEnv`; never
  the node's), `--cpus/--memory/--pids-limit` from node.yaml
  `sandbox:` (`SandboxConfig.Settings`), and nftables rules an init
  container loads (`NetworkPolicy.Ruleset`: the LAN, the host and the
  special ranges refused, 443/80 out). `Runtime` is `Podman` or the
  test `Fake`. `OpenSandbox` decides at start: `sandbox: off` is nil
  and today's exec path byte-for-byte; the mode left out is on when
  `podman --version` answers, else off with a log line; an explicit
  `on` without podman refuses to start. A limit kill
  (`*sandbox.LimitError`: OOM, pids, the runtime's timeout) is the
  node's condition: the runner releases the item, holds it off
  (`Declines`) and `Loop.NoteCondition` puts the reason on the
  heartbeat for `ConditionHold`; the heartbeat also carries `sandbox
  {mode, runtime, image}`. Channel-mode sessions and Codex are not in
  the pod yet.
- **Sidecars** (`internal/sandbox/sidecars.go`, `internal/node/services.go`,
  mesh-specs ADR 007 Services): build, evaluate and rebase call
  `OpenSessionPod` before the agent — `Resolve` the checkout, then
  `Session.Start`: the pod, each allowed service a sidecar in it
  (`AddSidecar`, `--pull missing`) under `SplitLimits` of the services
  budget, then every health check at once (`--health-cmd` by
  `ExecSidecar`, else `ProbeTCP` on the first port) within
  `sandbox.service_timeout`; `Run` runs the agent in that pod
  (`Job.Session`) and the node's `Close` removes it on every path. The
  prompts' `# Services` section (`ServicesLine`) and
  `$HOME/.mesh-services` (`ServicesListing`) list them. A missing
  optional one is a progress note (the verdict's notes for an
  evaluation); a required one releases a build, declines an evaluation
  (`Declines`) or fails a rebase, naming it, and no agent starts; a
  required refused one makes no pod at all.
- Every session gets mesh-context as an MCP server with the node's token and
  the project's context on disk under `.mesh/context/` with `INDEX.md`:
  the documents, then `## Live goals and items` — each live goal's items
  (seq, title, status, branch, PR) and the finished goals one line each,
  read per session through the gateway (`AddLive`, adr-003 decision 3).
  A failed read leaves the documents only, with a warning; `.mesh` is
  excluded from git, so none of it is ever committed.
  `WriteContext` returns what that cost (`ContextWritten`: bytes on
  disk with INDEX.md, files, documents left to fetch); each session
  logs it once (`<label>: context <n> bytes in <k> files, <m> documents
  to fetch`) and its usage report carries `contextBytes`, dropped for a
  platform without the field (`ErrNoContextUsage`, said once per run;
  docs/adr/adr-016).
- Costs: each session's `total_cost_usd` is reported to the platform per
  item (`ReportWorkItemProgress`, `EvaluateWorkItem`, `PlanGoal`).
  Priced tool calls are tool usage records: the harness counts each
  session's MCP calls per server (`Result.ToolCalls` — Claude's
  `tool_use` named `mcp__<server>__…`, Codex's completed
  `mcp_tool_call`, each call's id on `Result.ToolCallIDs`), and after a
  build or evaluate session `SessionAccount.Finish` sends one
  `RecordToolUsage(itemID, toolCallId, tool, report)` (mutation
  `recordToolUsage`) per call of a rendered tool with
  `pricePerCallCents > 0` — the session's report at the tool's price,
  no tokens, no actual cost; mesh-context and unpriced tools are none.
  A platform without `recordToolUsage` is said once per run and each
  call logged instead; it never fails the item.
  Every server's calls, errors, ms and first error's text (200 chars,
  redacted; `Result.ToolReport()`) ride the usage report as `tools`
  beside the total (`SessionAccount.Add` merges carried sessions' rows,
  the first error kept); a platform without the field gets it resent
  without (`ErrNoToolUsage`, said once, the list logged), and an entry's
  `error` goes only when the introspected `UsageReportInput` takes it
  (kept an hour). A Codex call's error is its `error.message`, and a
  server Codex says failed to start is an error with no call. A served
  console tool never called, or whose first call errored, and any other
  server that errored — the built-in `mesh-context` first — is named by
  `SessionTools.UseNote` — a build's progress note, the evaluation's
  notes, a plan's or research's log line (ADR-006 amendments
  2026-09-28).
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
- **Release path** (adr-002, amendment `node-releases-on-merge`): a
  merge to main touching `cmd/`, `internal/`, `images/node/`, `go.mod`
  or `go.sum` is tagged by `release-on-merge.yml`
  (`scripts/release-on-merge.sh`: patch, or minor on the PR label
  `release:minor` or a goal doc's `release: minor`; test prereleases
  with a `-` never count), pushed with `RELEASE_TAG_TOKEN` — never
  `GITHUB_TOKEN`, whose pushes start no workflow — and `release-tag.yml`,
  the only signer, builds it; docs-only merges cut none. `git tag` by
  hand is the fallback.
- Deploying to the mini by hand is the fallback to the release path:
  build for linux/amd64, `scp` to
  `~/bin/mesh.new` and `mesh node upgrade --binary ~/bin/mesh.new`: the
  binary is swapped (`.prev` kept), the node restarts at once and
  adopts its running `claude -p` sessions (README, "Upgrading a node
  without ending sessions"). A channel-mode session and a sandboxed
  one still end with the node: for those, `mv` it over `~/bin/mesh`
  and `mesh node drain` instead.
