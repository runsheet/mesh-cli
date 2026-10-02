---
summary: A node's sessions run on the Claude accounts the platform issues the claimed session (the claim-time read: itemCredentials, planCredentials, researchCredentials; an api key at its cap never chosen), with node.yaml's `accounts:` ({label, kind subscription|api, token_file|api_key_env}) the fallback, and every session's usage report rides the command that ends it (`usage` on finishWorkItem, evaluateWorkItem, planGoal, finishWorkItemRebase, recordDirectionResearch; each priced tool call on recordToolUsage; else one log line); the secret reaches the session in its environment only (CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY), with a temporary CLAUDE_CONFIG_DIR per session removed after, the inherited auth variables stripped, and the secret redacted from every log, error and report; each session runs on the account ChooseAccount picks by headroom and the spending order (subscriptions first, API only for urgent/high work or when subscriptions are stopped and work waits), and no account with headroom means no claim.
status: accepted
---

# ADR-005: Accounts on the node

## Context

A node has run every session on the operator's own `claude` login: one
account, whatever `~/.claude` (or the operator's `CLAUDE_CONFIG_DIR`)
holds. Goal `node-runs-on-many-accounts` (Chris, 2026-09-25, part of
"Mesh runs anything, on any account") gives a node several Claude
accounts — several Max 20x subscriptions and API keys — and has the
platform hand each session its credential at claim (runsheet
`credentials-sealed-in-loom`: sealed loom fields, decrypted only for the
node that claimed the session). That claim-time read does not exist
yet; the mini needs several Max accounts now.

The CLI signs in from its environment as well as from its config dir:
`CLAUDE_CODE_OAUTH_TOKEN` takes the long-lived OAuth token `claude
setup-token` prints for a subscription, `ANTHROPIC_API_KEY` an API key,
and `CLAUDE_CONFIG_DIR` moves the directory it keeps its state in
(sessions, settings, a stored login). Two sessions of different accounts
sharing a config dir share that state; a session that inherits the
operator's `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` beside its
own may run on the wrong account.

## Decision

- **node.yaml `accounts:` is the fallback source.** Each entry is
  `{label, kind, token_file | api_key_env}`: `kind: subscription` names
  a file holding a `claude setup-token` token (`~` expanded),
  `kind: api` names the environment variable holding an API key. The
  secret itself is never in node.yaml. Load refuses a missing or
  repeated label, an unknown kind, a subscription without `token_file`
  and an api account without `api_key_env`; `mesh node start` refuses an
  account whose secret cannot be read. No `accounts:` is the node as it
  was: one implicit account `default`, the operator's own login, nothing
  unset. When the platform's claim-time read lands it supplies the same
  `AccountConfig` shape per session and node.yaml stays the fallback.
- **The secret lives in the environment only.** A session on an account
  (`PrepareAccount`) gets `CLAUDE_CODE_OAUTH_TOKEN=<token>` or
  `ANTHROPIC_API_KEY=<key>`, and nothing but an empty directory is
  written to disk.
- **A temporary `CLAUDE_CONFIG_DIR` per session**, `os.MkdirTemp` at
  0700, removed when the session ends (`AccountSession.Cleanup`), so no
  two sessions — of one account or of two — share CLI state.
- **Inherited auth variables are stripped.** The harness's `Job.Unset`
  drops `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
  `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_CONFIG_DIR` (and `MESH_ACCOUNT`)
  from the node's environment before the session's own are added.
- **Limits key by label.** The session's env carries
  `MESH_ACCOUNT=<label>`; `node.Account` reads it first, so the latest
  plan standing (and the heartbeat's `limits`) is per account label,
  never per throwaway config dir.
- **Secrets are redacted.** `Job.Secrets` makes the harness redact them
  from the stderr it passes on and the errors it returns; the node
  redacts the session's errors and text before it logs them, reports
  them (`ReportWorkItemProgress`) or hands them in as a failure. The
  heartbeat carries labels only.
- **Which account: by headroom and the spending order.** Before every
  claim — `ClaimWorkItem`, `ClaimPlan`, `ClaimDirectionResearch`,
  `ClaimWorkItemRebase` — and before an evaluation starts, the loop
  picks the session's account with `ChooseAccount(now, accounts,
  readings, limits, priority, workWaits)`, a pure function of the latest
  reading per label:
  - each account is judged by `pace` on its own reading and the
    node.yaml `limits` thresholds: *stopped* (either window at `stop`,
    or a status not `allowed`, until that window's reset) takes nothing;
    *paced* (five-hour at `pace_five_hour`) takes only urgent and high
    work; an account never seen yet has its full headroom;
  - **subscriptions first**: of those that may take the work, the one
    with the most headroom — the lowest max of its five-hour and
    seven-day use, a window past its reset counting as unused; a tie
    goes to the first listed;
  - **an api account only for urgent/high work** — before a paced
    subscription, so the subscription's last room is kept — **or when
    every subscription is stopped and work waits** (an item, a plan, an
    evaluation, a rebase; research is not work that waits, so it runs
    on a subscription that is not paced or not at all);
  - **evaluations and rebases finish work already built**, which
    pacing never held back: they take an account by their goal's
    priority first and, failing that, a paced subscription as urgent
    and high work would (`accountToFinish`). A stopped account still
    takes neither, and they reach an api account only as their
    priority or exhausted subscriptions allow;
  - **none fits → no claim.** The item is not claimed, failed or
    released: it stays ready for a later tick or another node, and the
    tick logs one line of what waits (`accounts: no account has
    headroom for …`), not one per item.

  The chosen account rides the session's context (`WithAccount`) into
  `OpenAccount`; the session's reading lands in `Loop.Limits` under its
  label, so the next choice sees it. A held item adopted after a
  restart takes the account with most headroom, else the first listed.
  With no `accounts:` the one implicit account is the operator's login,
  judged by its strictest reading — the node paces exactly as before.

  *Amended 2026-09-27 (goal `loop-schedules-on-issued-accounts`):* the
  accounts chosen among are the union `Loop.accounts()` returns —
  node.yaml's `accounts:` first, then each issued account the node
  knows (`Loop.Known`, the same `KnownAccounts` store `Credentials.Open`
  notes into) whose label node.yaml does not already have — each
  judged by its own reading, which already lands under its label. The
  implicit `default`, judged by the strictest reading, is judged only
  when neither exists, so a platform that issues none paces as before.
  The chosen issued account rides the context on `WithAccount` like
  node.yaml's, for its label alone — `runsOn`, the per-account slots
  and the heartbeat's `scaling.accounts` name it — and `OpenAccount`
  never reads its secret: a chosen account whose `ID` is set and which
  is not among node.yaml's is passed over for node.yaml's first (or
  none, the operator's login). The secret comes only from the
  claim-time read, where `Credentials.Open` chooses among what the
  platform actually issues by the same rule (`withChooser`): the
  pre-claim choice is the loop's prediction, the claim-time read the
  truth. The `no account has headroom` line names the accounts judged
  (`…; judged max20, shhemail; left unclaimed until one has`).
- **Capacity and account choice work together.** Capacity (ADR-004:
  fixed, or `{min, max}` scaled by headroom) bounds how many items run
  at once; the choice decides whether each one may start and where.
  An item starts only when both allow it: a free slot *and* an account
  that may take its priority. The node's own mode (`nodePace`) is the
  most lenient of its subscriptions' (of every account's when it has no
  subscription): it is logged at each crossing, and when paced it
  halves the capacity as before — so a node is paced or stopped only
  when every subscription is, and one full Max account no longer
  stops a node that has another with room. Evaluations, plans, research
  and rebases still do not count against capacity, but each needs an
  account to start.

  *Amended 2026-09-27 (goal `loop-schedules-on-issued-accounts`):*
  `nodePace`, the spending order (`spendable`) and the scale judge the
  same union — node.yaml's accounts and the issued ones known —
  and `default` only when neither exists. The pacing line names the
  account that paces whenever the accounts judged are not the lone
  `default` (`limits: paced until …: max20: five-hour window at 85%
  …`), so a node on one issued subscription says which.

## The claim-time read and the usage record

Amended 2026-09-25 (item 3 of `node-runs-on-many-accounts`), when
runsheet's `credentials-sealed-in-loom` had published no field names
and the node defined a `sessionCredentials` mutation here. Amended
again 2026-09-27: the platform never took that shape — it serves the
read as runsheet ADR 019 and ADR 020 fix it, and no platform ever knew
`sessionCredentials` — so the node reads the platform's fields, and
this section describes them.

- **The claim-time read.** After a session's claim and before it
  starts, `Credentials.Open` (`gateway.Session`) asks the platform for
  the credentials that session may run on and its tools, one pair of
  query fields per session subject, each a JSON list (a `Map` scalar,
  selected bare):

  | session | credentials | tools |
  |---|---|---|
  | build, evaluate, rebase (an item) | `itemCredentials(itemId)` | `itemTools(itemId)` |
  | plan (a goal) | `planCredentials(goalId)` | `planTools(goalId)` |
  | research (a direction) | `researchCredentials(directionId)` | `researchTools(directionId)` |

  The node is not named: the platform knows it from the bearer, and
  answers only the node holding the claim (runsheet's `claimRead` —
  `WorkItem.node_id`, `Goal.planner_node_id`,
  `Direction.research_node_id`); anyone else is refused with the
  reason and no secret. A credential row is `{id, label, vendor, kind,
  serves, plan, windowTargets, dailyCap, monthlyCap, capUnit,
  secretVersion, secret}`. The node keeps the rows whose `vendor` is
  `anthropic` and whose `serves` is empty or names `claude-code` —
  any other vendor is a tool's account (ADR-006), not a session's —
  and maps `kind` `subscription` or `oauth_token` to `subscription`,
  `api_key` to `api`, with `dailyCap`/`monthlyCap` (dollars) as the
  key's caps when `capUnit` is `dollars` (0 is the default, $50 a day
  and $1,000 a month). Spend is not carried: the platform leaves a
  key at its cap out of the read (ADR 019, trd §6b), so room among the
  issued is the node's own readings (§3 Pacing). `gateway.Credential`'s
  `String` and `GoString` leave the secret out.
- **Issued credentials are the session's accounts.** They become
  `AccountConfig`s (`IssuedAccount`: the label, else the id; the secret
  held by pointer for this session only, so printing an account prints
  an address) and the session chooses among them by the same rule the
  loop chose by before the claim — its priority, whether work waits,
  whether it finishes built work — which rides the context
  (`withChooser`) bound to the latest readings. node.yaml is not
  consulted. The secret reaches the session exactly as before
  (`PrepareAccount`).
- **The hard stop at an api key's cap.** A key whose daily or monthly
  spend has reached its cap is never chosen, and is logged by label
  with its figures; a key with room holds its session's budget
  (`--max-budget-usd`) to what the smaller cap leaves, so a session
  does not continue past it. When no issued credential may take the
  session it does not start (`ErrNoAccount`): a build releases its item
  (`ReleaseWorkItem`, not failed), an evaluation stays in evaluating
  for the next tick, and a plan, research or rebase ends as its own
  error path does.
- **node.yaml is the fallback.** A platform without the credentials
  field for that session's subject (`Cannot query field`, the same
  detection as the item's GitHub token, `gateway.IsUnsupported`), or
  one that issues an empty list, leaves
  node.yaml's accounts and the loop's pre-claim choice exactly as
  above. The first fallback of a run logs once — `credentials: the
  platform issues no session credentials; sessions run on node.yaml's
  accounts (said once, for every session this run)` — and every
  session still asks, so a deploy of the platform half takes effect
  without a restart. A refusal is an error, as for the GitHub token:
  a session the platform will not credential does not run on the
  node's own accounts instead.
- **A usage report per session, on the command that ends it.** Every
  build, evaluation, plan, research and rebase session carries one
  report (`SessionAccount.Report`, runsheet trd §6a, ADR 018 (e)(f),
  ADR 019) as the optional `usage: UsageReportInput` of its finishing
  command: `finishWorkItem` (a build, built or already done),
  `evaluateWorkItem`, `planGoal`, `finishWorkItemRebase` and
  `recordDirectionResearch` (the empty hand-in of an unreadable session
  too). The report (`gateway.UsageReport`) is `{sessionId, adapter
  (claude-code|codex), model, credentialId?, credentialKind
  (subscription|api|unset), inputTokens, outputTokens,
  cacheReadTokens, cacheWriteTokens, toolCalls, wallMs, listCostCents,
  actualCostCents, turns?, inputPerTurnMean?, inputPerTurnMax?,
  largestToolResults?, fiveHourBefore?, fiveHourAfter?, sevenDayBefore?,
  sevenDayAfter?, fiveHourResetsAtBefore?, fiveHourResetsAtAfter?,
  sevenDayResetsAtBefore?, sevenDayResetsAtAfter?}`. `credentialId` is
  the id the claim-time read handed the session, left out for a
  node.yaml account or the operator's own login (`unset` then when it
  has no kind). The tokens are the stream's result `usage`
  (`harness.Result.Usage`): `inputTokens` counts every input token, the
  cache reads and writes included, which are also given on their own.
  `listCostCents` is the session's `total_cost_usd` in cents;
  `actualCostCents` the same on an api credential and 0 on a
  subscription. The windows are utilizations (0..1), each reset on its
  own field: before is the account's latest reading when the session
  opened, after the session's own last `rate_limit_event` (else the
  latest reading) when it ended; a window never read is left out. One
  command, one report: an evaluation whose Codex second opinion ran
  adds Codex's tokens and cost into it (as its `costCents` already
  sums them), and a plan asked twice hands in both sessions' in one.
  The report carries no secret.
- **Per-turn input and the largest tool results**
  (`mesh-spends-less-per-item`, goal "Mesh spends less per merged
  item"): `turns` is the session's turn count (a Claude session's
  result event `num_turns`, a Codex session's count of
  `turn.completed`s); `inputPerTurnMean` and `inputPerTurnMax` are its
  per-turn input tokens (each assistant turn's own share — input +
  cache read + cache creation for Claude, the delta between successive
  `turn.completed` cumulative totals for Codex), mean and largest
  across its turns; `largestToolResults` is its five largest tool
  results by content byte length, largest first, each `{tool, bytes}`
  (`gateway.ToolResultSize`) — `tool` the tool_use block's own name
  (`Bash`, `mcp__server__tool`), not just its MCP server. All four are
  `harness.Result`'s fields of the same names (`ParseStream`,
  `ParseCodexStream`), left off the report when zero or empty, added
  into a report more than one session carries (`SessionAccount.Add`:
  `Turns` and the tool-result list summed/merged, the means recombined
  weighted by each session's own turn count, the maxes taken). A
  platform whose `UsageReportInput` has none of these four fields
  (`unknownField` on one of them, distinct from no `usage` field at
  all) gets the report again without them: the rest of the usage and
  the command land, `gateway.ErrNoUsageDetail` says the detail did not,
  and `Carried` treats it as landed — the once-per-run notice is
  `usage: the platform's usage report carries no turns or tool-result
  detail; sent without them (said once, for every session this run)`.
  Every session, whatever became of its report, also gets one
  unconditional node.log line from the stream it actually saw: `usage:
  turns=N input/turn mean=M max=X largest=Tool:bytes,…`.
- **Priced tool calls go to `recordToolUsage`.** For each call of a tool
  rendered for a build or evaluation with `pricePerCallCents > 0`,
  `Finish` sends `recordToolUsage {aggregateId: the item, toolCallId,
  tool, usage}` — the call's id from the stream
  (`Result.ToolCallIDs`), the usage a copy of the session's report with
  `listCostCents` the tool's price per call, `actualCostCents` 0, no
  tokens and none of the turn or tool-result detail (`Turns`,
  `InputPerTurnMean`, `InputPerTurnMax`, `LargestToolResults`): those
  are the whole session's figures, not this one call's, and are cleared
  on the per-call copy so a platform summing them is not counting the
  same session once per priced call. mesh-context and unpriced tools send
  none. A platform without the
  mutation is said once per run and each
  call logged; `RecordToolUsage` takes the same detail-field fallback as
  the finishing commands (`ErrNoUsageDetail`: the call still lands,
  without those fields); a failed record is logged, never the item's
  failure.
- **The log is the fallback.** `SessionAccount.Finish` is each
  session's once-per-session end. A report its finishing command did
  not carry is written to the node's log as one line, `usage: {…}` —
  the report as JSON: against a platform whose command has no `usage`
  field (`gateway.ErrNoUsageLedger`: the command lands without it) the
  first of a run says `usage: the platform's finishing command takes no
  usage report; each session's record is logged here instead (said
  once, for every session this run)`; a session that ends another way —
  `failWorkItem`, `failWorkItemRebase`, a plan or research error, a
  finish the platform refused — has nowhere to carry it, and is logged
  without that line: the session spent, and its record must exist
  somewhere. An adopted session (ADR-009) has no account in the new
  process and carries none.
- **The node remembers what the platform issues.** Amended 2026-09-27
  (goal `loop-schedules-on-issued-accounts`): `Credentials.Open` notes
  every anthropic/claude-code credential the read hands it — label,
  kind, id, caps; never the secret — in `KnownAccounts`
  (`internal/node/known.go`), before the api-cap filter, so a capped
  key is still known. It is kept in `~/.mesh/accounts.json`, beside
  `ledger.json`, one entry per label; a label the platform has not
  issued for 24h (`KnownFor`) is forgotten. `Credentials` itself does
  not schedule on these accounts; the loop does, from the same store
  (`Loop.Known`, wired in `nodecmd.go`; see "Which account" above).
- **Channel mode's builds** keep the operator's login and send no
  record: the channel session is one long session per org, not a
  session per item. Its evaluations, plans, research and rebases are
  one-shot and do both.
- **Codex accounts are node.yaml's `codex:` block** (goal
  `codex-adapter`; `harness.Codex`, not yet wired into the loop) — and,
  since 2026-09-28, the platform's openai credentials serving codex
  (above). Each
  is `{name, home | api_key_env | api_key_file}`, exactly one, checked
  at load. A ChatGPT-plan account is a login in its own home
  (`CODEX_HOME=~/.mesh/codex/<name> codex login`). Every session gets a
  fresh temporary `CODEX_HOME` under the user cache dir (Codex refuses
  its helpers under `/tmp`), removed when it ends. For a plan account
  it holds a copy of the login's `auth.json`. Codex refreshes a plan's
  tokens in place and the refresh token rotates, so a copy that changed
  is written back to the login home. An API-key account puts nothing on
  disk; the key is in the session's environment only, as
  `CODEX_API_KEY` (codex-cli 0.157.0's `exec` reads it, and with only
  `OPENAI_API_KEY` sends no credential) and `OPENAI_API_KEY`, and is
  redacted like a Claude secret. The operator's own `CODEX_HOME`,
  `CODEX_API_KEY` and `OPENAI_API_KEY` are stripped. The session's MCP
  servers are `config.toml` in the same home, secrets by variable name
  (mesh-specs ADR 006). `codex.models {opus, sonnet, haiku}` maps the
  Claude tiers to Codex models (`CodexConfig.CodexModel`). Codex has no
  budget flag, so `MaxBudgetUSD` is not enforced for a Codex session.

- **Issued Codex accounts.** *Amended 2026-09-28 (goal
  `evaluations-run-on-codex`, item 2):* the claim-time read also hands
  a session an OpenAI account. `issuedCredential` keeps a row whose
  `vendor` is `openai` and whose `serves` names `codex` beside the
  anthropic/claude-code ones; `gateway.Credential` carries its
  `Adapter` (`claude-code` | `codex`) and `SecretVersion`. `kind`
  `oauth_token` is a subscription (the secret is the `auth.json`
  `codex login` writes), `api_key` an api key with the same cap
  handling as an Anthropic key. Any other vendor, and an openai row
  not serving codex, is still a tool's account (ADR-006).
  `IssuedAccount` carries the adapter onto `AccountConfig.Adapter`
  (empty is claude-code; node.yaml's `accounts:` are Claude's, its
  Codex ones the `codex:` block's). `Credentials.Open` takes the
  session's adapter (`node.Adapter` of its harness) and chooses only
  among the issued accounts of it, by the same `withChooser` rule
  judged by that adapter's readings; a read that issues none of it is
  `ErrNoAccount` (a codex-only read for a Claude session, and the
  reverse). A Codex session with no read, or an empty one, runs on the
  Codex harness's own account, never on node.yaml's Claude ones.
  `PrepareCodexAccount` (from `PrepareAccount`) makes a 0700 home under
  `CodexHomeRoot` (`<user cache dir>/mesh/codex`, as `harness.Codex`
  does — not `/tmp`) and puts `CODEX_HOME=<home>` and
  `MESH_ACCOUNT=<label>` on the env; the harness, seeing the job's own
  `CODEX_HOME`, writes only `config.toml` into it (item 1). **The one
  exception to env-only:** a subscription's secret is written verbatim
  as `<home>/auth.json` at 0600, since Codex reads its login from
  nothing else; a secret that is not a JSON object is refused without
  echoing it. An api key goes on the env as `CODEX_API_KEY` and
  `OPENAI_API_KEY` and nothing is written. `Cleanup` removes the home.
  `Apply` strips the operator's `CODEX_HOME`, `CODEX_API_KEY` and
  `OPENAI_API_KEY` (`CodexAuthEnv`) and puts the secret, the auth.json
  bytes and each token in them on `Job.Secrets`; nothing logs them.
  **The refreshed-login cache:** Codex rotates the refresh token in
  place, so before `Cleanup` removes the home a changed `auth.json` is
  kept at `CodexLoginCache` — `<root>/logins/<credentialId>.<secretVersion>.json`,
  0600 — and preferred over the issued secret when the same id and
  version is issued again; a new `secretVersion` (a login pasted or
  rotated on the platform) discards the old file. The node does not
  hand the refreshed login back to the platform. A Codex session's
  readings land under `CodexLimitsAccount(label)` (`ObserveLimits` keys
  a codex harness's session by its env's label), stay out of
  `Limits.Readings()` and are `Limits.CodexReadings()` by label;
  `KnownAccounts` notes a Codex account with its `adapter` (no
  secret), `Loop.scheduled` leaves it out of the Claude accounts the
  capacity is counted on and `Loop.CodexAccounts` judges it apart. The
  heartbeat's `limits` rows are typed (`NodeAccountLimitsInput`), so a
  Codex account's row is marked by its `codex:` account prefix rather
  than an `adapter` field; `scaling.accounts` carries Claude accounts
  only. Its usage report names `adapter: codex`, the issued
  `credentialId` and its `credentialKind`.

*Amended 2026-09-27 (goal `node-reports-usage-on-the-finish`):* until
this date the node handed each session's record to a
`recordSessionUsage(input: RecordSessionUsageInput!)` mutation, and each
priced tool's calls to `recordUsage`. Neither was a field a node may
call: `recordSessionUsage` never existed on runsheet's gateway and
`recordUsage` is RegisterPlatformOnly, so every session logged "the
platform keeps no usage ledger" and the ledger saw nothing from the
node. The platform's ledger takes the report on the finishing command
and each paid call on `recordToolUsage`, as above; both old mutations
and their client methods are gone.

## Consequences

- Several Max subscriptions work on one node from node.yaml before the
  platform's credentials exist; the operator's own login is untouched.
- Once the platform issues credentials a node needs none in node.yaml:
  a new node of a project runs on the project's accounts from its first
  claim, and node.yaml's `accounts:` only serve a platform that does
  not (yet) issue them. The pre-claim choice still sees node.yaml's
  accounts (or the operator's login) and their readings, so a claim can
  be won that no issued credential may run; such a build is released,
  not failed. *Amended 2026-09-27 (goal
  `loop-schedules-on-issued-accounts`):* the pre-claim choice now
  sees the issued credentials too: the loop chooses, paces and scales
  on the union of node.yaml's accounts and the issued ones it knows
  (remembered 24h in `~/.mesh/accounts.json`, so a restart knows them
  before its first claim), and judges the implicit `default` only
  when neither exists — a stale `default` reading from an earlier
  session no longer holds back research or normal work that fresh
  issued subscriptions could run. A claim can still be won that the
  claim-time read then refuses (an account issued an hour ago and not
  now); such a build is still released.
- The usage ledger sees every one-shot session's tokens, cost and
  windows on the command that ends it; a platform without the `usage`
  field, and a session that ends in a failure, leave the record in the
  node's log.
- A node with two Max accounts runs on the emptier one and moves to the
  other as the first fills; money is spent on an API key only where the
  spending order allows it. When the platform's claim-time read lands
  it applies the same rule (runsheet ADR 005, capacity across
  accounts).
- The choice reads the latest reading, which a session updates only as
  it runs: sessions started in one tick all go to the account that was
  emptiest at the tick's start. The per-account slots of runsheet ADR
  005 (capacity as the sum of the accounts') are a later step.
- A token file is the operator's to protect (0600); the node reads it at
  each session and never copies it.
- A session's CLI state (its transcript, `--resume`) dies with the
  session's config dir. Nothing on the node resumes a one-shot session,
  so nothing is lost; channel mode keeps the operator's login until it
  is moved onto accounts.
- A restart's `KnownAccounts` reloads `~/.mesh/accounts.json` before the
  node's first claim, so it knows the accounts the platform issued
  before this run without waiting for a session to be claimed again.
- Two Codex sessions on one plan account that both refresh its tokens
  write back in turn, and the last write wins. If one holds a refresh
  token the other already rotated, that login has to be signed in
  again.
- Redaction is by exact value: a secret split across two stderr writes
  can pass the stderr filter, though not the error, which is redacted
  whole.

## Amendment — 2026-09-28 (goal `node-says-why-work-waits`)

The heartbeat carries `holding`: the node's holding decision as data,
not just a log line — `internal/node/holding.go`'s `HoldingReport(now,
accounts, readings, cfg, capacity, p, unclaimed)`, pure, and
`gateway.HeartbeatExtras.Holding` (`internal/gateway/client.go`), sent
as `in["holding"]` only when set and only when introspection says
`HeartbeatNodeInput` has a `holding` field (`takesHolding`, cached the
same hour as `takesScaling`). The shape, camelCase like the rest:

```
{
  mode: "normal" | "paced" | "stopped",
  summary: string,
  until?: RFC3339,
  accounts: [{
    account, kind: "subscription" | "api_key",
    status: "ok" | "paced" | "exhausted" | "unknown",
    resetsAt?: RFC3339,
    takes: { priorities: ["urgent", "high", ...], maxItems: int, research: bool }
  }],
  unclaimed: [{ goalId, kind: "item" | "plan", count: int, reason: string }]
}
```

`accounts` and `unclaimed` are always `[]`, never null, when empty (the
`scaling.accounts` precedent, PR #255: the platform's nested input is
non-null). Per account: `status` is `pace`'s mode on its latest
reading — normal → `ok`, paced → `paced`, stopped → `exhausted`; an
account with no reading yet is `unknown` (`ChooseAccount` treats it as
full headroom, so it takes everything, the same as `ok`). `resetsAt` is
the pacing's `resumeAt` when the account holds. `takes`: `ok`/`unknown`
take all four priorities, up to the capacity in force, and research on
a subscription (never on an api key: research is not work that waits,
`internal/node/select.go`); `paced` takes urgent and high only, at most
half the capacity; `exhausted` takes nothing. `mode` is the node's own
aggregate pacing (`nodePace`); `until` is the earliest `resetsAt` among
the accounts that hold. `summary` is the one-line decision the console
shows in place of the node's log line — `"normal: up to N item(s)"`,
`"paced: urgent and high only, N item(s), no research"`, `"no headroom:
nothing claimed until <until>"` when every account is exhausted, or
`"stopped: nothing new until <until>: <why>"` when the node's own mode
is stopped but not every account is (an api account still `ok` while
every subscription is exhausted); a goal or two left unclaimed this
pass appends `"; k item(s) and m plan(s) left unclaimed"`.

This item (1 of the goal) is the report as data, exercised without a
loop or a platform; item 2 wires the loop's own unclaimed accounting
(`Unclaimed{GoalID, Kind, Count, Reason}`) and the heartbeat call.

What counts as passed over (item 2): ready work the pass left for
headroom, never for capacity, a claim refused or a runtime missing —
in `claimReady` an item no account has headroom for
(`accountFor` false), a normal or low item skipped because the only
free slots are the urgent-only reserve at the end of an api key's day,
and an item of a normal or low goal a ranged node at its min does not
build; in `planRequested` a goal in planning no account has headroom
for (kind `plan`). `Loop.passOver` keeps one `Unclaimed` per goal and
kind per pass, its `count` the items, and one sentence of reason from
`heldReason`: the goal's priority, what held it (the reserve, the
min), else each account's standing from its row of the report
(`holdingAccounts`) — e.g. `normal priority; only urgent and high
while paced (mesh - max20 until 2026-09-28T18:00:00Z); claude -
shhemail exhausted until 2026-10-03T03:00:00Z`. At the end of the pass
`Tick` builds `HoldingReport` and keeps it (`Loop.Holding`); since the
heartbeat goes out *before* the pass, each heartbeat carries the last
pass's report (none before the first). A draining node makes no pass
and sends its last report unchanged. A node with no accounts is the
operator's own login, one implicit account: its report is sent, not
nil — mode `normal`, the account `unknown` until a reading (item 1's
tests take it). The log lines `limits: paced …` and `accounts: no
account has headroom …` stay as they were. The
platform half is runsheet goal `goals-page-shows-account-limits`, with
the contract fixture `specs/contracts/heartbeatNode.json` (runsheet
ADR 026).

## Amendment — 2026-10-02 (goal builtin-is-mesh-context)

The built-in attachment is `mesh-context` (`tools.Builtin`, mesh-cli
#264/#396); `prologue` is the deprecated alias read until 2026-10-28
and no longer the name a current session carries. "Priced tool calls
go to `recordToolUsage`" above said `Prologue` sends no record; it now
says `mesh-context`, since that is the built-in's own name. Nothing
about what is reported or when changes — only the word.
