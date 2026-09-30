# The exchangeWorkflowToken contract

These files are the contract between mesh-cli's
`scripts/exchange-workflow-token.sh` and the runsheet gateway's
`exchangeWorkflowToken` mutation (runsheet goal
`rigger-binds-a-workflow-to-a-scoped-github-token`; mesh-cli
`docs/adr/adr-015-workflow-tokens-exchanged-through-rigger.md`). The
runsheet half copies them as its own fixtures; a change here is a change
to both, landed platform side first.

| File | What it is |
|---|---|
| `request.json` | The exact GraphQL body the script POSTs to `<gateway>/graphql`, with `Authorization: Bearer <the run's OIDC JWT>` and `Content-Type: application/json`. |
| `response.json` | A success: `token`, `expiresAt` (RFC 3339), `repository`, `permissions` (a JSON scalar, permission → level). |
| `refused.json` | A binding whose claims do not match the token (here the ref): a GraphQL error, `data: null`. The script exits 1 with the message. |
| `unknown-field.json` | A platform without the mutation: GraphQL's validation error. The script exits 1 with the message. |

## The claims the platform checks

The bearer is the GitHub Actions OIDC JWT the run requested with
`audience=runsheet-rigger`. The platform verifies its signature against
GitHub's JWKS, then checks every claim against the binding file the
*target* repository carries, `.runsheet/bindings/<binding>.yaml` on its
default branch (for this one, `runsheet/mesh`'s
`.runsheet/bindings/mesh-cli-release.yaml`, published from mesh-cli's
`dist/mirror/`):

| Claim | Must be |
|---|---|
| `iss` | `https://token.actions.githubusercontent.com` |
| `aud` | `runsheet-rigger` |
| `repository` | `runsheet/mesh-cli` |
| `job_workflow_ref` / `workflow_ref` | `runsheet/mesh-cli/.github/workflows/release-tag.yml@<ref>` — the workflow `.github/workflows/release-tag.yml` |
| `ref` | matches `refs/tags/v*` |
| `exp` / `nbf` | the token is current |

Any mismatch, an unknown binding or a binding that does not name the
`repository` asked for is a refusal naming the claim, as in
`refused.json`. Nothing is issued on a partial match.

## The token issued

- A GitHub App installation token scoped to **one repository** — the
  `repository` argument, which must be the repository carrying the
  binding.
- The permissions the binding's `grants` names and no more: here
  `contents: write`.
- A TTL of **at most one hour** (the binding's `ttl`, capped at 1h);
  `expiresAt` says when.

## The journal

The platform journals every exchange, issued or refused — the binding,
the repository, the claims it checked (`repository`, `workflow`, `ref`,
`run_id`, `sha`), the outcome and `expiresAt` — as Rigger's
`IdentityBinding` does for any credential it brokers (Rigger goal 2,
runsheet TRD §6f, ADR 005: no long-lived key anywhere). The token itself
is never journalled.
