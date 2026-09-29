# Mesh CLI rollout notes

## Immediate next steps

1. Decide where machine auth lives
   - portal backend extension
   - separate Mesh API service

2. Define token issuance model
   - org-level machine tokens
   - agent-scoped tokens later if needed

3. Implement minimal work-item writes first
   - create
   - update
   - handoff
   - review
   - learning

4. Implement spec operations
   - create
   - update
   - status

5. Migrate agent instructions
   - PM
   - DevLead
   - Builder

## Recommendation

Build the backend write API first, then layer the CLI on top.

That keeps the architecture clean:
- API = policy + auth + persistence
- CLI = operator/agent UX

## First dogfood target

Use `mesh-cli` to operate:
- `Agent Config Gitification v1`

That gives us a real internal ops item to prove the flow.

## Attested validation on the mini (node-attests-its-validation)

A build now runs its repository's `.runsheet/validate.yml` before the
push and uploads the attestation (ADR-014). The mini picks this up
from the next release: a node-changing merge is tagged and signed as
below (see [Node deploys](#node-deploys)), and the mini takes it once
`desiredVersion` is set on the console or by `mesh node update <tag>`
(the Fallback section below is for a build that is not a release).
Until Podman is on the mini its sessions are unsandboxed, so every
attestation it uploads says `runnerImage: host:<hostname>` and
`sandboxed: false`.

## Node deploys

A node-changing merge to `main` — anything under `cmd/`, `internal/`
or `images/node/`, or `go.mod`/`go.sum` — is tagged automatically:

1. `.github/workflows/release-on-merge.yml` bumps the version (patch,
   or minor on a `release:minor` PR label or goal doc) and pushes the
   tag.
2. `.github/workflows/release-tag.yml` builds the signed binaries and
   node image from that tag and mirrors the release, identical, to the
   public `runsheet/mesh` — nodes update from the mirror, anonymously,
   verified against `runsheet/mesh-cli`'s signing identity.
3. The console offers the release; the mini takes it once
   `desiredVersion` is set there, or with `mesh node update <tag>`
   (no cross-build needed — the release already carries
   `mesh-linux-amd64`).
4. Once runsheet's `nodes.autoUpdate` lands, the platform sets
   `desiredVersion` to the latest release itself when the mini is
   idle, and it follows main within the hour with no person.

**Prerequisite: `RELEASE_TAG_TOKEN`.** `release-on-merge.yml` pushes
the tag with this repository secret — a fine-grained token with
Contents: read and write on `runsheet/mesh-cli`; `GITHUB_TOKEN` cannot
be used, since a tag it pushes starts no workflow. Until Chris adds
it, no tag is cut: run `36507379094` stopped at exactly this check.
`v0.2.189` will be the first auto-cut tag once the token is set; the
public line today is `v0.2.188` on `runsheet/mesh`, the first signed
release on the mirror (ADR 013's amendment).

### Fallback

For a build that is not a release — the workflow cannot run, or a
change needs the mini before the next tag — upgrade by hand:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh
scp mesh-linux mini:bin/mesh.new
ssh mini 'mesh node upgrade --binary ~/bin/mesh.new'
```

This is the fallback, not the way: `mesh node upgrade --binary` swaps
the binary and restarts without a drain, so host sessions are adopted
by the new process — the same as a platform-driven self-update.
`mesh node update <tag>` above, used to take a signed release by hand,
is the draining variant instead: it waits for the node's sessions to
end before it swaps, so nothing is adopted there. Either way, the
ordinary path is the merge above.
