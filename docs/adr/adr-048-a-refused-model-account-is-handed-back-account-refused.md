---
summary: A v1 node logs a model vendor's error type and message on every 4xx/5xx (bounded to 300 characters, redacted of the lease's values, never a body or token), and a vendor's 401 or 403 on the job's model account ends the session at once and hands the lease back account_refused with {account_ref, vendor, status, error_type, error_message}, so Mesh cools the account down and offers the job again on another account without counting it; 429, 5xx/529, other 4xx and the relay's own refusals are not refusals; a Mesh that answers the cause 422 hears cannot_run.
status: accepted
---

# ADR-048: A refused model account is handed back account_refused

2026-11-01. Mesh v1. Mesh's side is projectmesh-io/api's ADR 001
amendment "account failover". Applies to `internal/sessionproxy` (the
model relay) and `internal/nodev1`.

## Context

On 2026-11-01 between 18:42 and 18:53 AEDT six leases on dl360-v1 ran on
one model account whose vendor answered every `POST /v1/messages` 403.
The node logged only `model anthropic POST /v1/messages answered 403, 197
bytes`; the session exited 1 (`claude: sandbox: agent exited 1`); the node
handed each lease back `cannot_run`; Mesh offered the job again on the
same account, and after `max_handbacks` (3) held it, though two other
accounts granted for the kind were healthy.

## Decision

- **The vendor's error is logged.** On any answer of 400 or more from
  the model vendor the relay reads, from what it streams back to the
  session (the first 16 KiB, gzip or plain), the vendor's error type and
  message alone: Anthropic's `{"type":"error","error":{"type","message"}}`
  or OpenAI's `{"error":{"type"|"code","message"}}`. The message is
  bounded to 300 characters, its control characters spaced, and redacted
  of the lease's values (the relay's mask: the model credential and the
  injected values' canaries). The call record (`sessionproxy.ModelCall`)
  carries `ErrorType` and `ErrorMessage`; the node's line ends
  `: <type>: <message>`. A body that is neither shape, and every request
  body, header and token, is never logged.
- **What counts as a refusal.** The vendor's final answer to a model
  call — after the relay's one re-read of the credential on a 401 — of:
  - **401**, any error type: the credential is not accepted;
  - **403**, any error type: the account may not do this.

  Not a refusal: 429 (the account's limits: Mesh's "account full"),
  5xx and 529 (the vendor's outage), any other 4xx (the request's own),
  and a 403 the relay answered itself (status 0 in the call record:
  nothing reached the vendor).
- **Only the job's account.** A lease that runs on the account Mesh
  picked (a `credential_ref` the node read from Latchkey, with an
  `account_ref`) is handed back for it. A session on the node's own
  login is not Mesh's account to fail over, and behaves as before.
- **The lease ends at once.** The first refusal ends the lease's context
  with the refusal as its cause, which stops the session; the node does
  not wait for the harness to give up. The lease is reported
  `account_refused` after the exposure and stop-rule checks (a
  credential exposed still fails the job) and before any other end: a
  session that finished first keeps its finish. Usage and access records
  post first, as for every hand-back; a lease that pushed is handed back
  all the same, since the next lease builds on its pushes.
- **The hand-back** is `POST /v1/leases/{id}/hand-back` with
  `cause: account_refused`, a reason naming the account, the call and the
  vendor's answer, and `account: {account_ref, vendor, status,
  error_type, error_message}` (fixture
  `lease-hand-back-account-refused.json`). Mesh does not count it,
  cools the account down and offers the job again on another account.
- **An older Mesh** answers the cause 422 (`refused`): the node hands
  the lease back `cannot_run` with the same reason, as before this ADR.

## Consequences

- The incident's six leases would each have been handed back once,
  uncounted; the account cooled down for 10 minutes; and the jobs taken
  on the other two accounts.
- A vendor whose 403 is about the request rather than the account (a
  model the account may not use) cools that account down for every job.
  Mesh's amendment names the trade.
