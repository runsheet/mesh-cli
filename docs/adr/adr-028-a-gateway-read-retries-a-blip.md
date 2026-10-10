---
title: "ADR-028: A gateway read retries a blip"
summary: The gateway client retries every read, and the three credential-minting mutations, on a transport error, timeout, 429 or 5xx — each try under 30s, for 2 minutes on a 1-2-5-10-20s backoff — and a window spent ends "after N tries over D", which a claim-time read carries into the item's lastError.
status: accepted
---

# ADR-028: A gateway read retries a blip

2026-10-10, goal `a-node-retries-a-gateway-timeout` (items 1 and 2).

## Context

On 2026-10-10 between 08:52 and 09:19Z the platform deployed three
times back to back. Three items failed inside that half hour with
`github token: Post "https://api.runsheet.dev/graphql": context deadline
exceeded` or `session credentials: … deadline exceeded` (runsheet
the-integrator-merges-a-batch-on-one-run#54,
the-platform-reads-a-token-by-ref-or-slug#18, grapevine
applying-follows-the-business#3). Each was one call that outlasted the
client's timeout while the gateway rolled over; the node failed the
whole item on it. One slow or dropped gateway call is not a reason to
fail a work item.

## Decision

**The retry lives in the gateway client, not at the call sites**
(`internal/gateway/client.go`, `retryRead` over `retryTransient`), so
every read gets it: the claim-time reads (`itemCredentials`/`itemTools`,
`planCredentials`, `researchCredentials` — `Client.Session`), the
item's, plan's and research's GitHub tokens, the index lists, and the
mesh-context GETs (`contextGetAt`).

### What is retried

- Every `query` document, and the shorthand `{ … }` (`retriesRead`).
- A mutation whose every root field is on `retriedMutations`:
  `issueItemRepoToken`, `planGitHubToken`, `researchGitHubToken`. Each
  asks the platform to mint a credential for a claim the node already
  holds; a second answer only replaces the first, so sending it again is
  safe.
- Only a transient answer (`isTransient`): a transport error, a timeout
  (the try's own deadline), an HTTP 429, or any 5xx — a bare one or a
  GraphQL error body carried on a 5xx. A `Retry-After` is honoured,
  capped at the table's largest wait.

### The window, the per-try timeout and the backoff

- `gateway.ReadRetryFor`, **2 minutes**, counted from the first try. A
  further try is made only when its wait ends inside the window; else
  the last answer is returned at once rather than after one more call.
- `gateway.ReadTryTimeout`, **30 seconds**, bounds each try with a
  context of its own, so one stalled call cannot spend the window. The
  `http.Client`'s 60s timeout stays above it, and the caller's ctx still
  bounds the whole: a node shutting down ends the retry with
  `ctx.Err()`.
- `gateway.ReadBackoff`, **1s, 2s, 5s, 10s, 20s**, the last repeated.

All three are package variables a test shortens
(`internal/node/gatewayretry_test.go`, `shortReadRetry`); no test waits
minutes.

### What is never retried, and why

- **Every other mutation** — `claimWorkItem`, `finishWorkItem`,
  `failWorkItem`, `releaseWorkItem`, `evaluateWorkItem`, `planGoal`,
  `heartbeatNode`, `registerNode`, the usage and tool-usage records — is
  a command sent exactly once. A command whose answer was lost may have
  been applied; sending it again could claim twice, finish twice, or
  count a cost twice. Those keep their own handling (a heartbeat is
  sent again next tick; a finish refused is an error).
- `recordValidationAttestation` keeps its own retry
  (`retryOnTransient` on `AttestationRetryFor`/`AttestationBackoff`,
  adr-014's amendment), now on the same loop, and its own message,
  `after N retries over D`.
- **An answer that is not transient** returns at once: a refusal (a
  GraphQL error on a 200), an unsupported operation (`IsUnsupported`,
  the fallbacks of an older platform), a 4xx other than 429. Asking
  again would get the same answer.

### What a spent window says

A window spent with no success returns the last answer wrapped as
`after N tries over D: <last answer>` — N counting the first try, D the
elapsed time rounded to the millisecond — and `errors.As` still reaches
the wrapped `*gateway.Error` or HTTP error beneath it.

On the node nothing defeats it: no claim-time path puts a deadline
shorter than `ReadRetryFor` on the read, and each call site wraps the
error with `%w` ahead of the text. A claim-time read the window ran out
on goes where it went before — `Runner.fail`, so `FailWorkItem`, never a
release (a failed credentials read is not one of the node's own
conditions: `ErrNoCredential` and `ErrNoAccount` release, a workspace
failure releases by ADR-024, anything else fails) — and the item's
lastError opens with its call site's prefix, for example:

```
session credentials: after 9 tries over 1m52.3s: gateway: 503 Service Unavailable
github token: after 9 tries over 1m51.8s: Post "https://api.runsheet.dev/graphql": context deadline exceeded
```

(the last answer's text as the client renders it). `Runner.fail` keeps
the first 2000 characters, so the count is always kept.

## Consequences

- A gateway down for a deploy's rollover — a call or a few under two
  minutes — no longer fails an item; one down for longer fails it with
  a reason that says how hard the node tried.
- A claim-time read can now take up to about two and a half minutes to
  fail, where it took 60 seconds; the claim's own heartbeat is not
  running yet, but the platform's stall window is far longer.
- The `gatewaytest` fake answers a field transiently by
  `TransientFirst`/`TransientAs` — now on any mutation and the
  claim-time read fields, with a `stall` that hangs until the client's
  try gives up (`StallFor` caps it) — for tests of both paths.
