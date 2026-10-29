package commands

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/nodev1"
)

var stopHookSocket string

// stopHookCmd is a v1 session's Claude Code Stop hook (projectmesh-io/api
// ADR 001 amendment, finish enforcement): it asks the lease's stop gate,
// on the socket the node named, whether the session may end yet, and
// prints the gate's refusal for claude to read. Anything that goes wrong
// lets the session stop — exit 0, nothing printed — since the node
// checks the finish again once the session has ended.
var stopHookCmd = &cobra.Command{
	Use:          nodev1.StopHookCommand + " --socket <path>",
	Short:        "A v1 session's Stop hook: refuse the session's end while its finish is owed",
	Hidden:       true,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if stopHookSocket == "" {
			return errors.New(nodev1.StopHookCommand + ": --socket is required")
		}
		if err := nodev1.RunStopHook(cmd.Context(), stopHookSocket, os.Stdin, cmd.OutOrStdout()); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v; letting the session stop\n", nodev1.StopHookCommand, err)
		}
		return nil
	},
}

func init() {
	stopHookCmd.Flags().StringVar(&stopHookSocket, "socket", "", "the lease's stop gate socket")
	rootCmd.AddCommand(stopHookCmd)
}
