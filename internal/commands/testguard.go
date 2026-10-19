package commands

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/harness"
)

var testGuardHook bool

// testGuardCmd is the PreToolUse hook that refuses a whole-suite or
// backgrounded test run (trd §3): a session's `.claude/settings.json`
// pipes every Bash call through `mesh test-guard --hook`, which answers
// harness.TestGuardHook on the hook's own JSON, the cwd as the checkout
// and the files changed since the merge base in it.
var testGuardCmd = &cobra.Command{
	Use:    "test-guard",
	Short:  "Deny a whole-suite or backgrounded Bash test run in a PreToolUse hook, naming the suites the change touched",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !testGuardHook {
			return fmt.Errorf("mesh test-guard: only --hook is supported")
		}
		in, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return err
		}
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		out, ok, err := harness.TestGuardHook(in, dir, testGuardChangedPaths(dir))
		if err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "mesh test-guard:", err)
			return nil
		}
		if ok {
			_, err = cmd.OutOrStdout().Write(out)
		}
		return err
	},
}

// testGuardChangedPaths is `git diff --name-only <merge base>...HEAD` in
// dir: the files this checkout's branch changed against the branch it
// tracks, else against origin's default branch. Neither found, or the
// diff fails, is no changed paths — never an error, since the guard
// still answers on a generic reason.
func testGuardChangedPaths(dir string) []string {
	base := testGuardMergeBaseRef(dir)
	if base == "" {
		return nil
	}
	out, err := exec.Command("git", "-C", dir, "diff", "--name-only", base+"...HEAD").Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths
}

// testGuardMergeBaseRef is the ref to diff this checkout's HEAD
// against: the branch it tracks, else origin's default branch, else ""
// when dir is not a git checkout with either.
func testGuardMergeBaseRef(dir string) string {
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}").Output(); err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			return ref
		}
	}
	if out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output(); err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			return ref
		}
	}
	return ""
}

func init() {
	testGuardCmd.Flags().BoolVar(&testGuardHook, "hook", false, "answer a Claude Code PreToolUse hook: deny a whole-suite or backgrounded test run")
	rootCmd.AddCommand(testGuardCmd)
}
