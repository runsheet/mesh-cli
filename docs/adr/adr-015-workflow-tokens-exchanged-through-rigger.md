---
summary: A workflow's cross-repository credential is exchanged from the run's GitHub OIDC identity through the runsheet gateway (Rigger's exchangeWorkflowToken) against a binding file the target repository carries — one repository, the granted permissions, at most an hour, journalled — never a stored token; scripts/testdata/exchange-workflow-token/ is the contract, and RELEASE_TOKEN is a transitional, logged fallback that lives in scripts/mirror-token.sh alone, one function its removal deletes.
status: accepted
---

# ADR-015: Workflow tokens are exchanged through Rigger

2026-09-30, goal `release-tag-mirrors-with-a-rigger-binding` (item 2).
Changes how `release-tag.yml`'s mirror step gets its token (amends
ADR-002's "the public mirror on runsheet/mesh"). The platform half is
runsheet goal `rigger-binds-a-workflow-to-a-scoped-github-token`, under
Rigger (runsheet TRD §6f, ADR 005, Rigger goal 2's `IdentityBinding`).

## Context

`release-tag.yml` publishes every tag's signed release on
`runsheet/mesh-cli` and then mirrors it to the public `runsheet/mesh`
(ADR-002). The workflow's own `GITHUB_TOKEN` cannot write another
repository, so the mirror step ran on `RELEASE_TOKEN`: a fine-grained
personal token stored as a repository secret. On 2026-09-29 at 00:03Z
the mirror step failed with `HTTP 403: Resource not accessible by
personal access token`. The token had been made by `projectmesh-io` on
2026-04-18; the mirror had since moved to `runsheet`, and nothing said
so until a release broke. The operator made a new one by hand to ship
`v0.2.188`. It still belongs to a person and still expires.

Rigger exists to end exactly this — "no long-lived key anywhere": the
platform exchanges an identity it can verify for a short-lived, scoped
credential and journals the exchange. A GitHub Actions run already has
such an identity: its OIDC token (the publish job has `id-token: write`
for Sigstore), whose claims name the repository, the workflow and the
ref. A GitHub App installation token scoped to one repository is the
simplest binding there is, and ADR 032's attested validation needs the
same broker to fetch attestations with a run's OIDC token.

## Decision

- **A workflow's credential on another repository is exchanged, never
  stored.** The workflow requests its OIDC token with audience
  `runsheet-rigger` and POSTs it as the bearer to the runsheet gateway's
  `exchangeWorkflowToken(repository, binding)`; the answer is `{ token
  expiresAt repository permissions }`.
  `scripts/exchange-workflow-token.sh` does it: the token on stdout,
  `expiresAt` on stderr, and exit 1 with the reason — a refusal, a
  platform without the mutation, no OIDC token — on stderr.
- **The target repository carries the binding.** The platform checks
  the JWT's signature and its claims against
  `.runsheet/bindings/<binding>.yaml` on the target repository: issuer,
  audience, the source repository, the workflow (`job_workflow_ref`),
  the ref pattern. A repository opts in to being written by committing
  the file; whoever can change it owns the grant. For the mirror it is
  `runsheet/mesh`'s `.runsheet/bindings/mesh-cli-release.yaml` —
  `runsheet/mesh-cli`, `.github/workflows/release-tag.yml`,
  `refs/tags/v*`, `contents: write`, `ttl: 1h` — kept here as
  `dist/mirror/.runsheet/bindings/mesh-cli-release.yaml` and published
  by `scripts/mirror-release.sh`'s `publish_file`, as the README is
  (PUT only when its content differs).
- **The token is the least that works.** An installation token on one
  repository, the binding's `grants` and nothing more, a TTL of at most
  an hour. The platform journals every exchange, issued or refused, and
  never the token itself.
- **The fixture is the contract.**
  `scripts/testdata/exchange-workflow-token/` holds the exact request
  body, a success, a refusal and a platform without the mutation, and a
  README stating the claims checked, the token's scope and the journal.
  `scripts/exchange-workflow-token_test.sh` runs the script against them
  with a fake `curl`; the runsheet half copies them as its own fixtures.
  A change to either side changes the fixture first.
- **`RELEASE_TOKEN` is transitional.** Until the runsheet half answers,
  a failed exchange falls back to the secret, and the log says which
  path it took: `mirror token: exchanged through rigger, expires
  <expiresAt>` or `mirror token: RELEASE_TOKEN fallback (exchange
  failed: <reason>)`. With neither, the step fails naming both. The
  choice is `scripts/mirror-token.sh` (tested by
  `scripts/mirror-token_test.sh`), and the fallback in it is one
  function, `fallback_token`, and one read of the environment; the
  workflow only calls the script. Once a tag has mirrored on the
  exchanged token, the goal's item 10 deletes that function, its call,
  the workflow's `RELEASE_TOKEN` env line and the secret.

## Consequences

- No person's token sits behind the public release channel; a moved or
  renamed repository fails the exchange with a reason naming the claim,
  not a 403 on a token nobody remembers.
- Every write to the mirror is attributable to a run: the journal holds
  the workflow, ref, run and sha it was issued to.
- The mirror's writability is declared in the mirror itself, in review,
  rather than in a secret's settings page.
- The release now depends on the runsheet gateway being up at tag time.
  During the transition a gateway outage falls back to `RELEASE_TOKEN`;
  after it, a failed exchange fails the mirror step and the re-run
  repeats it.
- Any other workflow that needs a cross-repository credential — ADR 032's
  attestation fetch included — uses the same script with its own binding,
  not a new secret.

## Where it stands (2026-10-02)

Item 9 of the goal. `scripts/mirror-token-reading.sh` (item 6), run
live against `runsheet/mesh-cli` on 2026-10-02, reads every v* run of
`release-tag.yml` from `v0.2.226` to `v0.2.235` (runs 37001059687 to
37038403839, 11:26Z to 17:05Z) as `fallback`, each refused with
`unauthenticated: the bearer is a JWT with no role (unknown key id)`,
the earlier `v0.2.222`–`v0.2.225` the same — except `v0.2.234`, which
never reached the mirror step (`Install cosign` failed first). No run
has mirrored on an exchanged token yet.

The refusal is the gateway's authentication, before any binding is
read: it does not recognise GitHub's OIDC issuer,
`https://token.actions.githubusercontent.com`, for the audience
`runsheet-rigger`, so it looks the JWT's `kid` up among its own keys,
finds none, and calls the bearer a JWT with no role. Nothing on this
side changes that; it waits on the runsheet goal
`rigger-binds-a-workflow-to-a-scoped-github-token`, which teaches the
gateway that issuer and its JWKS and answers `exchangeWorkflowToken`.

To make that half's acceptance checkable from a run log,
`scripts/exchange-workflow-token.sh` now prints, after a refusal's
reason, one line `presented: kid=… iss=… aud=… sub=… repository=… ref=…
job_workflow_ref=…` — the JWT's header `kid` and claims, decoded
unverified, never the token or its signature — and the mirror step's
choice of token moved out of the workflow into
`scripts/mirror-token.sh`, where the fallback is the one function item
10 deletes. The `mirror token: ` lines are unchanged, so
`scripts/mirror-token-reading.sh` reads the runs before and after alike.
