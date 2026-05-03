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
	Example: "mesh auth login\nmesh work-item create \"Stabilize release flow\" project <project-id>\nmesh work-item update <id> ready\nmesh work-item handoff <id> pm devlead \"Ready for review\"\nmesh work-item review <id> devlead approve \"Ship it\"\nmesh work-item learning <id> \"Release flow now stable\"",
}

func init() {
	rootCmd.AddCommand(authCmd, workItemCmd, projectCmd, orgCmd, nodeCmd, agentCmd, sessionCmd)
	rootCmd.AddCommand(researchCmd, testCmd)
	rootCmd.AddCommand(workflowCmd)
	rootCmd.AddCommand(runCmd, psCmd, attachCmd, logsCmd, restartCmd, rmCmd, inspectCmd)
	rootCmd.AddCommand(pullCmd, imagesCmd, templatesCmd, localUpdateCmd)
	rootCmd.AddCommand(channelServerCmd)
	rootCmd.AddCommand(discordCmd)
	rootCmd.AddCommand(whatsappCmd)
	rootCmd.AddCommand(peersCmd)
	rootCmd.AddCommand(emergencyStopCmd)
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
