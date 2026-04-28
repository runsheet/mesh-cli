package main

// TODO(spec22-phase5): Discord gateway logic moved to internal/gateways/discord.
// The cobra command stub stays here because init() references channelServerCmd
// from package main and internal packages cannot import package main.

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	dgw "projectmesh.io/mesh-cli/internal/gateways/discord"
)

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

func init() {
	channelServerDiscordGatewayCmd.Flags().StringVar(&discordGatewayAccountFlag, "account", "", "Discord account name (required)")
	channelServerCmd.AddCommand(channelServerDiscordGatewayCmd)
}
