package commands

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	"projectmesh.io/mesh-cli/internal/auth"
	idaemon "projectmesh.io/mesh-cli/internal/daemon"
	meshv1 "projectmesh.io/mesh-cli/proto/meshv1"
)

var (
	sessionListNode   string
	sessionAttachNode string
	sessionNewUpNode  string
	sessionNewUpYes   bool
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Manage live Claude Code sessions on Mesh nodes",
}

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List live sessions on a node (defaults to the local daemon)",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		nodeID, err := resolveSessionNode(sess, sessionListNode)
		if err != nil {
			return err
		}
		client := newMeshClient(sess.BaseURL, sess.AccessToken)
		var resp struct {
			OK       bool `json:"ok"`
			Sessions []struct {
				AgentID         string `json:"agentId"`
				ClaudeSessionID string `json:"claudeSessionId"`
				Pid             int    `json:"pid"`
				State           string `json:"state"`
				StartedAt       string `json:"startedAt"`
				LastActivityAt  string `json:"lastActivityAt"`
			} `json:"sessions"`
		}
		if err := client.doRequest("POST", "/session/list", map[string]string{"nodeId": nodeID}, &resp); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "AGENT\tPID\tSTATE\tSTARTED\tLAST_ACTIVITY")
		for _, s := range resp.Sessions {
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", s.AgentID, s.Pid, s.State, s.StartedAt, s.LastActivityAt)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if len(resp.Sessions) == 0 {
			fmt.Println("No sessions running on this node.")
		}
		return nil
	},
}

var sessionAttachCmd = &cobra.Command{
	Use:   "attach <agent-id>",
	Short: "Attach a terminal view to a running session (stub: portal-only for v1)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_ = args
		return fmt.Errorf("session attach is not yet supported from the CLI; open the session in the Mesh portal instead")
	},
}

var sessionNewUpCmd = &cobra.Command{
	Use:   "new-up <agent-id>",
	Short: "Kill-and-restart a session without --resume (fresh conversation)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		nodeID, err := resolveSessionNode(sess, sessionNewUpNode)
		if err != nil {
			return err
		}
		if !sessionNewUpYes {
			reader := bufio.NewReader(os.Stdin)
			answer := promptLine(reader, fmt.Sprintf("Restart session for agent %s on node %s (conversation lost)? [y/N]: ", args[0], nodeID))
			if strings.ToLower(strings.TrimSpace(answer)) != "y" {
				fmt.Println("Cancelled.")
				return nil
			}
		}
		client := newMeshClient(sess.BaseURL, sess.AccessToken)
		var resp map[string]any
		payload := map[string]string{"nodeId": nodeID, "agentId": args[0]}
		if err := client.doRequest("POST", "/session/new-up", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

func init() {
	sessionListCmd.Flags().StringVar(&sessionListNode, "node", "", "Node slug or ID (defaults to local daemon)")
	sessionAttachCmd.Flags().StringVar(&sessionAttachNode, "node", "", "Node slug or ID (defaults to local daemon)")
	sessionNewUpCmd.Flags().StringVar(&sessionNewUpNode, "node", "", "Node slug or ID (defaults to local daemon)")
	sessionNewUpCmd.Flags().BoolVarP(&sessionNewUpYes, "yes", "y", false, "Skip confirmation prompt")

	sessionCmd.AddCommand(sessionListCmd, sessionAttachCmd, sessionNewUpCmd)
}

func resolveSessionNode(sess *auth.Session, flag string) (string, error) {
	if v := strings.TrimSpace(flag); v != "" {
		if looksLikeUUID(v) {
			return v, nil
		}
		id, err := lookupNodeBySlug(sess, v)
		if err != nil {
			return "", err
		}
		return id, nil
	}

	token, err := auth.LoadNodeToken()
	if err != nil || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("no --node given and no local daemon token found; pass --node <slug>")
	}
	id, err := probeLocalNodeID(sess.BaseURL, token)
	if err != nil {
		return "", fmt.Errorf("no --node given and local daemon unreachable: %w", err)
	}
	return id, nil
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func lookupNodeBySlug(sess *auth.Session, slug string) (string, error) {
	client := newMeshClient(sess.BaseURL, sess.AccessToken)
	var resp struct {
		OK    bool `json:"ok"`
		Nodes []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"nodes"`
	}
	if err := client.doRequest("GET", "/node/list", nil, &resp); err != nil {
		return "", err
	}
	for _, n := range resp.Nodes {
		if n.Slug == slug {
			return n.ID, nil
		}
	}
	return "", fmt.Errorf("no node with slug %q in your account", slug)
}

func probeLocalNodeID(baseURL, token string) (string, error) {
	addr := defaultGrpcAddrFor(baseURL)
	conn, err := idaemon.DialGrpc(addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	client := meshv1.NewNodeServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
	resp, err := client.Register(ctx, &meshv1.RegisterRequest{Version: "mesh-cli-probe"})
	if err != nil {
		return "", err
	}
	if resp.GetNodeId() == "" {
		return "", fmt.Errorf("register returned empty nodeId")
	}
	return resp.GetNodeId(), nil
}
