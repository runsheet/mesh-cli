package commands

import (
	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/version"
)

var rootCmd = &cobra.Command{
	Use:     "mesh",
	Short:   "Mesh CLI — AI, orgs, agents, channels",
	Long:    "Mesh CLI is the operator tool for the Mesh control plane. Sign in once, then manage orgs, agents, and sessions from your saved Mesh session.",
	Version: version.Version,
	Example: "mesh auth login\nmesh agent create --name nova --pinned-node-id <node-id> --model claude-sonnet-4-6\nmesh node start",
}

func init() {
	rootCmd.AddCommand(authCmd, orgCmd, nodeCmd, agentCmd, sessionCmd)
	rootCmd.AddCommand(runCmd, psCmd, attachCmd, logsCmd, restartCmd, rmCmd, inspectCmd)
	rootCmd.AddCommand(pullCmd, imagesCmd, templatesCmd, localUpdateCmd)
	rootCmd.AddCommand(peersCmd)
	rootCmd.AddCommand(emergencyStopCmd)
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
