package commands

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/node"
)

var (
	nodeUsageLog  string
	nodeUsageFrom string
	nodeUsageTo   string
	nodeUsageGoal string
	nodeUsageKind string
)

// nodeUsageCmd is the node-side reading of what sessions cost
// (adr-016's amendment): node.ReadUsage over node.log's usage records,
// printed as node.UsageReading.Lines. The platform exposes no usage
// query, so this is the reading a goal doc quotes.
var nodeUsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Read node.log's usage records: sessions, input tokens, cache reads and context bytes by kind, for a window",
	Long: `Reads every session's usage record in node.log — ` + "`<label>: usage <kind> {…}`" + `,
and the unlabelled ` + "`usage: {…}`" + ` an older node wrote, paired by cost to the
session's end line — logged in [--from, --to), and prints one row per kind:
sessions, median and mean input tokens, the cache-read share, and the median
context bytes over the sessions that carry them. Dates are local, as
node.log's timestamps are.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		from, err := time.ParseInLocation("2006-01-02", nodeUsageFrom, time.Local)
		if err != nil {
			return fmt.Errorf("--from: want YYYY-MM-DD: %w", err)
		}
		to, err := time.ParseInLocation("2006-01-02", nodeUsageTo, time.Local)
		if err != nil {
			return fmt.Errorf("--to: want YYYY-MM-DD: %w", err)
		}
		if !to.After(from) {
			return fmt.Errorf("--to %s is not after --from %s", nodeUsageTo, nodeUsageFrom)
		}
		path := nodeUsageLog
		paths := []string{path}
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			// node.log and what rotation kept of it, oldest first
			path = node.ServiceLog(home)
			if paths = node.LogFiles(path, node.NodeLogKeep); len(paths) == 0 {
				paths = []string{path}
			}
		}
		var readers []io.Reader
		for _, p := range paths {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			readers = append(readers, f)
		}
		u, err := node.ReadUsage(io.MultiReader(readers...), node.UsageQuery{From: from, To: to, Goal: nodeUsageGoal, Kind: nodeUsageKind})
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, l := range u.Lines() {
			fmt.Fprintln(cmd.OutOrStdout(), l)
		}
		return nil
	},
}

func init() {
	nodeUsageCmd.Flags().StringVar(&nodeUsageLog, "log", "", "the node log to read (default ~/.mesh/node.log and its rotated node.log.1 … node.log.5)")
	nodeUsageCmd.Flags().StringVar(&nodeUsageFrom, "from", "", "the window's first day, YYYY-MM-DD (required)")
	nodeUsageCmd.Flags().StringVar(&nodeUsageTo, "to", "", "the day after the window's last, YYYY-MM-DD, exclusive (required)")
	nodeUsageCmd.Flags().StringVar(&nodeUsageGoal, "goal", "", "only records whose label names this goal's slug (its items' and its plan's)")
	nodeUsageCmd.Flags().StringVar(&nodeUsageKind, "kind", "", "only records of this kind: build, evaluate, rebase, plan, research or unknown")
	_ = nodeUsageCmd.MarkFlagRequired("from")
	_ = nodeUsageCmd.MarkFlagRequired("to")
	nodeCmd.AddCommand(nodeUsageCmd)
}
