package commands

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/mcprelay"
)

var (
	mcpRelayURL       string
	mcpRelayTokenFile string
)

// mcpRelayCmd is a session's runsheet server: a stdio MCP relay to
// the gateway's /mcp whose bearer is a file the node keeps fresh, read
// on every request, so the session outlives any one token.
var mcpRelayCmd = &cobra.Command{
	Use:          "mcp-relay --url <gateway>/mcp --token-file <path>",
	Short:        "Relay stdio MCP to the gateway's /mcp on a bearer read from a file per request",
	Hidden:       true,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if mcpRelayURL == "" || mcpRelayTokenFile == "" {
			return errors.New("mcp-relay: --url and --token-file are required")
		}
		r := &mcprelay.Relay{URL: mcpRelayURL, TokenFile: mcpRelayTokenFile, Stderr: cmd.ErrOrStderr()}
		return r.Run(cmd.Context(), os.Stdin, cmd.OutOrStdout())
	},
}

func init() {
	mcpRelayCmd.Flags().StringVar(&mcpRelayURL, "url", "", "the MCP endpoint, e.g. https://api.runsheet.dev/mcp")
	mcpRelayCmd.Flags().StringVar(&mcpRelayTokenFile, "token-file", "", "the file holding the bearer, read on every request")
	rootCmd.AddCommand(mcpRelayCmd)
}
