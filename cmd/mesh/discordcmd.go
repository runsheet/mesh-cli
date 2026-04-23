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
	Long:  "Without --account, lists channels for every paired account. Each account's gateway must be live.",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(discordListChannelsAccountFlag)
		var names []string
		if name != "" {
			names = []string{name}
		} else {
			all, err := ListDiscordAccounts()
			if err != nil {
				return fmt.Errorf("list accounts: %w", err)
			}
			if len(all) == 0 {
				fmt.Println("no Discord accounts paired")
				return nil
			}
			names = all
		}
		c, err := NewLocalClient()
		if err != nil {
			return err
		}
		multi := len(names) > 1
		for i, n := range names {
			if multi {
				if i > 0 {
					fmt.Println()
				}
				fmt.Printf("[%s]\n", n)
			}
			var resp struct {
				Channels []struct {
					ID      string `json:"id"`
					Name    string `json:"name"`
					Type    int    `json:"type"`
					GuildID string `json:"guildId"`
				} `json:"channels"`
			}
			if err := c.do(context.Background(), http.MethodGet, "/discord/list-channels?account="+n, nil, &resp); err != nil {
				if multi {
					fmt.Printf("  (error: %v)\n", err)
					continue
				}
				return fmt.Errorf("list-channels: %w", err)
			}
			if len(resp.Channels) == 0 {
				fmt.Println("  no channels visible to this bot")
				continue
			}
			// Best-effort subscription fetch so we can annotate each channel
			// with its routing fate ("owned by agent X", "guild default", etc.).
			// If the status call fails we just skip the annotations.
			annotations := lookupDiscordChannelAnnotations(c, n)
			for _, ch := range resp.Channels {
				typeName := "text"
				if ch.Type == 2 {
					typeName = "voice"
				}
				ann := annotations(ch.ID, ch.GuildID)
				if ann != "" {
					fmt.Printf("  %s  %s  (%s)  %s\n", ch.ID, ch.Name, typeName, ann)
				} else {
					fmt.Printf("  %s  %s  (%s)\n", ch.ID, ch.Name, typeName)
				}
			}
		}
		return nil
	},
}

// lookupDiscordChannelAnnotations returns a function that maps (channelId,
// guildId) → a human-readable routing annotation for `discord list-channels`.
// Best effort: if the status fetch fails the returned function always returns
// "" so the caller can fall back to unannotated output.
func lookupDiscordChannelAnnotations(c *LocalClient, account string) func(channelID, guildID string) string {
	var resp struct {
		Accounts []DiscordAccountStatus `json:"accounts"`
	}
	if err := c.do(context.Background(), http.MethodGet, "/discord/status?account="+account, nil, &resp); err != nil {
		return func(string, string) string { return "" }
	}
	var gw *GatewayStatus
	for i := range resp.Accounts {
		if resp.Accounts[i].Account == account && resp.Accounts[i].Gateway != nil {
			gw = resp.Accounts[i].Gateway
			break
		}
	}
	if gw == nil {
		return func(string, string) string { return "" }
	}
	type ownerInfo struct {
		agent string
		index int
		kind  string // "channel" | "guild-override" | "guild-default"
	}
	channelOwners := map[string]ownerInfo{}
	guildOwners := map[string]ownerInfo{}
	for _, s := range gw.Subscriptions {
		switch s.SubscriptionMode {
		case "guild":
			guildOwners[s.RoutingKey] = ownerInfo{s.Agent, s.Index, "guild-default"}
			for _, cid := range s.Overrides {
				channelOwners[cid] = ownerInfo{s.Agent, s.Index, "guild-override"}
			}
		default: // "channel" or empty (legacy)
			channelOwners[s.RoutingKey] = ownerInfo{s.Agent, s.Index, "channel"}
		}
	}
	renderOwner := func(o ownerInfo) string {
		return fmt.Sprintf("→ %s (%s)", o.agent, o.kind)
	}
	return func(channelID, guildID string) string {
		if o, ok := channelOwners[channelID]; ok {
			return renderOwner(o)
		}
		if o, ok := guildOwners[guildID]; ok {
			return renderOwner(o)
		}
		return "→ unrouted"
	}
}

func init() {
	discordListChannelsCmd.Flags().StringVar(&discordListChannelsAccountFlag, "account", "", "Account name (omit to list channels for every paired account)")
}

// -- status --

var discordStatusAccountFlag string

var discordStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show gateway status for a Discord account",
	Long: "Asks the daemon for live supervisor + gateway state. Without --account, " +
		"prints status for every paired account. If the daemon isn't running, prints " +
		"'daemon stopped' rather than reading stale on-disk status files.",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(discordStatusAccountFlag)

		path := "/discord/status"
		if name != "" {
			path += "?account=" + name
		}
		c, err := NewLocalClient()
		if err != nil {
			return err
		}
		var resp struct {
			Accounts []DiscordAccountStatus `json:"accounts"`
		}
		if err := c.do(context.Background(), http.MethodGet, path, nil, &resp); err != nil {
			// daemon-not-running surfaces as the canonical message from
			// wrapDialErr; anything else is a real failure worth bubbling up.
			if strings.Contains(err.Error(), "daemon not running") {
				fmt.Println("status: daemon stopped")
				fmt.Println("  start it with `mesh node start` (or `systemctl --user start mesh-node`)")
				return nil
			}
			return err
		}
		if len(resp.Accounts) == 0 {
			if name != "" {
				fmt.Printf("discord account %q not paired\n", name)
				return nil
			}
			fmt.Println("no Discord accounts paired")
			return nil
		}
		multi := len(resp.Accounts) > 1
		for i, st := range resp.Accounts {
			if multi && i > 0 {
				fmt.Println()
			}
			printDiscordAccountStatus(st)
		}
		return nil
	},
}

func init() {
	discordStatusCmd.Flags().StringVar(&discordStatusAccountFlag, "account", "", "Account name (omit to print status for every paired account)")
}

// printDiscordAccountStatus renders one account's status. The render reflects
// supervisor state + liveness, not the raw status.json (which can be stale
// after a gateway crash).
func printDiscordAccountStatus(st DiscordAccountStatus) {
	out := map[string]any{
		"account":         st.Account,
		"supervisorState": st.SupervisorState,
	}
	switch {
	case !st.Supervised:
		out["status"] = "idle: not referenced by any agent"
	case st.Supervised && !st.Live:
		out["status"] = "starting or restarting (gateway not reachable)"
	case st.Supervised && st.Live && st.Gateway != nil:
		out["status"] = st.Gateway.Status
		out["botUserId"] = st.Gateway.BotUserID
		out["botUsername"] = st.Gateway.BotUsername
		out["messagesIn"] = st.Gateway.MessagesIn
		out["messagesOut"] = st.Gateway.MessagesOut
		out["subscriptions"] = st.Gateway.Subscriptions
		out["droppedBy"] = st.Gateway.DroppedBy
	default:
		out["status"] = "live but no metrics yet"
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(data))
}
