package commands

import (
	"context"

	"github.com/spf13/cobra"
)

var inspectCmd = &cobra.Command{
	Use:   "inspect <name>",
	Short: "Show the definition + runtime state of a local agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		lc, err := newLocalClient()
		if err != nil {
			return err
		}
		out, err := lc.inspectSession(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(out)
	},
}
