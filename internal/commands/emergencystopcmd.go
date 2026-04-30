package commands

// mesh emergency-stop — spec 31 §10.3.
//
// Revokes the shipper agent's node token and sets the emergency_stop flag in
// agents.metadata.shipper_config so the in-flight Shipper session halts on
// its next phase-transition poll.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var emergencyStopRepo string

var emergencyStopCmd = &cobra.Command{
	Use:   "emergency-stop",
	Short: "Immediately halt the shipper agent for one or all repos",
	Long: `Revoke the Shipper agent's node token and set the emergency_stop flag.

Any in-flight ship operation will halt at its next phase-transition checkpoint.
To resume, rotate the node token (mesh node rotate-token) and clear the flag
via POST /agent/update with metadata.shipper_config.emergency_stop = false.

The emergency stop does not delete branches or PRs — state remains inspectable.`,
	Example: "mesh emergency-stop --repo mesh-api\nmesh emergency-stop --repo all",
	RunE:    runEmergencyStop,
}

func init() {
	emergencyStopCmd.Flags().StringVar(&emergencyStopRepo, "repo",
		"all", "Repo scope: mesh-api | mesh-portal | mesh-cli | all")
}

func runEmergencyStop(cmd *cobra.Command, args []string) error {
	sess, err := requireSession()
	if err != nil {
		return err
	}

	client := newMeshClientForSession(sess)

	// Determine which shipper agent names to halt.
	var agentNames []string
	switch emergencyStopRepo {
	case "mesh-api":
		agentNames = []string{"shipper-api"}
	case "mesh-portal":
		agentNames = []string{"shipper-portal"}
	case "mesh-cli":
		agentNames = []string{"shipper-cli"}
	case "all":
		agentNames = []string{"shipper-api", "shipper-portal", "shipper-cli"}
	default:
		return fmt.Errorf("unknown repo %q; expected mesh-api | mesh-portal | mesh-cli | all", emergencyStopRepo)
	}

	// 1. Fetch agent list to resolve agent IDs for the target shippers.
	var agentListResp struct {
		OK     bool `json:"ok"`
		Agents []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Metadata struct {
				ShipperConfig *struct {
					EmergencyStop bool `json:"emergency_stop"`
				} `json:"shipper_config"`
			} `json:"metadata"`
			PinnedNodeID string `json:"pinned_node_id"`
		} `json:"agents"`
	}
	if err := client.doRequest("GET", "/agent/list", nil, &agentListResp); err != nil {
		return fmt.Errorf("fetch agent list: %w", err)
	}

	agentsByName := make(map[string]struct {
		ID           string
		PinnedNodeID string
	})
	for _, a := range agentListResp.Agents {
		agentsByName[a.Name] = struct {
			ID           string
			PinnedNodeID string
		}{ID: a.ID, PinnedNodeID: a.PinnedNodeID}
	}

	var halted, skipped []string

	for _, name := range agentNames {
		agent, ok := agentsByName[name]
		if !ok {
			fmt.Printf("  [skip] %s: agent not found in this account\n", name)
			skipped = append(skipped, name)
			continue
		}

		// 2. Revoke the node token via POST /node/revoke-token.
		// We look up the node's tokens via the agent's pinned_node_id.
		if agent.PinnedNodeID != "" {
			revokeBody := map[string]any{"nodeId": agent.PinnedNodeID}
			var revokeResp struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if revokeErr := client.doRequest("POST", "/node/revoke-token", revokeBody, &revokeResp); revokeErr != nil {
				fmt.Printf("  [warn] %s: revoke-token failed: %v\n", name, revokeErr)
			} else if !revokeResp.OK {
				fmt.Printf("  [warn] %s: revoke-token: %s\n", name, revokeResp.Error)
			} else {
				fmt.Printf("  [ok]   %s: node token revoked (node=%s)\n", name, agent.PinnedNodeID)
			}
		} else {
			fmt.Printf("  [warn] %s: no pinned node; token revocation skipped\n", name)
		}

		// 3. Set emergency_stop = true in agents.metadata.shipper_config.
		metadataPatch := map[string]any{
			"shipper_config": map[string]any{
				"emergency_stop": true,
			},
		}
		metaJSON, _ := json.Marshal(metadataPatch)
		updateBody := map[string]any{
			"agentId":  agent.ID,
			"metadata": json.RawMessage(metaJSON),
		}
		var updateResp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if updateErr := client.doRequest("POST", "/agent/update", updateBody, &updateResp); updateErr != nil {
			fmt.Printf("  [warn] %s: set emergency_stop failed: %v\n", name, updateErr)
		} else if !updateResp.OK {
			fmt.Printf("  [warn] %s: set emergency_stop: %s\n", name, updateResp.Error)
		} else {
			fmt.Printf("  [ok]   %s: emergency_stop flag set\n", name)
		}

		halted = append(halted, name)
	}

	fmt.Println()
	if len(halted) > 0 {
		fmt.Printf("Emergency stop applied to: %s\n", strings.Join(halted, ", "))
		fmt.Println("In-flight ship operations will halt at the next phase-transition checkpoint.")
		fmt.Println("To resume: mesh node rotate-token, then clear the flag via POST /agent/update.")
	}
	if len(skipped) > 0 {
		fmt.Printf("Skipped (not found): %s\n", strings.Join(skipped, ", "))
	}

	return nil
}
