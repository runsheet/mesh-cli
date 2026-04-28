package commands

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	itpl "projectmesh.io/mesh-cli/internal/template"
)

var localUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Re-resolve the template ref and restart the agent if the SHA moved",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		lc, err := newLocalClient()
		if err != nil {
			return err
		}
		resp, err := lc.updateSession(context.Background(), args[0])
		if err != nil {
			return err
		}
		oldSha, _ := resp["oldSha"].(string)
		newSha, _ := resp["newSha"].(string)
		status, _ := resp["status"].(string)

		if status == "up-to-date" {
			fmt.Printf("up-to-date %s (sha %s)\n", args[0], itpl.ShortSHA(newSha))
			return nil
		}
		fmt.Printf("updated %s (template %s → %s)\n", args[0], itpl.ShortSHA(oldSha), itpl.ShortSHA(newSha))
		if restarted, _ := resp["restarted"].(bool); restarted {
			if sid, _ := resp["sessionId"].(string); sid != "" {
				fmt.Printf("restarted session %s\n", sid)
			}
		}
		return nil
	},
}
