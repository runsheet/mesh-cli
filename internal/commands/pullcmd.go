package commands

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

var pullCmd = &cobra.Command{
	Use:   "pull <template-ref>",
	Short: "Fetch a template into the local cache without creating an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		lc, err := newLocalClient()
		if err != nil {
			return err
		}
		resp, err := lc.pullTemplate(context.Background(), args[0])
		if err != nil {
			return err
		}
		fmt.Printf("pulled %s\n  sha:   %s\n  cache: %s\n", args[0], resp.SHA, resp.CachePath)
		return nil
	},
}
