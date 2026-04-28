package commands

import (
	"context"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var (
	logsFollowFlag bool
	logsRawFlag    bool
)

var logsCmd = &cobra.Command{
	Use:   "logs <name>",
	Short: "Print the log ring buffer for a local agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		lc, err := newLocalClient()
		if err != nil {
			return err
		}
		var out io.Writer = os.Stdout
		if !logsRawFlag {
			out = newAnsiStripper(os.Stdout)
		}
		return lc.logs(context.Background(), args[0], logsFollowFlag, out)
	},
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFollowFlag, "follow", "f", false, "Stream new output as it arrives")
	logsCmd.Flags().BoolVar(&logsRawFlag, "raw", false, "Preserve ANSI escape sequences (default strips them)")
}
