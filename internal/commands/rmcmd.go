package commands

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var rmForceFlag bool

var rmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Stop and remove a local agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if !rmForceFlag {
			reader := bufio.NewReader(os.Stdin)
			answer := promptLine(reader, fmt.Sprintf("stop and remove session %q? [y/N]: ", name))
			if strings.ToLower(strings.TrimSpace(answer)) != "y" {
				fmt.Println("Cancelled.")
				return nil
			}
		}
		lc, err := newLocalClient()
		if err != nil {
			return err
		}
		if err := lc.removeSession(context.Background(), name); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", name)
		return nil
	},
}

func init() {
	rmCmd.Flags().BoolVarP(&rmForceFlag, "force", "f", false, "Skip confirmation")
}
