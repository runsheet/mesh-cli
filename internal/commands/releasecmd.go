package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/release"
)

// releaseVersion is a release's version: vMAJOR.MINOR.PATCH, with an
// optional pre-release.
var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

var releaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Build a mesh-cli release for runsheet to sign and publish",
	Long: `Commands for shipping mesh-cli through runsheet (ADR 002 amendment
2026-11-15). mesh-cli's .runsheet/deploy.yml runs them in a runsheet deploy
job: build, have the node sign SHA256SUMS (the deploy kind's sign_release
node tool), then upload every asset to runsheet's release store.`,
}

var releaseBuildCmd = &cobra.Command{
	Use:   "build --version vX.Y.Z --sha <commit> [--out dist]",
	Short: "Build the release binaries, mesh-version and SHA256SUMS into a directory",
	Long: `Builds mesh-linux-amd64, mesh-linux-arm64 and mesh-darwin-arm64 from this
checkout with the version and commit stamped in, writes mesh-version (the
version alone, which the signed sums must name), and writes SHA256SUMS over
all four. It signs and uploads nothing.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		version, _ := cmd.Flags().GetString("version")
		sha, _ := cmd.Flags().GetString("sha")
		out, _ := cmd.Flags().GetString("out")
		if !releaseVersion.MatchString(version) {
			return fmt.Errorf("--version %q is not vMAJOR.MINOR.PATCH", version)
		}
		if len(sha) < 7 {
			return fmt.Errorf("--sha is the commit being released (at least 7 characters)")
		}
		if err := os.MkdirAll(out, 0o755); err != nil {
			return err
		}
		for _, inv := range release.BuildPlan(version, sha, out) {
			c := exec.CommandContext(cmd.Context(), inv.Args[0], inv.Args[1:]...)
			c.Env = append(os.Environ(), inv.Env...)
			c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := c.Run(); err != nil {
				return fmt.Errorf("%v: %w", inv.Args, err)
			}
		}
		if err := release.WriteVersion(out, version); err != nil {
			return err
		}
		sums, err := release.WriteSums(out, release.Artefacts())
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s written:\n%s", filepath.Join(out, release.SumsName), sums)
		return nil
	},
}

func init() {
	releaseBuildCmd.Flags().String("version", "", "the version being released, vMAJOR.MINOR.PATCH")
	releaseBuildCmd.Flags().String("sha", "", "the commit being released")
	releaseBuildCmd.Flags().String("out", "dist", "the directory the assets are written to")
	releaseCmd.AddCommand(releaseBuildCmd)
	rootCmd.AddCommand(releaseCmd)
}
