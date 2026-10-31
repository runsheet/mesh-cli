---
summary: A v1 lease's relay injects the credentials its offer carried (access.secrets, access.mint) on the hosts each credential's Latchkey rule names and the kind's egress allows, as one egress.Rule per ref on ADR-045's TLS termination. Each is asked of Latchkey's for-job route on the lease's session token as the lease opens (a refusal hands the lease back credential_timeout or product), held in memory only, asked again at its expiry and at least every 15 minutes, written in its form (header, bearer, basic, query, template), refused 403 credential_preset when the sandbox set it itself (the placeholder `mesh` excepted), refused 502 credential_unavailable or credential_refused when it cannot be had, and audited by ref, form and version, never a value; every injected value, in every encoding, reads [secret:<ref>] in the node's log, its reports, the transcript and the session's errors, and is a canary whose sight in a request body, or toward a host outside its rule, ends the job credential_exposed (mesh-specs gap 28, node half; gap 5 item 4).
status: accepted
---

# ADR-046: The relay injects the credentials a job carried

2026-10-31. Mesh v1, mesh-specs gap 28 (the relay injects static
secrets and scoped-mint children), on ADR-045's TLS termination.
Latchkey's side: latchkey ADR 031 (Secrets, #692) and ADR 027 §9
(scoped mint per job, #694). Applies to `internal/nodev1` and
`internal/egress`.

## Context

A job's offer now carries credentials by ref (projectmesh-io/api #80,
"carried access"): `access.secrets` (Latchkey Secret refs),
`access.mint` (`{connection_ref, permissions[], resources[], ttl}`, a
scoped child of a tenant's connection) and `access.federation` (gap 32,
not this ADR). A Namecheap key, a Cloudflare token or a webhook secret
has had no way into a session but an environment variable, which the
runsheet v1 plan rules out: the pod must never hold one.

## Decision

1. **The brief reads them.** `meshapi.Access` gains `Secrets`, `Mint`
   (`MintGrant`) and `Federation` (`FederationRef`, read as the bare ref
   or `{ref, purpose?}`); absent is nil and changes nothing
   (`take-carried` fixture).

2. **Asked once as the lease opens, on the session token.** For each
   Secret ref and each mint's `connection_ref` the node calls
   `POST <Latchkey>/v1/credentials/for-job {credential_ref}` with
   `Authorization: DPoP <mesh-session token>` and a fresh proof
   (`sessionTokens.CredentialForJob`, the model credential's client).
   Latchkey reads the scope from the job itself; the node names only the
   ref. The answer — `{inject, fields, version?, expires_at,
   revocable?, connection?, service?}` — is held in the node's memory
   alone (`heldCredential`): never written to disk, a log line, an
   event, an AccessRecord or evidence. A ref Latchkey will not give
   hands the lease back before the session runs: cause `product` when
   Latchkey refused the product's configuration (a 4xx other than 401
   or 429: `not_the_jobs`, `purpose_not_granted`,
   `connection_not_found`, `connection_needs_reauth`,
   `scope_exceeds_grant`, `minter_not_enabled`, `invalid_scope`), else
   `credential_timeout` (unreachable, 401, 429, 5xx, `vendor_refused`,
   `vendor_unreachable`; gap 14).

3. **One rule per ref, on the egress it already has.**
   `egress.InjectRule` terminates exactly the kind's egress hosts (as
   the tenant narrows them) that the credential's rule names (exact, or
   one leading `*.`): never another, so injection never widens egress.
   A ref whose hosts the egress names none of is held but injected
   nowhere, and said. The lease's CA is made over every rule's hosts
   with the registries' (ADR-045). A lease whose kind declares no
   egress, or whose session runs on the host, injects nothing, and says
   so.

4. **Forms.** On a request whose method and path the rule's optional
   `methods` and `paths` match (a dot segment never does), the
   credential of the moment is written:
   `header` — `{header}: {prefix}{value}`; `bearer` —
   `Authorization: Bearer {value}`; `basic` — `Authorization: Basic
   base64(user:pass)`, from the rule's `user`/`pass` templates or the
   fields `user`/`pass` (`username`/`password`); `query` — each `params`
   entry rendered as a parameter; `template` — each `headers` entry
   rendered. Templates are `{{field}}` substitutions only; a field the
   credential lacks refuses the request. A connection's child comes with
   Latchkey's `{hosts, header, prefix}` and no form: it is the header
   form. The value of header and bearer is the rule's `field`, the
   only field, or the first of `access_token`, `token`, `api_token`,
   `api_key`, `key`, `value`, `secret`. `signing` is refused by
   `InjectSpec.Check` until it is built.

5. **The sandbox may not choose its own value.** A request that
   already carries a header or parameter the rule writes is answered
   `403` with `X-Mesh-Error: credential_preset` and never forwarded —
   unless what it carries is the placeholder `mesh` (`mesh`, a scheme
   and `mesh`, Basic of `mesh:mesh`), which is replaced. The placeholder
   is what the pod holds where a tool insists on a variable: the
   variables the rule's `env` names (not yet in Latchkey's answer) and,
   for a connection, its service's (`CLOUDFLARE_API_TOKEN`,
   `GH_TOKEN`/`GITHUB_TOKEN`, `GITLAB_TOKEN`), set in the pod's proxy
   environment for its agent, steps and sidecar.

6. **Re-asked, failing closed.** A held credential is asked again on
   the first request after `expires_at` less 30 s or 15 minutes after
   it was asked (latchkey ADR 031's `cache_for`), whichever is first,
   so a grace-0 rotation reaches a running job within 15 minutes and a
   child is renewed before it lapses. A credential past its time is
   dropped before the re-ask: when Latchkey refuses, the rule's
   requests are answered `502` with `X-Mesh-Error:
   credential_refused` and Latchkey's code and status in the body; when
   it cannot be reached, `credential_unavailable`. Nothing the sandbox
   sent goes on in its place and nothing stale is reused.

7. **Audited without a value.** Each request a rule acted on is an
   AccessRecord `{kind: api, vendor: secret | connection, service:
   inject <form>, method, path: <ref>, outcome: ok | refused | failed,
   reason: <status> [version <v>] | <the relay's code>, bytes}`.
   Latchkey's own rows carry the job and lease.

8. **Redacted wherever it might echo** (gap 28 item 5, amended
   2026-10-31). Each credential is registered with the lease's
   `injectedValues` as it is taken, before its first use, in every form
   it could be seen in: the value, its URL-encoded forms, its base64 in
   the standard and URL alphabets (padded, and the stable middle at each
   of the three alignments, so it is found inside a longer base64 run),
   and a Basic header's `user:pass` and its base64. Values shorter than
   8 bytes (a username) are neither redacted nor watched. Every form
   reads `[secret:<ref>]` in one place each output passes through:
   `Runtime.logf` (every node log line, and so the stop rules', the
   relay's and the tools' lines) and the reasons sent to Mesh
   (`fail`, `handBackExit`) redact every live lease's values, held from
   the take to the release; the transcript chunks the node signs for
   the product scrub them first (`transcript.Options.Named`,
   `tripline.Options.Named`, whose marks are kept from the patterns by
   stand-ins); the session's errors and its stderr (`SessionSpec.
   Injected`, `streamRedact`, holding back a value split across writes).
   Tool results and command output reach the product only in the
   transcript and Mesh only in those reasons, so they pass through the
   same.

9. **The values are the lease's canaries** (gap 5 item 4, gap 28 item
   5). A lease that injects anything terminates every host its egress
   allows (`watchRule`, writing nothing), not only the injected ones, so
   every request it sends through its tunnel is read; a lease that
   injects nothing is spliced as before. The tunnel's `Inspector` sees
   each request as the session sent it, before any rule: a value in its
   URL or headers toward a host outside the value's rule is refused
   `403 credential_exposed` and never sent (toward its own host it is
   left to the rule's preset refusal). Its body is scanned as it streams
   (`egress.ScanBody`): a rolling window holds back the longest form's
   length less one byte until the next read rules a split value out, so
   nothing is buffered and a value never leaves whole; a find cuts the
   request short and answers `403 credential_exposed` when the answer
   has not begun. The model relay, which holds each body whole, refuses
   one holding a value the same way (`sessionproxy.Model.Inspect`). Any
   exposure stops the session at once and fails the lease
   `stopped_by_rule` (Mesh takes no other terminal cause) with the
   reason `credential_exposed: the value of <ref> (version <v>) was seen
   in <where> toward <host>`, posts a `dlp` security report with rule
   `credential_exposed`, and logs that Latchkey is not told: #692
   serves no `secret.exposed` route, so the version is not marked
   exposed there.

10. **The lease's end releases** (gap 28 item 6). However the lease
    ends — finished, failed (an exposure included), handed back before
    or after its session, cancelled by Mesh, past its budget, the node
    stopping — and before its end is told to Mesh, which revokes its
    Latchkey session, `releaseCarried` drops every held credential (no
    rule injects after) and, when the access minted children, calls
    `POST /v1/credentials/for-job/release {}` on the session token
    (latchkey #694, `{released: n}`), bounded by 15 s. The answer is an
    AccessRecord of the lease `{vendor: latchkey, service: release,
    outcome: ok, reason: released <n>}` or `{outcome: failed, reason:
    release_failed: <why>}`, which is not one of the lease's writes; a
    failure never holds the lease open, and Latchkey's backstops (the
    session's end revoking its children, the five-minute sweep) revoke
    what it missed. A lease whose access names only Secrets drops them
    and calls nothing.

## Consequences

- A job reaches a vendor's API with `curl` and nothing in its
  environment but placeholders.
- Every injected host is terminated (ADR-045's cost: HTTP/1.1 to the
  session, the relay reading its plaintext) for the leases that carry
  credentials, only.
- Latchkey's for-job route serves connection refs today (#694); a
  Secret ref is answered by it only from ADR 031's phase 2 (rollout
  step 3). Until then the route reads a Secret ref's ask as the model
  credential's (no `fields` in its answer, or a refusal), and the lease
  is handed back before its session runs.
- A request that echoes a value toward a host the egress does not
  allow is refused by egress before anything is read; one by a client
  that ignores the proxy finds no route. A host that echoes a value
  back in its answer is not scanned: answers are not read.
- Terminating every egress host of an injecting lease costs HTTP/2 to
  the session (ADR-045) on all of them, for those leases only.
- Two rules that write the same header to the same request conflict:
  the second finds it set and refuses `credential_preset`.

## Amendment 2026-10-31: federation targets (mesh-specs gap 32)

`access.federation` (`[{ref, purpose?}]` or bare refs, projectmesh-io/api
#80) is now served. Latchkey's side is latchkeyid/latchkey #693 (ADR 031
§3 as built): one route for a product's workload and a job's relay, not
the for-job route this ADR's item 2 uses.

1. **Minted on the session token.** For each ref the node calls `POST
   <Latchkey>/v1/federation/token` with `Authorization: DPoP <mesh-session
   token>`, a fresh proof and `{"target": <ref>, "purpose": <purpose>}`,
   nothing else (Latchkey refuses `org`, `project` or `environment` from
   a session). The purpose is the offer's, else the kind's name (its last
   `/` segment, lower-cased, as Latchkey reads the job's kind). The
   answer is `{subject_token, issued_token_type, expires_in, target_ref,
   cloud, exchange}`; the subject token lives five minutes and names the
   job, kind and lease.
2. **Traded by the node, at Google alone.** For `cloud: gcp` the node
   posts STS's token exchange to `exchange.sts_url` (`grant_type`
   token-exchange, `audience`, `subject_token_type`,
   `requested_token_type` access_token) and, when the target names a
   `service_account`, `generateAccessToken` at
   `<exchange.iam_credentials_url>/projects/-/serviceAccounts/<sa>`
   with `scope` the target's `scopes` and `lifetime` `<exchange.duration>s`.
   STS is asked for `cloud-platform` when a service account follows
   (generateAccessToken takes no narrower federated token; Latchkey's own
   verify does the same), else for the target's scopes. Before any token
   is sent, each URL must be `https` on exactly `sts.googleapis.com` or
   `iamcredentials.googleapis.com` (no user, no port but 443): anything
   else is refused `gcp_sts`/`gcp_iam` `invalid_exchange`, and no
   redirect is followed. Only this package's tests can point the trade at
   a fake (`Runtime.federationHTTP`, `federationCheck`, unexported); no
   build, flag or variable can.
3. **Injected as bearer on the kind's Google hosts.** One
   `egress.InjectRule` per target over the kind's egress hosts under
   `*.googleapis.com` that the egress allows, exactly as item 3 (never
   widening egress; a literal `*.` entry is not a host). A host one
   target writes is not another's, and a target with no host left is not
   minted, which is said. Federation rules come before secrets', so a
   secret's rule on the same host still finds the header set and refuses.
   The rule `Overwrite`s: the Google provider and `gcloud` always send an
   Authorization, so the one the sandbox sent is replaced, not refused
   `credential_preset`. The pod holds `GOOGLE_OAUTH_ACCESS_TOKEN=mesh`
   and `CLOUDSDK_AUTH_ACCESS_TOKEN=mesh`.
4. **Refreshed, then dropped.** Minted and traded once as the lease
   opens; again on the first request within a minute of the cloud
   token's end (`expireTime`, else `duration`), for as long as the lease
   lives, the old token dropped first; dropped at the lease's end with
   the held credentials (`releaser.fed`, run by `releaseCarried` before
   Mesh hears of the end), after which the rule refuses
   `credential_unavailable`. Nothing is kept across leases, and nothing
   is released at Latchkey: a subject token is not revocable and dies
   in five minutes.
5. **Failure is closed.** A request finding no token is answered `502`
   with `X-Mesh-Error: credential_refused` and, in the body, Latchkey's
   code (`target_not_found`, also for the bare 404 of an org without the
   `federation` feature; `target_disabled`; `purpose_not_granted`;
   `not_the_jobs`) or `gcp_sts`/`gcp_iam` with Google's error (STS's
   `error`/`error_description`, IAM's `status`/`message`), or
   `credential_unavailable` when Latchkey (401, 503, unreachable) or
   Google (5xx, unreachable) did not serve. At the lease's opening the
   same failure hands the lease back, its reason naming the code: cause
   `product` for Latchkey's 4xx other than 401/429, a Google 4xx other
   than 429, or a cloud the node does not trade; `credential_timeout`
   otherwise and for an exchange URL that is not Google's (Latchkey's
   fault, not the product's).
6. **Redacted and audited.** The subject token, STS's federated token
   and the cloud token are each handed to the lease's redactor
   (`injectedValues.add`) as they are had, before they are sent
   anywhere, so each reads `[secret:<target ref>]` wherever it might
   echo and is one of the lease's canaries; a lease with a federation
   rule terminates every egress host for them, as one with a secret
   does. Error text is scrubbed of the tokens it was sent.
   Each request is an AccessRecord `{vendor: federation, service: inject
   bearer, path: <ref>, reason: <status> purpose <purpose>}`.

Not yet: AWS (`AssumeRoleWithWebIdentity`, then `aws_sigv4` re-signing)
and Azure (a client assertion, then bearer) and generic OIDC. Their
targets hand the lease back `product` `cloud_not_supported`;
`heldFederation.tradeFor` is the seam they join at.
