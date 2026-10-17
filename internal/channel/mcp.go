// Package channel is the node's Claude Code channel (research preview):
// an MCP server the daemon serves to one persistent `claude` session. It
// pushes the platform's events into the session as
// `notifications/claude/channel` and takes the session's reports back
// through tools that are thin calls on the gateway.
package channel

import "projectmesh.io/mesh-cli/internal/meshmcp"

// The stdio MCP server lives in internal/meshmcp, shared with `mesh mcp`
// (ADR-023, mesh mcp); channel mode is the one that sets Channel.

// ServerName is the `--dangerously-load-development-channels
// server:mesh` flag's own name: Claude Code's channel capability flag,
// unrelated to and unchanged by meshmcp.ServerName, the MCP server's
// own identity in its initialize response (ADR-023's amendment
// 2026-10-17).
const ServerName = "mesh"

// Tool is one MCP tool: its schema and its handler.
type Tool = meshmcp.Tool

// Server is the stdio MCP server; channel mode sets Channel.
type Server = meshmcp.Server

// ObjectSchema builds a JSON schema for an object of string/int/bool
// properties, with the required names.
func ObjectSchema(props map[string]string, required ...string) map[string]any {
	return meshmcp.ObjectSchema(props, required...)
}
