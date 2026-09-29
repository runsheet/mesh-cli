package commands

import (
	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/harness"
)

// sandboxProbeCmd is what a Codex preflight runs inside Codex's sandbox
// (harness.SandboxProbe): it binds 127.0.0.1:0, prints the address and
// exits 0, or exits non-zero with the listen's error — `socket:
// operation not permitted` in a sandbox without the network.
var sandboxProbeCmd = &cobra.Command{
	Use:          "sandbox-probe",
	Short:        "Bind a loopback socket and print its address (the Codex preflight's sandbox probe)",
	Hidden:       true,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return harness.ProbeLoopback(cmd.OutOrStdout())
	},
}

func init() {
	rootCmd.AddCommand(sandboxProbeCmd)
}
