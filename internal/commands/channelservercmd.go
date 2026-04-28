package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	dgw "projectmesh.io/mesh-cli/internal/gateways/discord"
	wgw "projectmesh.io/mesh-cli/internal/gateways/whatsapp"
	ishim "projectmesh.io/mesh-cli/internal/shim"
)

var channelServerCmd = &cobra.Command{
	Use:   "channel-server",
	Short: "Run a channel server (internal: spawned by the mesh daemon)",
	Long: "channel-server is normally spawned by the mesh daemon to back the " +
		"MCP servers listed in an agent's .claude/settings.json. Direct " +
		"invocation is supported for debugging.",
}

var (
	echoAgentFlag  string
	echoIndexFlag  int
	meshShimAgentFlag string
)

var channelServerEchoCmd = &cobra.Command{
	Use:   "echo",
	Short: "Echo MCP server (debug / slice-1 plumbing exerciser)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return ishim.RunEchoChannelServer(echoAgentFlag, echoIndexFlag)
	},
}

var channelServerMeshShimCmd = &cobra.Command{
	Use:   "mesh-shim",
	Short: "Unified mesh MCP shim (spawned by claude via plugin .mcp.json)",
	Long: "mesh-shim speaks MCP JSON-RPC on stdio and bridges to the daemon's " +
		"per-agent router socket (~/.mesh/agents/<name>/mesh.sock). One shim per " +
		"agent; the router multiplexes across all platform gateways.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if meshShimAgentFlag == "" {
			return fmt.Errorf("--agent is required")
		}
		return ishim.Run(meshShimAgentFlag)
	},
}

var discordGatewayAccountFlag string

var channelServerDiscordGatewayCmd = &cobra.Command{
	Use:   "discord-gateway",
	Short: "Discord gateway (daemon-supervised; one per account)",
	Long: "discord-gateway holds the upstream discordgo connection for a paired " +
		"Discord account and serves shim connections over gateway.sock. " +
		"Normally spawned by the mesh daemon's ChannelManager.",
	RunE: func(cmd *cobra.Command, args []string) error {
		account := strings.TrimSpace(discordGatewayAccountFlag)
		if account == "" {
			return fmt.Errorf("--account is required")
		}
		return dgw.Run(account)
	},
}

var whatsappGatewayAccountFlag string

var channelServerWhatsappGatewayCmd = &cobra.Command{
	Use:   "whatsapp-gateway",
	Short: "WhatsApp gateway (daemon-supervised; one per account)",
	Long: "whatsapp-gateway holds the whatsmeow client for a paired WhatsApp account " +
		"and serves router connections over gateway.sock. " +
		"Normally spawned by the mesh daemon's ChannelManager.",
	RunE: func(cmd *cobra.Command, args []string) error {
		account := strings.TrimSpace(whatsappGatewayAccountFlag)
		if account == "" {
			return fmt.Errorf("--account is required")
		}
		return wgw.Run(account)
	},
}

func init() {
	channelServerEchoCmd.Flags().StringVar(&echoAgentFlag, "agent", "", "Agent name this channel backs (required)")
	channelServerEchoCmd.Flags().IntVar(&echoIndexFlag, "index", 0, "Channel index within the agent's channel list")
	channelServerMeshShimCmd.Flags().StringVar(&meshShimAgentFlag, "agent", "", "Agent name (required)")
	channelServerDiscordGatewayCmd.Flags().StringVar(&discordGatewayAccountFlag, "account", "", "Discord account name (required)")
	channelServerWhatsappGatewayCmd.Flags().StringVar(&whatsappGatewayAccountFlag, "account", "", "WhatsApp account name (required)")
	channelServerCmd.AddCommand(channelServerEchoCmd)
	channelServerCmd.AddCommand(channelServerMeshShimCmd)
	channelServerCmd.AddCommand(channelServerDiscordGatewayCmd)
	channelServerCmd.AddCommand(channelServerWhatsappGatewayCmd)
}
