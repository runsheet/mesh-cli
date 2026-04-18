package main

import (
	"github.com/spf13/cobra"
)

// channelServerCmd is the parent of `mesh channel-server <type>` subcommands.
// Each channel type registers its own subsubcommand here. Slice 1 ships the
// `echo` type; slice 2 adds `discord`.
//
// The subprocess is spawned by the daemon's ChannelManager; users rarely
// invoke it directly. Its stdio follows the MCP JSON-RPC 2.0 convention so
// claude code can talk to it through the .claude/settings.json wiring.
var channelServerCmd = &cobra.Command{
	Use:   "channel-server",
	Short: "Run a channel server (internal: spawned by the mesh daemon)",
	Long: "channel-server is normally spawned by the mesh daemon to back the " +
		"MCP servers listed in an agent's .claude/settings.json. Direct " +
		"invocation is supported for debugging.",
}

func init() {
	channelServerCmd.AddCommand(channelServerEchoCmd)
}
