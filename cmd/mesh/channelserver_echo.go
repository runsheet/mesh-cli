package main

// TODO(spec22-phase6): echo server implementation moved to internal/shim.
// This file retains the cobra command stub only.

import (
	"github.com/spf13/cobra"
	ishim "projectmesh.io/mesh-cli/internal/shim"
)

var (
	echoAgentFlag string
	echoIndexFlag int
)

var channelServerEchoCmd = &cobra.Command{
	Use:   "echo",
	Short: "Echo MCP server (debug / slice-1 plumbing exerciser)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return ishim.RunEchoChannelServer(echoAgentFlag, echoIndexFlag)
	},
}

func init() {
	channelServerEchoCmd.Flags().StringVar(&echoAgentFlag, "agent", "", "Agent name this channel backs (required)")
	channelServerEchoCmd.Flags().IntVar(&echoIndexFlag, "index", 0, "Channel index within the agent's channel list")
}
