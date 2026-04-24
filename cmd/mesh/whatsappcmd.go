package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

// whatsappCmd is the root of the `mesh whatsapp` command tree.
var whatsappCmd = &cobra.Command{
	Use:   "whatsapp",
	Short: "Manage WhatsApp accounts",
	Long:  "Pair WhatsApp accounts with the node, list accounts and chats, and check gateway status.",
}

func init() {
	whatsappCmd.AddCommand(whatsappPairCmd)
	whatsappCmd.AddCommand(whatsappUnpairCmd)
	whatsappCmd.AddCommand(whatsappListAccountsCmd)
	whatsappCmd.AddCommand(whatsappListChatsCmd)
	whatsappCmd.AddCommand(whatsappStatusCmd)
}

// -- pair --

var whatsappPairAccountFlag string

var whatsappPairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair a WhatsApp account with this node via QR code",
	Long: "pair runs the WhatsApp Web QR pairing handshake in the terminal. " +
		"Open WhatsApp on your phone → Linked Devices → Link a Device, then scan the QR code. " +
		"On success the account is registered and the daemon is signalled to start the gateway.",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(whatsappPairAccountFlag)
		if name == "" {
			return fmt.Errorf("--account is required")
		}
		return runWhatsappPair(name)
	},
}

func init() {
	whatsappPairCmd.Flags().StringVar(&whatsappPairAccountFlag, "account", "", "Account name (required)")
}

// runWhatsappPair runs the interactive QR pairing flow in the CLI process.
// It creates the account directory, runs the whatsmeow pairing handshake,
// renders QR codes in the terminal, and on success writes account.yaml and
// signals the daemon.
func runWhatsappPair(name string) error {
	storePath, err := WhatsappStorePath(name)
	if err != nil {
		return fmt.Errorf("store path: %w", err)
	}

	// Create the account dir before writing store.db.
	meta := whatsappAccountMeta{
		Name:     name,
		Mode:     "web",
		PairedAt: time.Now().UTC(),
	}
	if err := WriteWhatsappAccount(name, meta); err != nil {
		return fmt.Errorf("create account: %w", err)
	}

	container, err := sqlstore.New(context.Background(), "sqlite", "file:"+storePath+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		return fmt.Errorf("open whatsapp store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return fmt.Errorf("get device: %w", err)
	}

	if deviceStore.ID != nil {
		return fmt.Errorf("account %q is already paired (store already contains a device ID); unpair first", name)
	}

	client := whatsmeow.NewClient(deviceStore, waLog.Noop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	qrCh, err := client.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("get QR channel: %w", err)
	}

	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect for pairing: %w", err)
	}
	defer client.Disconnect()

	fmt.Printf("Pairing WhatsApp account %q\n", name)
	fmt.Println("Open WhatsApp → Linked Devices → Link a Device, then scan the QR code below.")
	fmt.Println()

	for item := range qrCh {
		switch item.Event {
		case "code":
			fmt.Print("\033[H\033[2J") // clear screen
			fmt.Printf("Pairing WhatsApp account %q — scan with your phone:\n\n", name)
			qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
			fmt.Printf("\n(QR refreshes every %s)\n", item.Timeout.Round(time.Second))

		case "success":
			fmt.Println("\nPairing successful!")
			// Persist account meta. PhoneNumber will be filled by the gateway on first connect.
			meta.PairedAt = time.Now().UTC()
			if err := WriteWhatsappAccount(name, meta); err != nil {
				fmt.Printf("  warning: could not write account.yaml: %v\n", err)
			} else {
				fmt.Printf("  account.yaml written to ~/.mesh/whatsapp-accounts/%s/\n", name)
			}

			// Signal daemon to start the gateway.
			if err := whatsappDaemonPair(name); err != nil {
				fmt.Printf("  note: daemon signal failed (%v); gateway starts on next node boot\n", err)
			} else {
				fmt.Println("  daemon notified; gateway starting")
			}
			return nil

		case "timeout":
			return fmt.Errorf("pairing timed out — no QR scanned within the window; try again")

		case "error":
			return fmt.Errorf("pairing error: %v", item.Error)

		default:
			return fmt.Errorf("pairing failed with status %q", item.Event)
		}
	}

	return fmt.Errorf("QR channel closed without a result")
}

// whatsappDaemonPair signals the daemon to start the gateway for a newly-paired account.
func whatsappDaemonPair(accountName string) error {
	c, err := NewLocalClient()
	if err != nil {
		return err
	}
	payload := map[string]string{"account": accountName}
	return c.do(context.Background(), http.MethodPost, "/whatsapp/pair", payload, nil)
}

// -- unpair --

var whatsappUnpairAccountFlag string

var whatsappUnpairCmd = &cobra.Command{
	Use:   "unpair",
	Short: "Remove a paired WhatsApp account",
	Long:  "unpair refuses if any agent references the account. On success it stops the gateway and removes the account directory.",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(whatsappUnpairAccountFlag)
		if name == "" {
			return fmt.Errorf("--account is required")
		}

		ref, agentName, err := agentReferencesWhatsappAccount(name)
		if err != nil {
			return fmt.Errorf("check agent refs: %w", err)
		}
		if ref {
			return fmt.Errorf("account %q is still referenced by agent %q; remove or update the agent first", name, agentName)
		}

		// Signal daemon to stop the gateway before removing the dir.
		_ = whatsappDaemonUnpair(name) // best-effort

		removed, err := RemoveWhatsappAccount(name)
		if err != nil {
			return fmt.Errorf("remove account: %w", err)
		}
		if !removed {
			return fmt.Errorf("whatsapp account %q not found", name)
		}
		fmt.Printf("whatsapp account %q removed\n", name)
		return nil
	},
}

func init() {
	whatsappUnpairCmd.Flags().StringVar(&whatsappUnpairAccountFlag, "account", "", "Account name (required)")
}

func whatsappDaemonUnpair(accountName string) error {
	c, err := NewLocalClient()
	if err != nil {
		return err
	}
	payload := map[string]string{"account": accountName}
	return c.do(context.Background(), http.MethodPost, "/whatsapp/unpair", payload, nil)
}

// -- list-accounts --

var whatsappListAccountsCmd = &cobra.Command{
	Use:   "list-accounts",
	Short: "List paired WhatsApp accounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		names, err := ListWhatsappAccounts()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("no WhatsApp accounts paired")
			return nil
		}
		for _, n := range names {
			meta, err := LoadWhatsappAccountMeta(n)
			if err != nil {
				fmt.Printf("  %s  (error reading meta: %v)\n", n, err)
				continue
			}
			pairedAt := ""
			if !meta.PairedAt.IsZero() {
				pairedAt = "  paired " + meta.PairedAt.Format("2006-01-02")
			}
			phone := ""
			if meta.PhoneNumber != "" {
				phone = "  phone=" + meta.PhoneNumber
			}
			fmt.Printf("  %s%s%s\n", n, pairedAt, phone)
		}
		return nil
	},
}

// -- list-chats --

var whatsappListChatsAccountFlag string

var whatsappListChatsCmd = &cobra.Command{
	Use:   "list-chats",
	Short: "List recent chats for a WhatsApp account (requires live gateway)",
	Long: "list-chats reads the whatsmeow store for the named account and prints " +
		"known chat JIDs. The gateway must have been connected at least once so the " +
		"store is populated.",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(whatsappListChatsAccountFlag)
		if name == "" {
			return fmt.Errorf("--account is required")
		}

		storePath, err := WhatsappStorePath(name)
		if err != nil {
			return fmt.Errorf("store path: %w", err)
		}
		if _, err := os.Stat(storePath); os.IsNotExist(err) {
			return fmt.Errorf("no store.db for account %q; pair the account first", name)
		}

		container, err := sqlstore.New(context.Background(), "sqlite", "file:"+storePath+"?_foreign_keys=on", waLog.Noop)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}

		contacts, err := container.GetFirstDevice(context.Background())
		if err != nil {
			return fmt.Errorf("get device: %w", err)
		}
		if contacts.ID == nil {
			return fmt.Errorf("account %q is not paired (no device in store)", name)
		}

		client := whatsmeow.NewClient(contacts, waLog.Noop)
		allContacts, err := client.Store.Contacts.GetAllContacts(context.Background())
		if err != nil {
			return fmt.Errorf("list contacts: %w", err)
		}
		if len(allContacts) == 0 {
			fmt.Println("no contacts in store (connect the gateway first so WhatsApp can sync)")
			return nil
		}
		for jid, info := range allContacts {
			name := info.FullName
			if name == "" {
				name = info.PushName
			}
			if name == "" {
				name = "-"
			}
			fmt.Printf("  %s  %s\n", jid.String(), name)
		}
		return nil
	},
}

func init() {
	whatsappListChatsCmd.Flags().StringVar(&whatsappListChatsAccountFlag, "account", "", "Account name (required)")
}

// -- status --

var whatsappStatusAccountFlag string

var whatsappStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show gateway status for a WhatsApp account",
	Long: "Asks the daemon for live supervisor + gateway state. Without --account, " +
		"prints status for every paired account. If the daemon isn't running, prints " +
		"'daemon stopped' rather than reading stale on-disk status files.",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(whatsappStatusAccountFlag)

		path := "/whatsapp/status"
		if name != "" {
			path += "?account=" + name
		}
		c, err := NewLocalClient()
		if err != nil {
			return err
		}
		var resp struct {
			Accounts []WhatsappAccountStatus `json:"accounts"`
		}
		if err := c.do(context.Background(), http.MethodGet, path, nil, &resp); err != nil {
			if strings.Contains(err.Error(), "daemon not running") {
				fmt.Println("status: daemon stopped")
				fmt.Println("  start it with `mesh node start` (or `systemctl --user start mesh-node`)")
				return nil
			}
			return err
		}
		if len(resp.Accounts) == 0 {
			if name != "" {
				fmt.Printf("whatsapp account %q not paired\n", name)
				return nil
			}
			fmt.Println("no WhatsApp accounts paired")
			return nil
		}
		multi := len(resp.Accounts) > 1
		for i, st := range resp.Accounts {
			if multi && i > 0 {
				fmt.Println()
			}
			printWhatsappAccountStatus(st)
		}
		return nil
	},
}

func init() {
	whatsappStatusCmd.Flags().StringVar(&whatsappStatusAccountFlag, "account", "", "Account name (omit to print status for every paired account)")
}

func printWhatsappAccountStatus(st WhatsappAccountStatus) {
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
		out["messagesIn"] = st.Gateway.MessagesIn
		out["messagesOut"] = st.Gateway.MessagesOut
		out["subscriptions"] = st.Gateway.Subscriptions
	default:
		out["status"] = "live but no metrics yet"
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(data))
}
