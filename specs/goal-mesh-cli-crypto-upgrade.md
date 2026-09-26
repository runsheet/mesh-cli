---
title: Goal — mesh-cli's golang.org/x/crypto is current, and its 13 alerts close
kind: plan
status: draft
parent: prd
---

# Goal — mesh-cli's golang.org/x/crypto is current, and its 13 alerts close

Mesh goal `mesh-cli-crypto-upgrade`, running.

## Statement

GitHub reports 13 open Dependabot alerts on projectmesh-io/mesh-cli (7 critical, 2 high, 4 moderate), every one in golang.org/x/crypto (the ssh package: agent and key constraints not enforced, @revoked not honoured, deadlocks and panics). Upgrade golang.org/x/crypto (and whatever go mod tidy then requires of golang.org/x/*) to the latest release, run the full suite, and check whether the node actually uses x/crypto/ssh; if it does not, say so in the goal doc.

## Acceptance

go.mod pins the latest golang.org/x/crypto; go vet, go build and go test ./... pass; after merge the repo's open Dependabot alerts for golang.org/x/crypto are zero.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Upgrade golang.org/x/crypto to latest and record whether the node uses x/crypto/ssh | running | — | — | $0.32 |

## Log

- 2026-09-24 · item 1 · session started
- 2026-09-24 · item 1 · I upgraded golang.org/x/crypto from v0.50.0 to v0.57.0, the latest release today, and all the acceptance checks pass. The node does not use x/crypto/ssh, which is where all 13 alerts are. **What changed** (one commit, `3fccfa3`, no Co-Authored-By trailer as AGENTS.md requires, not pushed): - **`go.mod` / `go.sum`:** x/crypto is now v0.57.0 and is still marked `// indirect`. That version requires newer x/sys (v0.43.0 → v0.48.0) and x/term (v0.42.0 → v0.46.0), so `go get` raised those too. No other module changed, and no code fixes were needed. - **`docs/dependency-crypto-ssh.md`** (new): has the `summary:` line and the Finding, Upgrade and Dependabot alerts sections. **Does the node use x/crypto/ssh?** No, neither directly nor through sprig: - No Go file in the repo imports x/crypto. - The only way in is `internal/template` → sprig → `x/crypto/bcrypt`, which also pulls in blowfish, pbkdf2 and scrypt. `go mod why -m golang.org/x/crypto` shows that chain. - `go mod why golang.org/x/crypto/ssh` reports that the main module does not need the package. - `go list -deps ./... | grep x/crypto/ssh` prints nothing. - The other `vendor/golang.org/x/crypto/...` lines in that list are Go's own bundled copy, not a dependency in go.mod. **Acceptance, all checked after the commit:** 1. `go.mod` shows v0.57.0, the same version `go list -m golang.org/x/crypto@latest` reports. 2. `go mod tidy` followed by `git diff --exit-code` shows no changes. 3. `go vet ./...`, `go build ./cmd/mesh/` and `go test ./...` all pass. 4. The doc exists, and its Finding matches the empty result of the ssh `go list` check. 5. My commit touches only `go.mod`, `go.sum` and the new doc. `git diff main..HEAD` also shows `specs/goal-mesh-cli-crypto-upgrade.md`, but that comes from the earlier goal commit `46ad497`, not from this work. 6. No commit message on the branch has a Co-Authored-By trailer. Whether the Dependabot alerts close can only be checked on GitHub after merge.
