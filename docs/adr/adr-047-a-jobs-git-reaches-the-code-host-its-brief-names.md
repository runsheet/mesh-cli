---
summary: A v1 lease's git relay reaches the code host its brief's git names (mesh-specs gap 29, node half). The checkout dials https://<git.host>, else the origin of git.base_url with the API path dropped and a public vendor's API host mapped to its git host, else the vendor's own; the token is minted at Latchkey's for-job source route with the brief's vendor and base_url as the brief has them and put on in the vendor's Basic shape (GitLab oauth2, Bitbucket x-token-auth); apply_onto fetches a chain member's head by the vendor's ref (GitLab refs/merge-requests/<n>/head, Bitbucket by its branch or sha); the push fence is unchanged; every git token is redacted and a canary under git:<repo>; a relay sidecar may dial the code host its own brief names and no other; a brief with no vendor or base_url reaches github.com as before. Latchkey mints no GitLab or Bitbucket job token yet (501).
status: accepted
---

# ADR-047: A job's git reaches the code host its brief names

2026-10-31. Mesh v1, mesh-specs gap 29 ("A job's git grant names its
code host", amended in mesh-specs #202 to `vendor`/`base_url`), node
half. Mesh's side is projectmesh-io/api #80 and #82. Latchkey's side is
`POST /v1/credentials/for-job/source` (ADR 023's amendment of
2026-10-29), which mints GitHub alone today. Applies to `internal/nodev1`,
`internal/nodetools` and `internal/gitrelay`.

## Context

A brief's git is `{vendor: github|gitlab|bitbucket, base_url?, host?,
repo, access, read_repos?, push[]}`. `vendor` is absent for github.
`base_url` is the code host's https **API root**, such as
`https://gitlab.acme.test/api/v4`, and is empty for the vendor's public
host. `host` is the hostname the checkout dials. `repo` may nest on
GitLab (`acme/platform/web`). The relay already had an adapter per
vendor (`gitrelay.Vendor`: repository layout and the token's Basic
shape), and the mint already sent `vendor` and `base_url` (#1676). But
the node took `base_url` as the git origin, so a GitLab API root became
`https://gitlab.acme.test/api/v4/acme/web.git`. It also fetched a chain
member's head only by the ref the brief named, and it neither redacted
nor watched the git token as gap 28 does an injected value.

## Decision

1. **The origin is the host the brief names** (`baseOf`). It is
   `https://<git.host>` when the brief names a host. Otherwise it is
   `git.base_url`'s scheme and host, with its path (the API root)
   dropped and a public vendor's API host read as its git host:
   `api.github.com` is `github.com`, `api.bitbucket.org` is
   `bitbucket.org`. With neither, it is "" for the vendor's own origin
   (`gitrelay.Vendor.Origin`: github.com, gitlab.com, bitbucket.org). A
   brief that names no vendor and no base_url reaches github.com
   exactly as before. `gitHost` is that origin's host name.
2. **The token is minted for the brief's code host.** The relay's mint
   is `POST <Latchkey>/v1/credentials/for-job/source {vendor, base_url,
   repository, access}` on the lease's DPoP session token, with `vendor`
   and `base_url` exactly as the brief has them (Latchkey compares them
   with the job's git as Mesh answers it), and `repository` the brief's
   path, nested or not. The relay puts the token on each request in the
   vendor's shape: GitHub `x-access-token:<t>`, GitLab `oauth2:<t>`,
   Bitbucket `x-token-auth:<t>` (HTTP Basic). The token is never in the
   sandbox: not in the session's environment, not in the checkout's
   config, and never on disk.
3. **A chain member's head is fetched by the vendor's ref**
   (`nodetools.ChangeRef`). `apply_onto` reads each `code.apply[].ref` as
   the vendor keeps a change's head. GitHub keeps it at
   `refs/pull/<n>/head` and GitLab at `refs/merge-requests/<n>/head`, and
   a brief naming one vendor's change ref on the other's host is read
   as that host's. Bitbucket keeps no change ref, so one is dropped and
   the head is fetched by its sha, or by its branch when the brief names
   the branch (`refs/heads/…`). Any other ref is fetched as named.
4. **The push fence is unchanged.** `create_only` and the listed refs,
   fast-forward-only unless a rule says false, no deletes, read repos
   read-only: all are the relay's, whatever the vendor.
5. **Every git token is redacted and a canary** (gap 28 item 5). Each
   token the relay or its sidecar is minted is handed to the lease's
   `injectedValues` before its first use (`addGitToken`), under the ref
   `git:<repo>`, its own host the brief's git host. It then reads
   `[secret:git:<repo>]` in the node's log, the reasons sent to Mesh and
   the transcript. Seen toward another host through a terminated tunnel,
   or in a request body, it ends the job `credential_exposed` like an
   injected value.
6. **Egress to a self-managed host is the lease's alone.** The
   in-process relay dials only its grant's origin, which is its brief's.
   A relay sidecar's tunnel adds to its fixed upstreams only its own
   brief's git host (`fixedUpstreams`), so a lease can reach a
   self-managed GitLab only when its brief names it. No other lease's
   pod can.

## Consequences

- Latchkey does not yet mint a GitLab or Bitbucket job token. Its
  for-job source route refuses `vendor: gitlab|bitbucket` with
  `501 "Latchkey does not mint gitlab tokens yet"`
  (`oidc/jobsourcevendor.go` `sourceVendor`: GitHub only). Before that,
  it refuses a nested repository with `400 "repository must be
  owner/name"` (`sessionRepository`). So a GitLab or Bitbucket lease is
  handed back `credential_timeout` at its checkout, with Latchkey's
  answer in the reason, until Latchkey serves it. Latchkey needs:
  - a `sourceVendorMinter` for each of GitLab and Bitbucket, minting
    through the tenant's code-host connection;
  - `baseURLOK` for a self-managed base_url;
  - a repository pattern that admits GitLab's subgroups.

  The minters exist for ADR 027 §9's scoped connections
  (`external/minters` gitlab: a project access token with
  `read_repository`/`write_repository`; bitbucket: a workspace OAuth
  token, not repository-scoped). The node needs no change when that
  lands.
- runsheet's Mesh v1 offers name a chain member as `refs/pull/<n>/head`
  whatever the host (`aggregates.PullHeadRef`). Item 3 reads that as
  the vendor's ref, so such offers work on GitLab and Bitbucket too.
- Fetching a Bitbucket head by sha needs the host to serve the sha. A
  brief that names the member's branch avoids relying on it.
