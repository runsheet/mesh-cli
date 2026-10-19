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
// The work sweep): the plan judged by the platform's answer for every
// org the credential belongs to, its live set every session under the
// work root still alive. --dry-run prints the plan and removes nothing.
var nodeSweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Remove what ~/.mesh/work no longer needs: finished items' checkouts, finished goals' directories, old kept sessions",
	Long: `Runs one pass of the work sweep the node runs at start-up and hourly: an
item's checkout, session dir and attestation copy once the item is merged,
cancelled or superseded, or failed for over a week; a done or cancelled goal's
whole directory; a goal's plan/ once it is not planning; kept sessions past 14
days. Nothing a live session runs in, nothing the platform cannot answer for,
no symlink. Prints one line per removal and a total; --dry-run prints the same
lines and removes nothing.`,
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
		orgs, err := gw.MyOrgs(ctx)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(orgs))
		for _, o := range orgs {
			ids = append(ids, o.ID)
		}
		root, err := node.WorkRoot()
		if err != nil {
			return err
		}
		live, err := node.AliveSessionSubjects(root, nil)
		if err != nil {
			return err
		}
		// why an entry is left goes to stderr; what goes, to stdout
		judged := log.New(cmd.ErrOrStderr(), "", 0).Printf
		return workSweepPass(ctx, gw, root, ids, node.LiveSessions{IDs: live}, judged, cmd.OutOrStdout(), nodeSweepDryRun)
	},
}

// workSweepPass plans one pass of the work sweep and, unless dry, applies
// it: the plan's lines (one per removal, then the total) to out — a dry
// run's exactly those Apply would log — and the judging to logf.
func workSweepPass(ctx context.Context, gw node.Gateway, root string, orgIDs []string, live node.LiveSessions, logf func(string, ...any), out io.Writer, dry bool) error {
	plan, err := node.PlanWorkSweep(ctx, gw, root, orgIDs, live, time.Now(), logf)
	if err != nil {
		return fmt.Errorf("sweep %s: %w", root, err)
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
// through logf.
func nodeWorkSweep(ctx context.Context, gw node.Gateway, root string, scopes node.Scopes, ids map[string]bool, keep []string, sb *sandbox.Sandbox, logf func(string, ...any)) {
	orgIDs := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		orgIDs = append(orgIDs, sc.ID)
	}
	plan, err := node.PlanWorkSweep(ctx, gw, root, orgIDs, node.LiveSessions{IDs: ids, Keep: keep, Sandbox: sb}, time.Now(), logf)
	if err != nil {
		logf("sweep %s: %v", root, err)
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
