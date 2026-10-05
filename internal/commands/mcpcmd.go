package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/mcpinstall"
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

var (
	mcpInstallForce   bool
	mcpInstallDryRun  bool
	mcpInstallClients []string
	mcpInstallRemote  bool
	mcpInstallURL     string
)

// mcpExecutable is the running mesh binary, the command every client's
// entry runs; tests see the test binary.
var mcpExecutable = os.Executable

// mcpInstallCmd is `mesh mcp install`: for each client of the install
// table (mcpinstall.Clients, ADR-025) — claude, codex, cursor — writes
// its MCP entry for `<this binary> mcp` into its own config file and
// format, and its instruction files rendered from the embedded `/mesh`
// skill (meshmcp.Skill; ADR-023, Skill). --client picks the clients
// (default: every one Detect finds), --dry-run prints each diff and
// writes nothing, --force overwrites an instruction file that differs.
// --remote writes the hosted server's entry instead — the gateway's
// /mcp, or --url — signed in by OAuth when its issuer offers dynamic
// registration, else a bearer read from $MESH_TOKEN, never a token in a
// file (ADR-025, Remote).
// A file it cannot parse, or one that differs without --force, is
// reported and left alone, the other clients still written, and the
// command ends with status 1.
var mcpInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Add mesh mcp and the /mesh instructions to Claude Code, Codex and Cursor",
	Long: "mesh mcp install writes each client's MCP server entry for `mesh mcp` and its\n" +
		"instructions: Claude Code's ~/.claude.json and /mesh skill, Codex's\n" +
		"$CODEX_HOME/config.toml, Cursor's ~/.cursor/mcp.json. Every other entry is left\n" +
		"byte-for-byte; re-running changes nothing.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		exe, err := mcpExecutable()
		if err != nil {
			return err
		}
		if exe, err = filepath.Abs(exe); err != nil {
			return err
		}
		all := mcpinstall.Clients(meshmcp.Skill)
		var clients []mcpinstall.Client
		if len(mcpInstallClients) > 0 {
			if clients, err = mcpinstall.Select(all, mcpInstallClients); err != nil {
				return err
			}
		} else if clients = mcpinstall.Detected(all, home); len(clients) == 0 {
			return errors.New("no client found; pass --client")
		}

		opts := mcpinstall.PlanOptions{Force: mcpInstallForce}
		if mcpInstallRemote {
			opts.Remote, opts.URL = true, strings.TrimSpace(mcpInstallURL)
			if opts.URL == "" {
				opts.URL = node.MeshMCPURL(gatewayURL())
			}
			// discovered once, for every client
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			opts.Auth = mcpinstall.DiscoverAuth(ctx, opts.URL)
			fmt.Fprintf(out, "url: %s\nauth: %s\n", opts.URL, opts.Auth.Describe())
		} else if mcpInstallURL != "" {
			return errors.New("--url needs --remote")
		}
		changes, planErr := mcpinstall.Plan(home, exe, clients, opts)
		if !mcpInstallDryRun {
			if err := mcpinstall.Apply(changes); err != nil {
				return err
			}
		}
		claudeWritten := false
		for _, c := range changes {
			if mcpInstallDryRun {
				fmt.Fprint(out, mcpinstall.Diff(c.Path, c.Before, c.After))
			}
			fmt.Fprintf(out, "%s: %s %s\n", c.Client, c.Status(mcpInstallDryRun), c.Path)
			claudeWritten = claudeWritten || (c.Client == "claude" && c.What == "mcp")
		}
		left := 0
		for _, err := range unjoin(planErr) {
			left++
			var u *mcpinstall.UnparseableError
			if errors.As(err, &u) {
				fmt.Fprintf(out, "%s: cannot parse (%v); left alone\n", u.Path, u.Err)
			} else {
				fmt.Fprintf(out, "%v; left alone\n", err)
			}
		}
		for _, c := range clients {
			if _, oauth := opts.Auth.(mcpinstall.OAuth); oauth && c.Name == "codex" {
				fmt.Fprintf(out, "codex: sign in with `codex mcp login %s`\n", mcpinstall.ServerName)
			}
			if c.Name == "claude" && !claudeWritten && mcpInstallRemote {
				fmt.Fprintf(out, "\nAdd mesh to Claude Code by hand with:\n\n  %s\n", claudeRemoteAdd(opts))
			} else if c.Name == "claude" && !claudeWritten {
				fmt.Fprintf(out, "\nAdd mesh mcp to Claude Code by hand with:\n\n  claude mcp add --scope user mesh -- %s mcp\n", exe)
			}
		}
		if left > 0 {
			cmd.SilenceUsage = true
			return fmt.Errorf("%d file(s) left alone", left)
		}
		return nil
	},
}

// claudeRemoteAdd is the `claude mcp add` line for the remote entry.
func claudeRemoteAdd(opts mcpinstall.PlanOptions) string {
	line := "claude mcp add --scope user --transport http mesh " + opts.URL
	if b, ok := opts.Auth.(mcpinstall.Bearer); ok {
		line += ` --header "Authorization: Bearer ${` + b.EnvVar + `}"`
	}
	return line
}

// unjoin is the errors errors.Join joined, or err alone.
func unjoin(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

func init() {
	mcpInstallCmd.Flags().BoolVar(&mcpInstallForce, "force", false, "overwrite an instruction file that differs from the one mesh-cli ships")
	mcpInstallCmd.Flags().BoolVar(&mcpInstallDryRun, "dry-run", false, "print every file and diff the install would write; write nothing")
	mcpInstallCmd.Flags().StringSliceVar(&mcpInstallClients, "client", nil, "claude, codex, cursor or all (repeatable or a comma list; default every client found)")
	mcpInstallCmd.Flags().BoolVar(&mcpInstallRemote, "remote", false, "write the hosted server's entry (no local binary): OAuth when the issuer registers clients, else a bearer from $"+mcpinstall.BearerEnvVar)
	mcpInstallCmd.Flags().StringVar(&mcpInstallURL, "url", "", "the MCP URL for --remote (default: the gateway's /mcp)")
	mcpCmd.AddCommand(mcpInstallCmd)
}
