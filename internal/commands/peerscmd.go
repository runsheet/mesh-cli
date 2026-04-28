package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var peersCmd = &cobra.Command{
	Use:   "peers",
	Short: "Manage local peers registrations",
	Long:  "Inspect and manage agents registered with the in-daemon peers broker (scope=machine).",
}

func init() {
	peersCmd.AddCommand(peersListCmd)
	peersCmd.AddCommand(peersStatusCmd)
	peersCmd.AddCommand(peersSetSummaryCmd)
}

var peersListTeamFlag string

var peersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List peers registered with the local daemon",
	Long:  "Queries the daemon's in-process peers broker. Shows all agents with a peers channel subscription.",
	RunE: func(cmd *cobra.Command, args []string) error {
		lc, err := newLocalClient()
		if err != nil {
			return err
		}
		path := "/peers/list"
		if peersListTeamFlag != "" {
			path += "?team=" + peersListTeamFlag
		}
		var resp struct {
			Peers []struct {
				PeerID     string   `json:"peer_id"`
				NodeID     string   `json:"node_id"`
				Teams      []string `json:"teams"`
				Summary    string   `json:"summary,omitempty"`
				LastSeenAt string   `json:"last_seen_at"`
			} `json:"peers"`
		}
		if err := lc.do(context.Background(), http.MethodGet, path, nil, &resp); err != nil {
			if strings.Contains(err.Error(), "daemon not running") {
				fmt.Println("peers: daemon stopped")
				fmt.Println("  start it with `mesh node start`")
				return nil
			}
			return err
		}
		if len(resp.Peers) == 0 {
			if peersListTeamFlag != "" {
				fmt.Printf("no peers registered in team %q\n", peersListTeamFlag)
			} else {
				fmt.Println("no peers registered")
			}
			return nil
		}
		for _, p := range resp.Peers {
			teams := strings.Join(p.Teams, ", ")
			if teams == "" {
				teams = "(none)"
			}
			lastSeen := p.LastSeenAt
			if t, err := time.Parse(time.RFC3339, p.LastSeenAt); err == nil {
				lastSeen = t.Local().Format("2006-01-02 15:04:05")
			}
			fmt.Printf("  %s  node=%s  teams=[%s]  last_seen=%s\n", p.PeerID, p.NodeID, teams, lastSeen)
			if p.Summary != "" {
				fmt.Printf("    summary: %s\n", p.Summary)
			}
		}
		return nil
	},
}

func init() {
	peersListCmd.Flags().StringVar(&peersListTeamFlag, "team", "", "Filter by team name")
}

var peersStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show peers broker status",
	Long:  "Shows all registered peers (same as list) plus a summary count.",
	RunE: func(cmd *cobra.Command, args []string) error {
		lc, err := newLocalClient()
		if err != nil {
			return err
		}
		var resp struct {
			Peers []json.RawMessage `json:"peers"`
		}
		if err := lc.do(context.Background(), http.MethodGet, "/peers/list", nil, &resp); err != nil {
			if strings.Contains(err.Error(), "daemon not running") {
				fmt.Println("peers status: daemon stopped")
				return nil
			}
			return err
		}
		fmt.Printf("peers registered: %d\n", len(resp.Peers))
		if len(resp.Peers) > 0 {
			fmt.Println("run `mesh peers list` for details")
		}
		return nil
	},
}

var (
	peersSetSummaryAgentFlag   string
	peersSetSummarySummaryFlag string
)

var peersSetSummaryCmd = &cobra.Command{
	Use:   "set-summary",
	Short: "Set the summary for a registered peer",
	Long:  "Updates the peer's self-description in the broker. The peer must be live (shim connected).",
	RunE: func(cmd *cobra.Command, args []string) error {
		agent := strings.TrimSpace(peersSetSummaryAgentFlag)
		summary := strings.TrimSpace(peersSetSummarySummaryFlag)
		if agent == "" {
			return fmt.Errorf("--agent is required")
		}
		if summary == "" {
			return fmt.Errorf("--summary is required")
		}

		lc, err := newLocalClient()
		if err != nil {
			return err
		}
		payload := map[string]string{"peer_id": agent, "summary": summary}
		var resp map[string]string
		if err := lc.do(context.Background(), http.MethodPost, "/peers/set-summary", payload, &resp); err != nil {
			if strings.Contains(err.Error(), "daemon not running") {
				fmt.Println("peers set-summary: daemon stopped")
				return nil
			}
			return err
		}
		fmt.Printf("peer %q summary updated\n", agent)
		return nil
	},
}

func init() {
	peersSetSummaryCmd.Flags().StringVar(&peersSetSummaryAgentFlag, "agent", "", "Agent name (peer_id) to update (required)")
	peersSetSummaryCmd.Flags().StringVar(&peersSetSummarySummaryFlag, "summary", "", "Summary text (required)")
}
