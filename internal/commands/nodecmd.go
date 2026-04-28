package commands

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/auth"
	idaemon "projectmesh.io/mesh-cli/internal/daemon"
	"projectmesh.io/mesh-cli/internal/node"
)

var (
	nodeBaseURLFlag  string
	nodeGrpcAddrFlag string
	nodeRegTimeout   int
)

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Manage the local Mesh node daemon",
}

var nodeRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Pair this machine with the Mesh portal and save a node token",
	RunE: func(cmd *cobra.Command, args []string) error {
		base := resolveBaseURL(nodeBaseURLFlag)
		portal := derivePortalURL(base)

		code, err := node.RequestNodeRegistrationCode(base)
		if err != nil {
			return fmt.Errorf("request registration code: %w", err)
		}

		fmt.Printf("Paste this code into the portal at %s/nodes to register this machine:\n\n    %s\n\nWaiting for the portal to redeem the code (up to %d minutes)...\n",
			portal, code, nodeRegTimeout)

		nodeID, err := node.PollAndSaveNodeToken(base, code, time.Duration(nodeRegTimeout)*time.Minute)
		if err != nil {
			return err
		}

		fmt.Printf("Node registered. nodeId=%s token saved to ~/.mesh/node-token\n", nodeID)
		fmt.Println("Start the daemon with: mesh node start")
		return nil
	},
}

var nodeStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Run the Mesh node daemon in the foreground",
	RunE: func(cmd *cobra.Command, args []string) error {
		base := resolveBaseURL(nodeBaseURLFlag)
		addr := nodeGrpcAddrFlag
		if strings.TrimSpace(addr) == "" {
			addr = defaultGrpcAddrFor(base)
		}
		fmt.Printf("Starting mesh node daemon baseUrl=%s grpcAddr=%s\n", base, addr)
		return runDaemon(base, addr)
	},
}

var nodeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "List your registered nodes and their last-seen status",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		base := resolveBaseURL(nodeBaseURLFlag)
		if base == "" {
			base = sess.BaseURL
		}

		client := newMeshClient(base, sess.AccessToken)
		var resp struct {
			OK    bool `json:"ok"`
			Nodes []struct {
				ID         string  `json:"id"`
				Name       string  `json:"name"`
				Slug       string  `json:"slug"`
				Status     string  `json:"status"`
				LastSeenAt *string `json:"last_seen_at"`
				OwnerID    string  `json:"owner_id"`
				Version    *string `json:"version"`
			} `json:"nodes"`
		}
		if err := client.doRequest("GET", "/node/list", nil, &resp); err != nil {
			return err
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSLUG\tSTATUS\tLAST_SEEN")
		found := 0
		for _, n := range resp.Nodes {
			if n.OwnerID != sess.UserID {
				continue
			}
			found++
			last := "-"
			if n.LastSeenAt != nil {
				last = *n.LastSeenAt
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.Name, n.Slug, n.Status, last)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if found == 0 {
			fmt.Println("No nodes registered to your account. Run `mesh node register` to pair this machine.")
		}
		return nil
	},
}

var nodeLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear the saved node token from this machine",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := auth.ClearNodeToken(); err != nil {
			return err
		}
		fmt.Println("Node token cleared from ~/.mesh/node-token")
		fmt.Println("To revoke the token server-side, use the portal's node-detail page.")
		return nil
	},
}

func init() {
	nodeRegisterCmd.Flags().StringVar(&nodeBaseURLFlag, "base-url", "", "Mesh API base URL (defaults to saved session)")
	nodeRegisterCmd.Flags().IntVar(&nodeRegTimeout, "timeout-minutes", 10, "Minutes to wait for portal to redeem the code")

	nodeStartCmd.Flags().StringVar(&nodeBaseURLFlag, "base-url", "", "Mesh API base URL (defaults to saved session)")
	nodeStartCmd.Flags().StringVar(&nodeGrpcAddrFlag, "grpc-addr", "", "gRPC host:port (defaults to base-url host with :8081)")

	nodeStatusCmd.Flags().StringVar(&nodeBaseURLFlag, "base-url", "", "Mesh API base URL (defaults to saved session)")

	nodeCmd.AddCommand(nodeRegisterCmd, nodeStartCmd, nodeStatusCmd, nodeLogoutCmd)
}

func resolveBaseURL(flag string) string {
	if v := strings.TrimSpace(flag); v != "" {
		return normalizeBaseURL(v)
	}
	if sess, err := auth.LoadSession(); err == nil && sess.BaseURL != "" {
		return normalizeBaseURL(sess.BaseURL)
	}
	if v := strings.TrimSpace(os.Getenv("MESH_API_URL")); v != "" {
		return normalizeBaseURL(v)
	}
	return "https://api.projectmesh.io"
}

func derivePortalURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "https://app.projectmesh.io"
	}
	host := u.Host
	if strings.HasPrefix(host, "api.") {
		host = "app." + strings.TrimPrefix(host, "api.")
	}
	return fmt.Sprintf("%s://%s", u.Scheme, host)
}

func defaultGrpcAddrFor(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "127.0.0.1:8081"
	}
	host := u.Hostname()
	return fmt.Sprintf("%s:8081", host)
}

// runDaemon wires WriteClaudeSettings callback and delegates to internal/daemon.
func runDaemon(baseURL, grpcAddr string) error {
	return idaemon.RunDaemon(baseURL, grpcAddr,
		writeClaudeSettingsForDaemon,
		probeLocalNodeID,
	)
}
