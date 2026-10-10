package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/meshmcp"
	"projectmesh.io/mesh-cli/internal/node"
)

// mcpCmd is `mesh mcp`: Mesh inside Claude Code, added once with
// `claude mcp add mesh -- mesh mcp` (ADR-023, mesh mcp). It speaks MCP
// on stdin/stdout on the credential in ~/.mesh/token — the person's
// Latchkey login or an rs_* token, refreshed under the lock — and
// writes nothing else to stdout.
var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve Mesh's tools to Claude Code: a stdio MCP server on your own login",
	Long: "mesh mcp is a stdio MCP server for Claude Code: add it once with\n\n" +
		"  claude mcp add mesh -- mesh mcp\n\n" +
		"and Claude Code gets mesh_goals, mesh_status, mesh_join, mesh_switch, mesh_progress,\n" +
		"mesh_ask, mesh_finish and mesh_release on the credential in ~/.mesh/token.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		gw, err := platformClient()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "mesh mcp: "+format+"\n", a...) }
		// the platform's /mcp is asked for its work tools at the start, and
		// again hourly as the tools are used (ADR-023, Hosted path)
		hosted := meshmcp.NewHosted(gw)
		go hosted.Serves(ctx)
		// the personal node is the person's own, on their own login, never
		// a machine's: it registers nothing through registrar and so
		// sends no credentialId (runsheet#2290 asks it of a machine alone)
		p := &meshmcp.Personal{GW: gw, Join: &meshmcp.Join{
			GW: gw, WS: &node.Git{Log: os.Stderr}, NodeID: mcpNodeID,
			Fallback: &node.TokenFallback{}, Hosted: hosted, Logf: logf,
		}}
		return p.Server().Serve(ctx, os.Stdin, os.Stdout)
	},
}

// mcpNodeID is the node mesh_join claims for: node.yaml's node id until
// the personal node's registration (the goal's item 2) answers its own.
func mcpNodeID(context.Context) (string, error) {
	cfg, err := node.LoadConfig()
	if err != nil {
		return "", err
	}
	if cfg.NodeID == "" {
		return "", errors.New("no node registered on this machine yet")
	}
	return cfg.NodeID, nil
}

var mcpInstallForce bool

// mcpInstallCmd is `mesh mcp install`: writes the embedded `/mesh` skill
// (skills/mesh/SKILL.md, meshmcp.Skill) to Claude Code's skills
// directory (ADR-023, Skill) — $CLAUDE_CONFIG_DIR/skills/mesh/SKILL.md
// when set, else ~/.claude/skills/mesh/SKILL.md — refusing to overwrite
// a file that differs unless --force.
var mcpInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Write the /mesh skill to Claude Code's skills directory",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := skillInstallPath()
		if err != nil {
			return err
		}
		want := meshmcp.Skill()
		if existing, err := os.ReadFile(path); err == nil {
			if bytes.Equal(existing, want) {
				fmt.Println(path, "is already up to date")
			} else if !mcpInstallForce {
				return fmt.Errorf("%s already exists and differs from the skill mesh-cli ships; pass --force to overwrite it", path)
			} else {
				if err := node.WriteFile(path, want); err != nil {
					return err
				}
				fmt.Println("overwrote", path)
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := node.WriteFile(path, want); err != nil {
				return err
			}
			fmt.Println("wrote", path)
		} else {
			return err
		}
		fmt.Println("\nAdd mesh mcp to Claude Code with:\n\n  claude mcp add mesh -- mesh mcp")
		return nil
	},
}

// skillInstallPath is $CLAUDE_CONFIG_DIR/skills/mesh/SKILL.md, else
// ~/.claude/skills/mesh/SKILL.md.
func skillInstallPath() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "skills", "mesh", "SKILL.md"), nil
}

func init() {
	mcpInstallCmd.Flags().BoolVar(&mcpInstallForce, "force", false, "overwrite a skill file that differs from the one mesh-cli ships")
	mcpCmd.AddCommand(mcpInstallCmd)
}
