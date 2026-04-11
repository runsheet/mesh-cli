# Mesh CLI - Go Implementation

This is the Go-based replacement for the Mesh CLI, providing parity with the existing TypeScript CLI.

## Installation

Build from source:

```bash
cd cmd/mesh
GO111MODULE=on go build -o mesh
```

## Authentication Commands (Testable Now)

- `mesh auth login [baseUrl]` - Browser-based login (stubbed, manual token entry)
- `mesh auth token-login [baseUrl] <accessToken>` - Login using an access token
- `mesh auth whoami` - Show current logged-in user
- `mesh auth logout` - Log out and clear session

## Work-Item Commands (Pending Backend Configuration)

The following commands are implemented but require a properly configured and accessible `mesh-api` backend to function correctly:

- `mesh work-item create <title> <sourceAgentId> [scope] [projectId]`
- `mesh work-item update <workItemId> <agentId> [status] [currentOwner]`
- `mesh work-item handoff <workItemId> <fromRole> <toRole> <agentId> [summary]`
- `mesh work-item review <workItemId> <reviewerRole> <decision> <agentId> [notes]`
- `mesh work-item learning <workItemId> <agentId> [outcome]`

## Release

Multi-platform binaries will be published via GitHub Releases triggered by tags matching `v*.*.*`.

## Known Limitations

- Full work-item command validation is pending proper backend API availability.
- Auth commands have been smoke tested locally and are ready for use.

## Contribution

Contributions and fixes are welcome.

---

This is the initial Go port of the Mesh CLI.
