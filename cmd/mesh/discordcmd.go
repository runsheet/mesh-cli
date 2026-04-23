package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// discordCmd is the root of the `mesh discord` command tree.
// Subcommands mirror spec 12 §2: pair, unpair, list-accounts, list-channels, status.
var discordCmd = &cobra.Command{
	Use:   "discord",
	Short: "Manage Discord platform accounts",
	Long:  "Pair Discord bot accounts with the node, list accounts and channels, and check gateway status.",
}

func init() {
	discordCmd.AddCommand(discordPairCmd)
	discordCmd.AddCommand(discordUnpairCmd)
	discordCmd.AddCommand(discordListAccountsCmd)
	discordCmd.AddCommand(discordListChannelsCmd)
	discordCmd.AddCommand(discordStatusCmd)
}

// -- pair --

var (
	discordPairAccountFlag string
	discordPairTokenFlag   string
)

var discordPairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair a Discord bot account with this node",
	Long:  "pair stores the bot token at ~/.mesh/discord-accounts/<name>/token.txt (mode 0600) and signals the daemon to start the gateway.",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(discordPairAccountFlag)
		token := strings.TrimSpace(discordPairTokenFlag)
		if name == "" {
			return fmt.Errorf("--account is required")
		}
		if token == "" {
			return fmt.Errorf("--token is required")
		}

		if err := WriteDiscordAccount(name, token); err != nil {
			return fmt.Errorf("pair discord account: %w", err)
		}
		fmt.Printf("discord account %q paired\n", name)
		fmt.Println("  token.txt written (mode 0600)")
		fmt.Println("  minimum bot permissions: Read Messages, Send Messages, View Channel")
		fmt.Println("  if using allowedRoles: enable GuildMembers privileged intent in the Discord developer portal")

		// Best-effort: signal the daemon to start the gateway. Not fatal if the
		// daemon isn't running — gateways start on next daemon boot.
		if err := discordDaemonPair(name); err != nil {
			fmt.Printf("  note: daemon signal failed (%v); gateway starts on next node boot\n", err)
		} else {
			fmt.Println("  daemon notified; gateway starting")
		}
		return nil
	},
}

func init() {
	f := discordPairCmd.Flags()
	f.StringVar(&discordPairAccountFlag, "account", "", "Account name (required)")
	f.StringVar(&discordPairTokenFlag, "token", "", "Discord bot token (required)")
}

// discordDaemonPair sends a POST /discord/pair to the local daemon socket.
func discordDaemonPair(accountName string) error {
	c, err := NewLocalClient()
	if err != nil {
		return err
	}
	payload := map[string]string{"account": accountName}
	return c.do(context.Background(), http.MethodPost, "/discord/pair", payload, nil)
}

// -- unpair --

var discordUnpairAccountFlag string

var discordUnpairCmd = &cobra.Command{
	Use:   "unpair",
	Short: "Remove a paired Discord bot account",
	Long:  "unpair refuses if any agent references the account. On success it stops the gateway and removes the account directory.",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(discordUnpairAccountFlag)
		if name == "" {
			return fmt.Errorf("--account is required")
		}

		// Check that no agent references this account.
		ref, agentName, err := agentReferencesDiscordAccount(name)
		if err != nil {
			return fmt.Errorf("check agent refs: %w", err)
		}
		if ref {
			return fmt.Errorf("account %q is still referenced by agent %q; remove or update the agent first", name, agentName)
		}

		// Signal daemon to stop the gateway before we remove the dir.
		_ = discordDaemonUnpair(name) // best-effort

		removed, err := RemoveDiscordAccount(name)
		if err != nil {
			return fmt.Errorf("remove account: %w", err)
		}
		if !removed {
			return fmt.Errorf("discord account %q not found", name)
		}
		fmt.Printf("discord account %q removed\n", name)
		return nil
	},
}

func init() {
	discordUnpairCmd.Flags().StringVar(&discordUnpairAccountFlag, "account", "", "Account name (required)")
}

func discordDaemonUnpair(accountName string) error {
	c, err := NewLocalClient()
	if err != nil {
		return err
	}
	payload := map[string]string{"account": accountName}
	return c.do(context.Background(), http.MethodPost, "/discord/unpair", payload, nil)
}

// -- list-accounts --

var discordListAccountsCmd = &cobra.Command{
	Use:   "list-accounts",
	Short: "List paired Discord bot accounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		names, err := ListDiscordAccounts()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("no Discord accounts paired")
			return nil
		}
		for _, n := range names {
			meta, err := LoadDiscordAccountMeta(n)
			if err != nil {
				fmt.Printf("  %s  (error reading meta: %v)\n", n, err)
				continue
			}
			pairedAt := ""
			if !meta.PairedAt.IsZero() {
				pairedAt = "  paired " + meta.PairedAt.Format("2006-01-02")
			}
			botID := ""
			if meta.BotUserID != "" {
				botID = "  bot_id=" + meta.BotUserID
			}
			fmt.Printf("  %s%s%s\n", n, pairedAt, botID)
		}
		return nil
	},
}

// -- list-channels --

var discordListChannelsAccountFlag string

var discordListChannelsCmd = &cobra.Command{
	Use:   "list-channels",
	Short: "List Discord channels visible to the bot (requires live gateway)",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(discordListChannelsAccountFlag)
		if name == "" {
			return fmt.Errorf("--account is required")
		}
		c, err := NewLocalClient()
		if err != nil {
			return err
		}
		var resp struct {
			Channels []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Type int    `json:"type"`
			} `json:"channels"`
		}
		if err := c.do(context.Background(), http.MethodGet, "/discord/list-channels?account="+name, nil, &resp); err != nil {
			return fmt.Errorf("list-channels: %w", err)
		}
		if len(resp.Channels) == 0 {
			fmt.Println("no channels visible to this bot")
			return nil
		}
		for _, ch := range resp.Channels {
			typeName := "text"
			if ch.Type == 2 {
				typeName = "voice"
			}
			fmt.Printf("  %s  %s  (%s)\n", ch.ID, ch.Name, typeName)
		}
		return nil
	},
}

func init() {
	discordListChannelsCmd.Flags().StringVar(&discordListChannelsAccountFlag, "account", "", "Account name (required)")
}

// -- status --

var discordStatusAccountFlag string

var discordStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show gateway status for a Discord account",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(discordStatusAccountFlag)
		if name == "" {
			return fmt.Errorf("--account is required")
		}
		st, err := ReadGatewayStatus("discord", name)
		if err != nil {
			return fmt.Errorf("read status: %w", err)
		}
		printDiscordStatus(name, st)
		return nil
	},
}

func init() {
	discordStatusCmd.Flags().StringVar(&discordStatusAccountFlag, "account", "", "Account name (required)")
}

func printDiscordStatus(name string, st GatewayStatus) {
	data, _ := json.MarshalIndent(map[string]any{
		"account":      name,
		"status":       st.Status,
		"botUserId":    st.BotUserID,
		"botUsername":  st.BotUsername,
		"messagesIn":   st.MessagesIn,
		"messagesOut":  st.MessagesOut,
		"subscriptions": st.Subscriptions,
		"droppedBy":    st.DroppedBy,
	}, "", "  ")
	fmt.Println(string(data))
}
