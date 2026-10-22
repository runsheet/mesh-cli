package commands

import (
	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/version"
)

var rootCmd = &cobra.Command{
	Use:     "mesh",
	Short:   "Mesh CLI — goals, work items and nodes on the runsheet platform",
	Long:    "Mesh CLI is the operator tool for Mesh. Save a console token once, register this machine as a node, then start it; set and watch goals from the same token.",
	Version: version.Resolved(),
	Example: "mesh auth token rs_…\nmesh node register --org acme --name \"Chris's laptop\"\nmesh node start\nmesh goal set goal.yaml && mesh goal plan ship-it plan.yaml && mesh goal watch ship-it",
}

func init() {
	// --version says what `mesh version` does: the tag and the commit
	rootCmd.SetVersionTemplate(version.Long() + "\n")
	rootCmd.AddCommand(authCmd, nodeCmd, goalCmd, mcpCmd)
	rootCmd.AddCommand(orgCmd, agentCmd, sessionCmd)
	rootCmd.AddCommand(runCmd, psCmd, attachCmd, logsCmd, restartCmd, rmCmd, inspectCmd)
	rootCmd.AddCommand(pullCmd, imagesCmd, templatesCmd, localUpdateCmd)
	rootCmd.AddCommand(peersCmd, productCmd)
	rootCmd.AddCommand(emergencyStopCmd)
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
