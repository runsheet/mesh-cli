package main

// This file is a thin compatibility shim. MCP migration logic now lives in
// internal/claudeconf; the wrappers below let cmd/mesh/ continue to use the
// old names without change until Phase 9.
//
// TODO(spec22-phase9): inline all call sites to internal/claudeconf.

import (
	"os"

	"projectmesh.io/mesh-cli/internal/claudeconf"
)

func migrateMCPSettingsToMesh() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	claudeconf.MigrateMCPSettingsToMesh(exe)
}

func migrateMCPFile(path, agentName, exe string, newArgs []string) {
	claudeconf.MigrateMCPFile(path, agentName, exe, newArgs)
}
