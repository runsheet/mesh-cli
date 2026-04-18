package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart <name>",
	Short: "Kill and relaunch a local agent session (fresh conversation)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := NewLocalClient()
		if err != nil {
			return err
		}
		resp, err := client.RestartSession(context.Background(), args[0])
		if err != nil {
			return err
		}
		fmt.Printf("restarted %s (session %s)\n", resp["name"], resp["sessionId"])
		return nil
	},
}
