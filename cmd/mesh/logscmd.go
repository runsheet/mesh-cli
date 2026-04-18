package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"
)

var logsFollowFlag bool

var logsCmd = &cobra.Command{
	Use:   "logs <name>",
	Short: "Print the log ring buffer for a local agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := NewLocalClient()
		if err != nil {
			return err
		}
		return client.Logs(context.Background(), args[0], logsFollowFlag, os.Stdout)
	},
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFollowFlag, "follow", "f", false, "Stream new output as it arrives")
}
