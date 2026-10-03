package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/meshmcp"
	"projectmesh.io/mesh-cli/internal/node"
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
		logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "mesh mcp: "+format+"\n", a...) }
		// the platform's /mcp is asked for its work tools at the start, and
		// again hourly as the tools are used (ADR-023, Hosted path)
		hosted := meshmcp.NewHosted(gw)
		go hosted.Serves(ctx)
		p := &meshmcp.Personal{GW: gw, Join: &meshmcp.Join{
			GW: gw, WS: &node.Git{Log: os.Stderr}, NodeID: mcpNodeID,
			Fallback: &node.TokenFallback{}, Hosted: hosted, Logf: logf,
		}}
		return p.Server().Serve(ctx, os.Stdin, os.Stdout)
	},
}

// mcpNodeID is the node mesh_join claims for: node.yaml's node id until
// the personal node's registration (the goal's item 2) answers its own.
func mcpNodeID(context.Context) (string, error) {
	cfg, err := node.LoadConfig()
	if err != nil {
		return "", err
	}
	if cfg.NodeID == "" {
		return "", errors.New("no node registered on this machine yet")
	}
	return cfg.NodeID, nil
}
