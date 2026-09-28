package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/harness"
)

var (
	toolTrimMaxKB int
	toolTrimHook  bool
)

// toolTrimCmd bounds a tool result (trd §3 Tool results): a session's
// Bash command is piped through it by the node's PreToolUse hook, which
// is this command again with --hook.
var toolTrimCmd = &cobra.Command{
	Use:    "tool-trim",
	Short:  "Trim stdin to head and tail under --max-kb (a node session's Bash output); --hook is the PreToolUse hook that pipes Bash through it",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		in, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if toolTrimHook {
			// a hook that cannot answer lets the call run as it is
			res, ok, err := harness.TrimHook(in, meshBinary(), toolTrimMaxKB)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "mesh tool-trim:", err)
				return nil
			}
			if ok {
				_, err = out.Write(res)
			}
			return err
		}
		_, err = io.WriteString(out, harness.TrimToolResult(string(in), toolTrimMaxKB*1024))
		return err
	},
}

// meshBinary is this executable's resolved path, for a command a session
// runs (the tool-trim hook); "mesh" when it cannot be found.
func meshBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "mesh"
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return exe
}

func init() {
	toolTrimCmd.Flags().IntVar(&toolTrimMaxKB, "max-kb", 16, "the cap in KB; 0 passes everything through")
	toolTrimCmd.Flags().BoolVar(&toolTrimHook, "hook", false, "answer a Claude Code PreToolUse hook: pipe the Bash command through tool-trim")
	rootCmd.AddCommand(toolTrimCmd)
}
