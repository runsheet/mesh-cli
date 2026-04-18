package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

var localUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Re-resolve the template ref and restart the agent if the SHA moved",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := NewLocalClient()
		if err != nil {
			return err
		}
		resp, err := client.UpdateSession(context.Background(), args[0])
		if err != nil {
			return err
		}
		oldSha, _ := resp["oldSha"].(string)
		newSha, _ := resp["newSha"].(string)
		status, _ := resp["status"].(string)

		if status == "up-to-date" {
			fmt.Printf("up-to-date %s (sha %s)\n", args[0], ShortSHA(newSha))
			return nil
		}
		fmt.Printf("updated %s (template %s → %s)\n", args[0], ShortSHA(oldSha), ShortSHA(newSha))
		if restarted, _ := resp["restarted"].(bool); restarted {
			if sid, _ := resp["sessionId"].(string); sid != "" {
				fmt.Printf("restarted session %s\n", sid)
			}
		}
		return nil
	},
}
