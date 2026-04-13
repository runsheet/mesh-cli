# Mesh CLI - Go Implementation

This is the Go-based replacement for the Mesh CLI, providing parity with the existing TypeScript CLI and a real browser-based auth flow.

## Installation

Build from source:

```bash
cd cmd/mesh
GO111MODULE=on go build -o mesh
```

## Authentication Commands

- `mesh auth login [baseUrl]` - Browser-based portal handoff login. Opens the Mesh portal login page, waits for the local callback, validates the returned session token with `mesh-api`, and saves the authenticated session locally.
- `mesh auth login --no-browser [baseUrl]` - Prints the browser login URL instead of opening it automatically.
- `mesh auth token-login [baseUrl] <accessToken>` - Explicit fallback. Validates the supplied bearer token against `mesh-api`, then saves the session locally.
- `mesh auth whoami` - Shows the authenticated identity for the current saved session.
- `mesh auth logout` - Removes the saved local session.

Sessions are stored in `~/.mesh-session.json`.

## Browser Login Architecture

`mesh auth login` now follows the auth model Mesh already uses:

- the portal owns end-user Supabase sign-in
- the CLI starts a local callback listener
- the CLI opens the Mesh portal login page with `cli_redirect=<local callback>`
- after successful sign-in, the portal redirects the browser back to the CLI with the signed-in access token
- the CLI validates that token with `GET /auth/session` on `mesh-api` before persisting it

This replaces the earlier attempt to force a generic OIDC Authorization Code + PKCE flow onto a Supabase setup that is not acting as the desired provider boundary for Mesh.

## Backend Expectations

`mesh-api` should expose:

- `GET /auth/login-start` returning browser-login metadata for the CLI
- `GET /auth/session` validating a bearer token and returning the current user identity

`mesh-portal` should support `GET /login?cli_redirect=<local callback>` and, after successful sign-in, redirect the browser to that local callback with the signed-in session token.

## Work-Item Commands

After `mesh auth login`, the following commands use the saved signed-in user session and call `mesh-api` with the user bearer token:

- `mesh work-item create <title> [scope] [projectId]`
- `mesh work-item update <workItemId> [--title ...] [--status ...] [--priority ...] [--owner-role ...] [--current-owner ...] [--scope ...] [--project-id ...] [--requester ...] [--note ...]`
- `mesh work-item handoff <workItemId> <fromRole> <toRole> [--summary ...] [--required-outputs ...] [--repo-path ...] [--system-target ...] [--constraints ...] [--test-expectations ...] [--linked-spec-version N] [--note ...]`
- `mesh work-item review <workItemId> <reviewerRole> <decision> [--notes ...] [--criteria-checklist ...]`
- `mesh work-item learning <workItemId> [--outcome ...] [--what-worked ...] [--what-failed ...] [--follow-up-needed] [--follow-up-item-id ...] [--note ...]`

`mesh work-item update --status` is validated against the live Ralph Loop state machine values:
`inbox`, `clarifying`, `spec_ready`, `awaiting_approval`, `ready_for_devlead`, `in_execution`, `blocked`, `in_review`, `changes_requested`, `done`, `archived`.

## Release

Multi-platform binaries are published via GitHub Releases triggered by tags matching `v*.*.*`.

## Known Limitations

- Live browser-login and work-item testing still depends on the deployed `mesh-portal` and `mesh-api` carrying the matching session-auth changes.
- Device-flow fallback is not implemented yet.

## Contribution

Contributions and fixes are welcome.
