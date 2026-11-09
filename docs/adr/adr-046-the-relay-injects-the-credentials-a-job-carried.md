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
   `credential_exposed` (amended 2026-10-31; `stopped_by_rule` on a Mesh
   older than projectmesh-io/api #83) with the reason `credential_exposed:
   the value of <ref> (version <v>) was seen in <where> toward <host>`,
   posts a `dlp` security report with rule `credential_exposed`, and,
   for a Secret, tells Latchkey the version is exposed (latchkey #697;
   see the amendment "an exposure is reported").

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

## Amendment 2026-10-31: an exposure is reported (latchkey #697, projectmesh-io/api #83)

Item 9's two stand-ins are replaced now that both sides serve an
exposure.

1. **The lease fails `credential_exposed`.** Mesh takes the cause
   (projectmesh-io/api #83, its ADR 001 amendment "credential exposed";
   contract `lease-fail-credential-exposed.json`): `POST
   /v1/leases/{id}/fail {"cause": "credential_exposed", "reason":
   "credential_exposed: the value of <ref> (version <v>) was seen in
   <where> toward <host>"}` → 200 `{end: failed}`. The reason names the
   ref, version and host, never the value (and passes the redactor like
   every reason). A Mesh older than #83 answers 422 to the cause: the
   node then fails the lease `stopped_by_rule` with the same reason, so
   no fail is lost (`Runtime.failExposed`).
2. **Latchkey is told of a Secret's exposure, before the session's
   token ends.** In `releaseCarried`, before the release and before
   Mesh hears the end and revokes the session, when the exposed ref is
   one of the job's `access.secrets`: `POST <Latchkey>/v1/credentials/
   for-job/exposed {"secret_ref": <ref>, "version": "<v>"}` on the
   session's DPoP token with a proof (latchkey ADR 031, "As built —
   rollout step 3"; `specs/contracts/secret-for-job.json`). Latchkey
   marks the version exposed and sends `secret.exposed` to the org; its
   answer `{exposed, already_exposed, exposed_versions, rotate, …}` is
   an AccessRecord `{vendor: latchkey, service: exposed, outcome: ok,
   reason: exposed|already_exposed version <v> of <ref> (exposed
   versions …)[; rotate it]}` and a node log line; a refusal (400
   `invalid_request`, 403 `not_the_jobs`, 404 `secret_not_found`, or
   Latchkey unreachable) is `{outcome: failed, reason: exposed_not_told:
   <status and code>}` and a log line asking for the rotation. Neither
   holds the lease open nor changes its cause. The "#692 serves no
   `secret.exposed` route" log line is gone.
3. **A connection's child has no exposure route.** A scoped
   connection's child (`access.mint`) is revoked by item 10's release
   at the lease's end, which is enough: it is the lease's alone. A
   federation target's tokens and the git token likewise end with the
   lease and its session. For those the node logs that Latchkey has no
   exposure route and tells it nothing.
4. **Latchkey #697's for-job codes.** `403 job_not_live` — the lease is
   no longer the job's live one — is not the product's configuration: a
   lease opening on it is handed back `credential_timeout` (and Mesh,
   whose lease it no longer is, answers the hand-back as gone).
   `404 secret_not_found`, `409 secret_not_injectable`, `409
   secret_disabled` and `409 secret_expired`, with `403 not_the_jobs`
   and `purpose_not_granted`, are the product's: `product`.

## Amendment 2026-11-01: the signing form (aws_sigv4, hmac_sha256)

Item 4's `signing` is now applied (mesh-specs gap 28 item 3; latchkey
ADR 031 §2.3, rollout step 4). Latchkey's rule names it flat, beside the
other forms' settings: `{form: signing, scheme, region?, service?,
header?, prefix?, field?, canonical?, ts_header?}`
(`internal/egress/signing.go`).

1. **`aws_sigv4 {region, service}`.** The secret's fields are
   `access_key_id`, `secret_access_key` and `session_token?`. The pod
   holds `AWS_ACCESS_KEY_ID=mesh` and `AWS_SECRET_ACCESS_KEY=mesh`, so an
   unmodified SDK or CLI signs with the placeholder key; the relay drops
   that signature and signs again (Signature Version 4, AWS's own test
   suite's vectors) with the real key, a fresh `X-Amz-Date` and, when
   the secret has one, `X-Amz-Security-Token`. It signs the host, the
   content type and MD5 and every `X-Amz-*` header the request carries;
   the canonical URI is the path as sent for `s3` and escaped again for
   every other service, as AWS's SDKs do.
2. **The payload hash.** A sandbox's `X-Amz-Content-Sha256` that is a
   SHA-256, `UNSIGNED-PAYLOAD` or `STREAMING-UNSIGNED-PAYLOAD-TRAILER`
   is kept: AWS checks it against the body, or signs none. Otherwise the
   body is read, up to 10 MiB, hashed and sent on; a larger body is
   `UNSIGNED-PAYLOAD` on `s3`, the one service that takes it, and
   refused `413 signing_needs_body` on any other. A body signed chunk by
   chunk (`STREAMING-AWS4-HMAC-SHA256-*`) is refused the same way: its
   chunk signatures chain from the placeholder key's seed.
3. **`hmac_sha256 {header, prefix?, field?, canonical, ts_header?}`.**
   The key is the rule's `field`, the only field, or the first of
   `secret`, `key`, `value`. `canonical: body` signs the body;
   `canonical: ts_body` signs `<unix seconds>.<body>` and writes the
   seconds to `ts_header` (decided here, the spec leaving it open: the
   form most webhook schemes use). The signature is lower-case hex after
   `prefix` (`sha256=`). The body is read whole, up to 10 MiB, else
   `413 signing_needs_body`.
4. **Preset.** As every form: a request carrying its own signature is
   refused `403 credential_preset` — for `aws_sigv4` an `Authorization`
   signed with a key other than `mesh`, an `X-Amz-Security-Token` other
   than `mesh`, or a presigned query (`X-Amz-Signature`,
   `X-Amz-Credential`, `X-Amz-Security-Token`); for `hmac_sha256` the
   header or `ts_header` set to anything but `mesh`.
5. **Redaction and canaries.** Every field is registered as item 8 says
   but the access key id, which every signed request carries in the
   clear in its `Authorization` and AWS answers in some responses: it is
   neither redacted nor a canary. The audit's form reads `signing
   aws_sigv4` or `signing hmac_sha256`.

A signing rule is the one rule that reads a body (`egress.Rule`'s
contract says so): it holds at most 10 MiB and puts it back for the
canary scan and the upstream. AWS federation (a FederationTarget's
`AssumeRoleWithWebIdentity`, then this form) is still gap 32's.

## Amendment 2026-11-01: the canary scan covers MCP and vendor API calls

Item 9 read only the tunnel's and the model relay's requests. A session
that got a value could still hand it to a product's MCP tool (which
might post it anywhere) or write it into a pull request through
Latchkey's vendor proxy. Now the relay's own routes are scanned too
(mesh-specs gap 28 item 5; `sessionproxy.Canary`).

1. **Requests.** Every MCP request's body — to a product's server, a
   slot's binding, a local server or the node's own tools (the finish
   among them) — and every vendor API call's path, query and body is
   scanned before it leaves. None of them is a value's own host, so a
   value seen is an exposure as item 9's: the call is refused `403
   credential_exposed` (a JSON-RPC error for MCP, `X-Mesh-Relay-Error`
   for a vendor call), the session stopped, the lease failed
   `credential_exposed` with `seen in an MCP request's body toward the
   MCP server <name>` or `a vendor API call's path or query|body toward
   <vendor>'s API`. A vendor call's streamed body (over 4 MiB) is
   scanned a MiB at a time with the last 4 KiB of the one before, so a
   value split across two is found before the second is sent.
2. **Answers.** What those servers and vendors answer passes through
   the proxy's mask, which now redacts every injected value as
   `[secret:<ref>]` before the session reads it (decided here: an
   answer is redacted, not an exposure, since the value never reached
   the session). The tunnel's answers are still not read (item 9's
   consequence stands).
3. **The relay sidecar holds no value.** In a pod with a relay sidecar
   the sidecar serves the product's MCP and the vendor calls but never
   holds the values, so it hands each scanned part to the node on the
   lease's socket (`POST /relay/scan`, 204 clean, 403 exposed) and each
   answer to be redacted (`POST /relay/redact`), when its credentials
   say `canary`. Either failing fails closed: the call is refused, the
   answer replaced by `relay: this answer could not be checked for
   injected credentials`. The node tools' calls, forwarded to the node
   unchanged, are scanned there.

## Amendment 2026-11-01: a push's content is checked

A git push was the other way a value could leave: committed to a file
or a message and pushed through the git relay, which read only the
command list. Now the relay reads the pack too (mesh-specs gap 28 item
5; `gitrelay/packscan.go`).

1. **Every object, as the object it makes.** As a push streams upstream
   its pack is parsed: each blob, tree, commit and tag is inflated and
   scanned a MiB at a time with a 4 KiB overlap; each delta is applied to
   its base and the object it makes is scanned, since git may copy a
   value's last bytes from the base and a delta's inserts alone would
   miss it. A delta's base is an object earlier in the pack, else (a
   thin pack) one the vendor already has, read from the node's mirror or
   the pushing checkout with hardened git and checked to hash to its id,
   so a planted object cannot stand in for it.
2. **The push is held back until it is clean.** The last 4 KiB of the
   push, the pack's checksum among them, go upstream only once the whole
   pack is read and clean: the vendor cannot accept a pack it never had
   whole, so a value found anywhere refuses the push (`403`, the git
   client says `the push carried a credential the relay injects`) with
   nothing updated, and the lease fails `credential_exposed` toward the
   repository, `seen in a git push's content`.
3. **Nothing goes unread.** A pack the relay cannot read — a sha256
   repository's `REF_DELTA`, a base neither in the push nor in a store
   the relay reads, a delta or base past 64 MiB, more than 512 MiB of
   bases held — refuses the push (`the push's pack could not be read`).
   Decided here: failing closed is the spec's rule, and a thin push's
   bases are what the checkout fetched, so they are there.
4. **Where.** The node's grant (`Grant.SetScan`, the lease's values,
   bases from its checkout) and a relay sidecar's relay (scanned on the
   node through `POST /relay/scan`, bases from the node's mirror, then
   the checkout it has read-only). Only a lease whose access carries a
   Secret, a scoped mint or a federation target has its pushes read, and
   only such a lease's sidecar hands its calls to the node (amending the
   amendment above, whose sidecar half is so gated too); the node's own
   proxy scans every lease's MCP and vendor calls, which costs nothing.

## Amendment 2026-11-01: AWS, Azure and generic OIDC federation targets (mesh-specs gap 32)

The federation amendment's "not yet" is built. Latchkey's side is
latchkeyid/latchkey's ADR 031 amendment "Federation to AWS, Azure and
generic OIDC": every `exchange` names `hosts`, an aws exchange its
`role_session_name` (`mesh-<lease>`), and an oidc target may be direct.

1. **The cloud is known from the first mint.** A target's cloud and the
   hosts its credential is written on come only in Latchkey's answer, so
   each target is minted as the lease opens before its hosts are chosen
   (`heldFederation.open`), and that subject token is the one traded
   while it is fresh (three minutes). A target whose hosts the kind's
   egress names none of is now minted but not traded — said, and still
   never widening egress. The rule's spec is fixed at that first mint.
2. **aws.** `AssumeRoleWithWebIdentity` at `exchange.sts_url`, which must
   be STS's own host on https (`sts.amazonaws.com`,
   `sts.<region>.amazonaws.com`, or the `.amazonaws.com.cn` partition's),
   with `role_arn`, `role_session_name`, `duration` and
   `session_policy`; the XML answer's keys are the credential
   (`access_key_id`, `secret_access_key`, `session_token`, the signing
   form's fields), ending at its `Expiration`. The rule is the signing
   form, `aws_sigv4` with **no region or service**, on the kind's egress
   hosts under `*.amazonaws.com` (`*.amazonaws.com.cn` when STS is the cn
   partition's); the pod holds the placeholder keys as for a Secret's
   rule. The secret and the session token are handed to the redactor as
   they are had and are canaries; the key's id is not, as for a Secret.
3. **The signing form's region and service may be absent.** One signer
   serves both gaps (`internal/egress/signing.go`, gap 28's): an
   `aws_sigv4` spec that names no region or service — a federation
   target's, whose role reaches every service, built by the node, never
   by Latchkey (whose Secret rules name both) — signs each request for
   the scope the sandbox's own placeholder signature names (its
   `Authorization`'s `Credential`), else the one its host's name says
   (`awsHostScope`: `<service>.<region>.amazonaws.com`, a bucket's
   virtual host, dual-stack, FIPS, or a global endpoint as `us-east-1`),
   else refuses `400 signing_scope_unknown`. `s3`'s single path escaping
   and unsigned large payload follow the scope so found. The signer's
   canonical request was checked against aws-sdk-go-v2's own signer
   (v1.42.0) on an S3 path and query with reserved characters and a
   Lambda path, and matches.
4. **azure.** The subject token as a client assertion (`grant_type
   client_credentials`, `client_id`, `scope`, RFC 7523's assertion type)
   at `exchange.token_url`, which must be `login.microsoftonline.com` on
   https; the access token is a bearer, `Overwrite`, on the kind's egress
   hosts among `exchange.hosts`.
5. **oidc.** A direct target (`mode: direct`, or no `token_url`) has its
   subject token itself written as the bearer, minted again a minute
   before its `expires_in` (five minutes) ends. A trading target's is
   traded by RFC 8693 at `exchange.token_url` (any https URL with no user
   or fragment: the tenant's own endpoint, which Latchkey holds), with
   `audience`, `subject_token_type`, `requested_token_type`; the access
   token is the bearer, `Overwrite`, ending at `expires_in` (else five
   minutes). Both on the kind's egress hosts among `exchange.hosts`.
6. **Refusals** name the step: `aws_sts` (AWS's XML `Code` and
   `Message`), `azure_entra`, `oidc_token` (OAuth's `error` and
   `error_description`), beside `gcp_sts`/`gcp_iam`. A 4xx other than 429
   is the product's configuration; a 5xx or no answer, or an exchange URL
   that is not the cloud's, is `credential_timeout`. An azure or oidc
   exchange naming no hosts is the product's. `cloud_not_supported` is
   now only a cloud no node knows.
7. **The pod's trust bundle** is also named by `AWS_CA_BUNDLE`, which
   the AWS CLI and SDKs read in place of their own bundle.

Not yet: sovereign Azure clouds' authorities; a presigned URL the
sandbox made on the placeholder (refused `credential_preset`, as for a
Secret).

## Amendment 2026-11-02: a well-known secret shape is refused, not only an injected value (mesh-specs gap 28 item 5)

The canary scan, on the relay's routes and in a push's pack, looked
only for a value the relay itself injected. A secret the session found
or was handed some other way never crossed the relay as an injected
value, so it left unseen.

1. **Three shapes, wherever the canary looks.** `egress.FindKnownSecret`
   recognises an AWS access key id (`(?:AKIA|ABIA|ACCA|ASIA)[0-9A-Z]{16}`),
   a GitHub token (`gh[opsur]_[0-9A-Za-z]{36,}`) and a PEM private-key
   header (`-----BEGIN … PRIVATE KEY-----`), labelled
   `secret:aws_access_key_id`, `secret:github_token` and
   `secret:pem_private_key`. It runs after the lease's own values on each
   of the canary's paths: `injectedValues.Scan` (an MCP or vendor API
   call, and a git push's pack through `Grant.SetScan`), `Body` (a
   tunnelled request's body as it streams, its window held at no less
   than `egress.MaxKnownSecretLen`, 80 bytes, so a shape split across
   two reads is still found) and `inspectModel` (a model request's body,
   held whole). A lease's own value is still looked for first and is
   still an exposure.
2. **Refused and reported, never an exposure.** A shape found is refused
   with `egress.CodeSecretSeen`, `secret_seen: the call carried a
   <label>` (a streamed body cut short as an injected value's is), logged
   `a <label> was seen in <where>; refused, not an injected value`
   (`injectedValues.secretSeen`), and handed to the lease's
   `onSecretSeen` callback, `Runtime.reportSecretSeen`: one `dlp`
   SecurityReport, verdict deny, its rule the label and its host the
   call's (a bare host, else none), never the value. Unlike an injected
   value it never calls `expose`: `exposure()` stays nil, `onExposed`
   never fires, the lease is not failed `credential_exposed` and Latchkey
   is not told — the shape was never a credential the job carried, so
   there is nothing to mark exposed or rotate. The session is refused
   that one call and runs on.
3. **One callback, told apart by label.** `Body`'s scan carries both
   kinds of finding through one `found` callback; `egress.IsKnownSecretLabel`
   (the `secret:` prefix, which no injected value's ref carries) sends a
   shape to `secretSeen` and anything else to `expose`.

`TestFindKnownSecret` (`internal/egress`) checks the three shapes and a
clean miss; `TestSecretSeen` (`internal/nodev1`) each of the four paths,
the secret-seen callback told once naming the shape and host, and the
lease's exposure nil;
`TestCanaryExposureIsUnchangedBySecretSeen` that a lease's own value is
still an exposure that fails the lease `credential_exposed`.
