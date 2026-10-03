package commands

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/meshmcp"
)

// mcpCmd is `mesh mcp`: Mesh inside Claude Code, added once with
// `claude mcp add mesh -- mesh mcp` (ADR-023, mesh mcp). It speaks MCP
// on stdin/stdout on the credential in ~/.mesh/token — the person's
// Latchkey login or an rs_* token, refreshed under the lock — and
// writes nothing else to stdout.
var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve Mesh's tools to Claude Code: a stdio MCP server on your own login",
	Long: "mesh mcp is a stdio MCP server for Claude Code: add it once with\n\n" +
		"  claude mcp add mesh -- mesh mcp\n\n" +
		"and Claude Code gets mesh_goals, mesh_status, mesh_join, mesh_switch, mesh_progress,\n" +
		"mesh_ask, mesh_finish and mesh_release on the credential in ~/.mesh/token.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		gw, err := platformClient()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return meshmcp.NewServer(gw).Serve(ctx, os.Stdin, os.Stdout)
	},
}
