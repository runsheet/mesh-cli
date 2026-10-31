---
summary: A v1 lease's egress tunnel terminates TLS for the hosts one of its rules names, with a CA of the lease's own whose key never leaves the node's memory and whose certificate, name-constrained to those hosts, is the one thing the pod is given (a bundle in its HOME named by SSL_CERT_FILE and friends, NODE_EXTRA_CA_CERTS); every other host is spliced by SNI as before. The first rule signs every request under a brief's egress.registries prefix with the lease's session token in the DPoP scheme (mesh-specs gap 31); gaps 28 and 32 add rules of their own.
status: accepted
---

# ADR-045: The relay terminates TLS for the hosts a rule names

2026-10-31. Mesh v1, mesh-specs gaps 31 (the relay signs requests to
runsheet's registries), 28 (static secrets and scoped-mint children) and
32 (federation tokens). Applies to `internal/nodev1` and
`internal/egress`; the legacy runtime is unchanged.

## Context

A lease's egress tunnel (`internal/egress`, projectmesh-io/api ADR 001
amendment "tool slots and egress") is a CONNECT proxy that allows a host
by name and SNI and then pipes bytes: it never reads past the
ClientHello. That is enough to allow or refuse a host, not to act on a
request. runsheet now serves npm, OCI and binaries at
`https://api.runsheet.dev/{npm,v2,artifacts}/` and takes a job's
mesh-session token only in the DPoP scheme with a proof, which npm,
docker, crane and curl cannot send; npm's tarball URLs are absolute on
`api.runsheet.dev`, so a local registry address (gap 18's
`MESH_REGISTRY`) cannot stand in for the host. Gap 28 needs the same
ability to rewrite a request's headers for Latchkey's secrets, and gap
32 for a cloud's federation exchange.

## Decision

1. **TLS is terminated only where a rule needs it.** `egress.Tunnel`
   takes a `CA` and `Rules`. After the policy allows a CONNECT and the
   ClientHello names the host asked, a host one of the rules
   `Terminates` is answered by the relay: TLS 1.2+ with a certificate
   minted for the host, HTTP/1.1 with keep-alive to the session, each
   request handed to every rule for the host and then forwarded on a
   connection of the node's own, verified against the system roots
   (`Upstream` in tests), with no proxy and the session's encodings
   left as they are. Bodies stream both ways: the reverse proxy flushes
   every write (`FlushInterval -1`) and the request body is read as the
   upstream takes it, so a gigabyte layer or an npm publish tarball is
   never held. A request whose Host, or absolute URL, names another
   host is refused 421. Every other host is spliced by SNI exactly as
   before.

2. **One CA per lease, in the node's memory.** `egress.NewCA` makes a
   P-256 key and a self-signed CA certificate when the lease's relay
   opens: `CALifetime` (7 days, past any lease), path length 0, and
   name constraints (critical) permitting only the rules' hosts and
   their subdomains. The key is never written to disk, never handed to
   the pod or to a relay sidecar, and goes with the lease's tunnel.
   Host certificates are minted on the first request for the host, an
   hour long (`LeafLifetime`), backdated five minutes for a pod's clock,
   kept and minted again ten minutes before they end. A per-node CA was
   rejected: one key every lease's pod trusts for as long as the node
   runs, where a per-lease one costs one key generation per lease that
   terminates anything and is trusted by no other lease.

3. **The pod gets the certificate and nothing else.** When a lease
   terminates anything, the node writes into the session's HOME
   `.mesh/trust/relay-ca.pem` (the CA alone) and
   `.mesh/trust/ca-bundle.pem` (the node's system bundle —
   `SSL_CERT_FILE` of the node, else the first of crypto/x509's Linux
   paths and `/etc/ssl/cert.pem` — with the CA appended), and adds to
   the pod's proxy variables `NODE_EXTRA_CA_CERTS` (the CA: node, npm,
   claude) and `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`,
   `PIP_CERT`, `GIT_SSL_CAINFO`, `CARGO_HTTP_CAINFO` (the bundle: Go
   tools such as crane, OpenSSL's curl and Python, requests, pip, git,
   cargo, uv). A node with no system bundle sets only
   `NODE_EXTRA_CA_CERTS` and says so. A lease that terminates nothing
   gets none of them.

4. **Only the session's CONNECTs are terminated.** A session on the
   host is not pointed at its relay, so a lease with no pod makes no CA.
   A relay sidecar's own upstream connections (its MCP, git and gap 18
   registry clients) carry `Mesh-Relay-Dial: own` on their CONNECT; the
   node's side of the lease's socket marks them `egress.SpliceOnly` and
   they are spliced whatever the rules say. The session's CONNECTs,
   which the sidecar forwards with `DialSession`, never carry it; the
   pod cannot reach the node's socket to forge it.

5. **Gap 31: the registries' rule.** The brief's
   `egress.registries` (`meshapi.Egress.Registries`) lists URL prefixes.
   `egress.SignRule` takes every request whose URL is under one — https,
   the prefix's host and port, the path as it goes upstream starting
   with the prefix's path at a segment boundary (`/npm/` never takes
   `/npmx`), no `.` or `..` segment raw or escaped — and sets
   `Authorization: DPoP <the session token of the moment>` and a fresh
   `DPoP` proof (`dpop.Key.Proof`: htm the method, htu the URL with its
   path escaped as sent and no query, ath the token's hash), replacing
   whatever the session sent. The token is asked per request
   (`sessionTokens.Token`), so a refreshed one is used from the next
   request on. A request to the same host outside every prefix (such
   as `/mcp`) goes on as sent. With no token to be had the request is
   answered `502` with `X-Mesh-Error: credential_unavailable` and never
   forwarded. The rule is built only from prefixes whose host and port
   the kind's egress hosts (narrowed by the tenant's) allow: a registry
   never widens egress, and one outside the hosts signs nothing.

6. **Audited without a value.** Each request a rule acted on is an
   AccessRecord of the lease: `{kind: api, vendor: registry, service:
   signed, method, path: <the prefix>, outcome: ok | failed, reason:
   <status> | <the relay's code>, bytes: <both bodies>}` — never the
   token, never the path past the prefix. The tunnel itself is still
   reported as an allowed egress with its bytes (security events).
   AccessRecords are posted in chunks of `meshapi.MaxAccessPost` (500),
   Mesh's limit, since an `npm ci` signs thousands.

7. **The seam for gaps 28 and 32.** A later rule implements
   `egress.Rule`: `Terminates(host) bool` names its hosts (exact, or
   its own wildcard), and `Apply(*http.Request) (*Applied, error)` sees
   each request with its URL absolute on the tunnel's host before it
   goes upstream, changes its headers or query, and answers what it
   applied (`Applied{Rule, Ref, Form}`, names only) or a `*Refusal`
   (status and `X-Mesh-Error` code: `403 credential_preset`, `502
   credential_unavailable`, `413 signing_needs_body`). The lease's CA is
   made over every rule's hosts (`interception` in
   `internal/nodev1/intercept.go`), and `interceptAudit` records each
   `Applied` by its rule. A rule must not read the body; one that must
   (`aws_sigv4` without `UNSIGNED-PAYLOAD`) refuses instead.

## Consequences

- A session's npm, docker/crane and curl reach runsheet's registries
  with no credential in the pod and no change to the project but its
  `.npmrc` scope line.
- The relay now reads the plaintext of the hosts its rules name. It
  reads nothing of any other host, and only for the lease's own pod.
- A DPoP-Nonce challenge on a signed request is not retried: the body
  may already be streaming. runsheet's registry routes ask none today.
- The trust bundle is the node's system roots, not the image's: the
  node verifies the same hosts against the same roots, and the node
  image is Debian's ca-certificates as most nodes are.
- HTTP/2 to the session is not offered (ALPN `http/1.1`); the upstream
  connection may use it.
