package commands

import (
	"context"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshmcp"
)

// sessionToolsCmd serves a host build session's own mesh_finish and
// mesh_fail on stdio (harness.NodeWorkMCP): each answers at once and
// records nothing on the platform, so the node reads the call off the
// session's stream and pushes before it hands the finish in (goal
// a-build-pushes-before-it-finishes).
var sessionToolsCmd = &cobra.Command{
	Use:          harness.SessionToolsCommand,
	Short:        "Serve a node build session's own finish tools over stdio MCP",
	Hidden:       true,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		srv := &meshmcp.Server{}
		for _, t := range harness.NodeWorkTools() {
			reply := t.Reply
			srv.Tools = append(srv.Tools, meshmcp.Tool{
				Name:        t.Name,
				Description: t.Description,
				Schema:      t.Schema,
				Call:        func(context.Context, map[string]any) (string, error) { return reply, nil },
			})
		}
		return srv.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
	},
}

func init() {
	rootCmd.AddCommand(sessionToolsCmd)
}
