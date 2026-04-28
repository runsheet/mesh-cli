package main

// TODO(spec22-phase5): WhatsApp gateway logic moved to internal/gateways/whatsapp.
// The cobra command stub stays here because init() references channelServerCmd
// from package main and internal packages cannot import package main.

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	wgw "projectmesh.io/mesh-cli/internal/gateways/whatsapp"
)

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
	channelServerWhatsappGatewayCmd.Flags().StringVar(&whatsappGatewayAccountFlag, "account", "", "WhatsApp account name (required)")
	channelServerCmd.AddCommand(channelServerWhatsappGatewayCmd)
}
