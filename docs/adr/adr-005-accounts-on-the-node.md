---
summary: A node's sessions run on Claude accounts listed in node.yaml (`accounts:` of {label, kind subscription|api, token_file|api_key_env}) until the platform hands a session its credential at claim; the secret reaches the session in its environment only (CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY), with a temporary CLAUDE_CONFIG_DIR per session removed after, the inherited auth variables stripped, and the secret redacted from every log, error and report; each session runs on the account ChooseAccount picks by headroom and the spending order (subscriptions first, API only for urgent/high work or when subscriptions are stopped and work waits), and no account with headroom means no claim.
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
## Consequences

- Several Max subscriptions work on one node from node.yaml before the
  platform's credentials exist; the operator's own login is untouched.
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
- Redaction is by exact value: a secret split across two stderr writes
  can pass the stderr filter, though not the error, which is redacted
  whole.
