package commands

import (
	"errors"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/validate/docscheck"
)

// errDocsCheckFailed is a docs check that found problems: its report is
// already printed, so the command says nothing more and exits 1.
var errDocsCheckFailed = errors.New("the docs check found problems")

// docsCheckCmd is the node's built-in docs check (docscheck), the
// command of the docs suite a repository that holds only documents is
// validated by (ADR-053 amendment 2026-11-16): the node runs it in
// process when a suite's command is exactly `mesh docs-check`, and this
// runs the same check anywhere mesh is installed.
var docsCheckCmd = &cobra.Command{
	Use:   "docs-check [dir]",
	Short: "Check a repository's Markdown: frontmatter is YAML, code fences close, and links inside the repository resolve",
	Long: "Check every Markdown file under dir (the current directory by default), outside its dot-paths: valid UTF-8, " +
		"frontmatter that closes and is a YAML mapping, code fences that close, and relative links and images that name " +
		"a file or directory inside the repository. Prints one line per problem and a summary; exits 1 when there are problems.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) == 1 {
			dir = args[0]
		}
		exit, err := docscheck.Run(dir, cmd.OutOrStdout())
		if err != nil {
			return err
		}
		if exit != 0 {
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			return errDocsCheckFailed
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(docsCheckCmd)
}
