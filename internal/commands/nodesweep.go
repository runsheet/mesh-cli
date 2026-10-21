package commands

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

var nodeSweepDryRun bool

// nodeSweepCmd is one pass of the work sweep by hand (mesh-cli ADR 008,
// The work sweep, over ADR-039's jobs): each job judged by the
// platform's answer for its item, goal or batch, its live set every
// session in the jobs tree still alive. --dry-run prints the plan and
// removes nothing.
var nodeSweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Remove what ~/.mesh/jobs no longer needs: finished items' and plans' jobs, old batches, old kept sessions",
	Long: `Runs one pass of the work sweep the node runs at start-up and hourly: an
item's job once the item is merged, cancelled or superseded, or failed for over
a week; a plan's job once its goal is not planning; a merge batch's or main
validation's job six hours on; kept sessions past 14 days. Nothing a live
session runs in, nothing the platform cannot answer for, no symlink. Prints one
line per removal and a total; --dry-run prints the same lines and removes
nothing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := node.LoadConfig(); err != nil {
			return err
		}
		src, err := tokenSource()
		if err != nil {
			return err
		}
		gw := gateway.NewWithSource(gatewayURL(), src)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		l, err := node.DefaultLayout()
		if err != nil {
			return err
		}
		live, err := node.AliveSessionSubjects(l.JobsDir(), nil)
		if err != nil {
			return err
		}
		// why an entry is left goes to stderr; what goes, to stdout
		judged := log.New(cmd.ErrOrStderr(), "", 0).Printf
		return workSweepPass(ctx, gw, l, nil, node.LiveSessions{IDs: live}, judged, cmd.OutOrStdout(), nodeSweepDryRun)
	},
}

// workSweepPass plans one pass of the work sweep and, unless dry, applies
// it: the plan's lines (one per removal, then the total) to out — a dry
// run's exactly those Apply would log — and the judging to logf.
// connections, when known, are the node's connections; nil judges no job
// by its connection.
func workSweepPass(ctx context.Context, gw node.Gateway, l *node.Layout, connections map[string]bool, live node.LiveSessions, logf func(string, ...any), out io.Writer, dry bool) error {
	plan, err := node.PlanJobSweep(ctx, gw, l, connections, live, time.Now(), logf)
	if err != nil {
		return fmt.Errorf("sweep %s: %w", l.JobsDir(), err)
	}
	if dry {
		fmt.Fprint(out, plan.String())
		fmt.Fprintln(out, "sweep: dry run; nothing removed")
		return nil
	}
	node.ApplyWorkSweep(plan, func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) })
	return nil
}

// nodeWorkSweep is the node's own pass (`mesh node start`, at start-up
// after the scope sweep and then hourly): judged and applied, every line
// through logf. A job whose connection is not among the scopes' goes,
// once every scope names its connection (node.ConnectionsOf).
func nodeWorkSweep(ctx context.Context, gw node.Gateway, l *node.Layout, scopes node.Scopes, ids map[string]bool, sb *sandbox.Sandbox, logf func(string, ...any)) {
	plan, err := node.PlanJobSweep(ctx, gw, l, node.ConnectionsOf(scopes), node.LiveSessions{IDs: ids, Sandbox: sb}, time.Now(), logf)
	if err != nil {
		logf("sweep %s: %v", l.JobsDir(), err)
		return
	}
	if len(plan.Removals) == 0 {
		return
	}
	node.ApplyWorkSweep(plan, logf)
}

func init() {
	nodeSweepCmd.Flags().BoolVar(&nodeSweepDryRun, "dry-run", false, "print what a pass would remove and remove nothing")
	nodeSweepCmd.Flags().StringVar(&gatewayFlag, "gateway", "", "platform gateway URL (default "+gateway.DefaultURL+", or MESH_GATEWAY)")
	nodeCmd.AddCommand(nodeSweepCmd)
}
