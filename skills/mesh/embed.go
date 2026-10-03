// Package meshskill carries skills/mesh/SKILL.md in the binary (as
// images/node's embed.go carries the node image's Containerfile), so
// `mesh mcp install` writes the file mesh-cli ships, not a copy that
// can drift from it.
package meshskill

import _ "embed"

// SKILL is skills/mesh/SKILL.md.
//
//go:embed SKILL.md
var SKILL []byte
