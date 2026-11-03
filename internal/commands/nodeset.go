package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/nodev1"
)

var (
	nodeSetCPUs  float64
	nodeSetMemGB float64
	nodeSetSlots int
)

// nodeSetCmd changes a running Mesh v1 node's capacity without a
// restart (ADR-044 amendment, 2026-11-03, "mesh node set signals the
// running node"): it rewrites node.yaml's own v1.capacity block —
// never through the legacy internal/node.Config, whose Save marshals
// its own typed struct and would drop the v1: key entirely — and, with
// a v1 node running on this machine, signals it (SIGHUP) to reread it
// at once; its scheduler's room reflects the new total within that
// signal, no restart. A node.yaml field this command sets from then on
// overrides the matching mesh node run flag at every future start
// ("flags become defaults"): the flag is only the default for a field
// node.yaml leaves unset.
var nodeSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Change a Mesh v1 node's capacity without restarting it",
	Long: `Rewrites node.yaml's v1.capacity and, with a Mesh v1 node (mesh node run
--api v1) running on this machine, signals it to reread it at once: its
room reflects the change within the signal, no restart. A flag not passed
here leaves that field as node.yaml already has it.

Once node.yaml names a field this way, mesh node run's matching flag is
only its default from then on: node.yaml wins at every future start too.

  mesh node set --cpus 64
  mesh node set --mem-gb 128 --slots 4`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		changedCPUs := cmd.Flags().Changed("cpus")
		changedMemGB := cmd.Flags().Changed("mem-gb")
		changedSlots := cmd.Flags().Changed("slots")
		if !changedCPUs && !changedMemGB && !changedSlots {
			return fmt.Errorf("mesh node set: pass at least one of --cpus, --mem-gb, --slots")
		}
		path, err := node.ConfigPath()
		if err != nil {
			return err
		}
		cur, _, err := nodev1.ReadCapacityConfig(path)
		if err != nil {
			return fmt.Errorf("mesh node set: reading node.yaml: %w", err)
		}
		if changedCPUs {
			cur.CPUs = nodeSetCPUs
		}
		if changedMemGB {
			cur.MemGB = nodeSetMemGB
		}
		if changedSlots {
			cur.Slots = nodeSetSlots
		}
		if err := nodev1.SetCapacityConfig(path, cur); err != nil {
			return fmt.Errorf("mesh node set: node.yaml: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "node.yaml: v1.capacity cpus=%g mem_gb=%g slots=%d\n", cur.CPUs, cur.MemGB, cur.Slots)
		state, err := v1RunState()
		if err != nil {
			return err
		}
		pid, ok := liveNode(state)
		if !ok {
			fmt.Fprintln(cmd.OutOrStdout(), "no Mesh v1 node is running on this machine; the change takes effect at its next start")
			return nil
		}
		if err := signalRestart(pid); err != nil {
			return fmt.Errorf("mesh node set: signal node (pid %d): %w", pid, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Mesh v1 node (pid %d) signalled: it rereads node.yaml's capacity now.\n", pid)
		return nil
	},
}

func init() {
	f := nodeSetCmd.Flags()
	f.Float64Var(&nodeSetCPUs, "cpus", 0, "CPUs this node offers")
	f.Float64Var(&nodeSetMemGB, "mem-gb", 0, "memory this node offers, in GB")
	f.IntVar(&nodeSetSlots, "slots", 0, "sessions at once; 0 is no slot limit")
	nodeCmd.AddCommand(nodeSetCmd)
}
