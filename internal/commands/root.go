package commands

import (
	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/version"
)

var rootCmd = &cobra.Command{
	Use:     "mesh",
	Short:   "Mesh CLI — AI, orgs, projects, workflows, tasks",
	Long:    "Mesh CLI is the operator tool for the Mesh control plane. Sign in once, then manage orgs, projects, agents (AI), workflows, and tasks from your saved Mesh session.",
	Version: version.Version,
	Example: "mesh auth login\nmesh workflow create --name \"Lead enrichment\" --definition ./graph.json\nmesh task create \"Investigate deploy flake\" --workflow-id <uuid>\nmesh task list --status pending",
}

func init() {
	rootCmd.AddCommand(authCmd, orgCmd, nodeCmd, agentCmd, sessionCmd)
	rootCmd.AddCommand(researchCmd, testCmd)
	rootCmd.AddCommand(runCmd, psCmd, attachCmd, logsCmd, restartCmd, rmCmd, inspectCmd)
	rootCmd.AddCommand(pullCmd, imagesCmd, templatesCmd, localUpdateCmd)
	rootCmd.AddCommand(peersCmd)
	rootCmd.AddCommand(emergencyStopCmd)
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
