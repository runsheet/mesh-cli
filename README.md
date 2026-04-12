# Mesh CLI - Go Implementation

This is the Go-based replacement for the Mesh CLI, providing parity with the existing TypeScript CLI and a real browser-based auth flow.

## Installation

Build from source:

```bash
cd cmd/mesh
GO111MODULE=on go build -o mesh
```

## Authentication Commands

- `mesh auth login [baseUrl]` - Browser-based OIDC Authorization Code + PKCE login. Opens your browser, waits for the local callback, exchanges the auth code, and saves the authenticated session locally.
- `mesh auth login --no-browser [baseUrl]` - Prints the OIDC authorize URL instead of opening it automatically.
- `mesh auth token-login [baseUrl] <accessToken>` - Explicit fallback. Validates the supplied bearer token against `mesh-api`, then saves the session locally.
- `mesh auth whoami` - Shows the authenticated identity for the current saved session.
- `mesh auth logout` - Removes the saved local session.

Sessions are stored in `~/.mesh-session.json`.

## Backend Auth Expectations

`mesh auth login` expects `mesh-api` to expose `GET /auth/login-start` with OIDC metadata for the CLI, either directly or via an issuer that supports standard OIDC discovery. The CLI expects enough information to derive:

- issuer or explicit authorization endpoint
- token endpoint
- client ID
- scopes
- optional userinfo endpoint

`mesh auth whoami` and `mesh auth token-login` use `GET /auth/session` on `mesh-api` to validate the access token and return the current user identity.

## Work-Item Commands

The following commands are implemented but still require a properly configured and accessible `mesh-api` backend to function correctly:

- `mesh work-item create <title> <sourceAgentId> [scope] [projectId]`
- `mesh work-item update <workItemId> <agentId> [status] [currentOwner]`
- `mesh work-item handoff <workItemId> <fromRole> <toRole> <agentId> [summary]`
- `mesh work-item review <workItemId> <reviewerRole> <decision> <agentId> [notes]`
- `mesh work-item learning <workItemId> <agentId> [outcome]`

## Release

Multi-platform binaries are published via GitHub Releases triggered by tags matching `v*.*.*`.

## Known Limitations

- Interactive browser login depends on `mesh-api` exposing real OIDC metadata and token validation.
- Device-flow fallback is not implemented yet.
- Full work-item command validation is still pending backend API availability.

## Contribution

Contributions and fixes are welcome.
