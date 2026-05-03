package commands

// mesh emergency-stop — spec 31 §10.3, updated spec 36.
//
// Revokes the shipper agent's node token and sets the emergency_stop flag in
// agents.metadata.shipper_config so the in-flight ship skill halts on its
// next phase-transition sentinel check.
//
// Also writes ~/.mesh/emergency-stop as a local sentinel file that the ship
// skill reads before each phase transition.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var emergencyStopRepo string
var emergencyStopClear bool

var emergencyStopCmd = &cobra.Command{
	Use:   "emergency-stop",
	Short: "Immediately halt any in-flight ship operation",
	Long: `Write the emergency-stop sentinel and revoke the shipper agent's node token.

The ship skill reads ~/.mesh/emergency-stop before each phase transition. If the
file is present and non-empty, it halts immediately and sets the task to
ship_failed.

Use --clear to remove the sentinel and re-enable shipping.

The emergency stop does not delete branches or PRs — state remains inspectable.`,
	Example: "mesh emergency-stop --repo mesh-api\nmesh emergency-stop --repo all\nmesh emergency-stop --clear",
	RunE:    runEmergencyStop,
}

func init() {
	emergencyStopCmd.Flags().StringVar(&emergencyStopRepo, "repo",
		"all", "Repo scope: mesh-api | mesh-portal | mesh-cli | all")
	emergencyStopCmd.Flags().BoolVar(&emergencyStopClear, "clear",
		false, "Clear the emergency-stop sentinel (re-enables shipping)")
}

// sentinelPath returns the path to the emergency-stop sentinel file.
func sentinelPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "emergency-stop"), nil
}

func runEmergencyStop(cmd *cobra.Command, args []string) error {
	sp, err := sentinelPath()
	if err != nil {
		return fmt.Errorf("resolve sentinel path: %w", err)
	}

	if emergencyStopClear {
		if err := os.Remove(sp); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove sentinel: %w", err)
		}
		fmt.Println("Emergency stop cleared. Sentinel removed.")
		fmt.Println("Note: API-side emergency_stop flag must be cleared separately via POST /agent/update.")
		return nil
	}

	// Write the local sentinel file. The ship skill reads this before each
	// phase transition and halts if present.
	if err := os.MkdirAll(filepath.Dir(sp), 0o700); err != nil {
		return fmt.Errorf("create sentinel dir: %w", err)
	}
	sentinelContent := fmt.Sprintf("%s scope=%s\n", time.Now().UTC().Format(time.RFC3339), emergencyStopRepo)
	if err := os.WriteFile(sp, []byte(sentinelContent), 0o600); err != nil {
		return fmt.Errorf("write sentinel: %w", err)
	}
	fmt.Printf("  [ok]   sentinel written: %s\n", sp)

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
		fmt.Println("To resume: mesh emergency-stop --clear, then mesh node rotate-token, then clear the API flag via POST /agent/update.")
	}
	if len(skipped) > 0 {
		fmt.Printf("Skipped (not found): %s\n", strings.Join(skipped, ", "))
	}

	return nil
}
