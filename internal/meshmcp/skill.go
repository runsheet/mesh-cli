package meshmcp

import (
	meshskill "projectmesh.io/mesh-cli/skills/mesh"
)

// Skill is skills/mesh/SKILL.md, the Claude Code skill `mesh mcp
// install` writes (ADR-023, Skill).
func Skill() []byte {
	return meshskill.SKILL
}
