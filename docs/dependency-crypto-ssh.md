---
title: golang.org/x/crypto and x/crypto/ssh in the node
summary: The node does not import or link golang.org/x/crypto/ssh; x/crypto is indirect (sprig → bcrypt) and was upgraded v0.50.0 → v0.57.0.
---

# golang.org/x/crypto and x/crypto/ssh in the node

## Finding

The node does not use `golang.org/x/crypto/ssh`. It is not imported
directly, and nothing the node imports (sprig included) pulls it in, so
it is not linked into the `mesh` binary.

`golang.org/x/crypto` is only an indirect dependency. It comes through
`github.com/Masterminds/sprig/v3`, which `internal/template` uses, and
sprig needs just `x/crypto/bcrypt` and what bcrypt depends on. The
commands below were run on the upgraded tree (go1.26.2):

- `grep -rn 'golang.org/x/crypto' --include=*.go .` found no matches, so
  no source file in the repo imports x/crypto.
- `go list -deps ./... | grep x/crypto` listed
  `golang.org/x/crypto/{bcrypt,blowfish,pbkdf2,scrypt}`, plus the
  standard library's own vendored copy (`vendor/golang.org/x/crypto/...`:
  `cryptobyte`, `chacha20`, `chacha20poly1305`, `internal/poly1305`,
  `internal/alias`), which ships with the Go toolchain and not with
  go.mod. `go list -deps ./... | grep x/crypto/ssh` printed nothing.
- `go mod why -m golang.org/x/crypto` printed
  `projectmesh.io/mesh-cli/internal/template` →
  `github.com/Masterminds/sprig/v3` → `golang.org/x/crypto/bcrypt`.
- `go mod why golang.org/x/crypto/ssh` printed
  `(main module does not need package golang.org/x/crypto/ssh)`.

The results were the same before the upgrade, on v0.50.0.

## Upgrade

On 2026-09-24, `golang.org/x/crypto` went from **v0.50.0** to **v0.57.0**,
which `go list -m golang.org/x/crypto@latest` reported as the latest
release that day. It stays `// indirect` in go.mod. x/crypto v0.57.0
requires newer `golang.org/x/sys` and `golang.org/x/term`, so `go get`
raised those as well (sys v0.43.0 → v0.48.0, term v0.42.0 → v0.46.0). No
other module changed. No code changes were needed:
`go vet ./...`, `go build ./cmd/mesh/` and `go test ./...` all pass, and
`go mod tidy` leaves go.mod and go.sum unchanged.

## Dependabot alerts

GitHub reports 13 open Dependabot alerts on runsheet/mesh-cli for
`golang.org/x/crypto` (7 critical, 2 high, 4 moderate). All of them are
in the `ssh` package: agent and key constraints not enforced, `@revoked`
not honoured, deadlocks and panics. As the Finding shows, the node never
reached that code. The alerts are raised because go.mod names a
vulnerable module version, not because the vulnerable package is used.
The upgrade moves the pin past the affected versions. Whether the alerts
close after merge is checked on GitHub, not in this change.
