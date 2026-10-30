package commands

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/relaysidecar"
)

var relaySidecarOpts relaysidecar.Options

// relaySidecarCmd is a sandboxed lease's relay, run by the node as a
// sidecar container in the lease's pod (projectmesh-io/api ADR 001
// amendment "relay sidecar"): it waits for the lease's credentials in
// its tmpfs, then serves the session's routes on the pod's loopback.
var relaySidecarCmd = &cobra.Command{
	Use:          relaysidecar.Command + " --credentials-dir <dir> --node <socket>",
	Short:        "A sandboxed lease's relay, as a sidecar in its pod",
	Hidden:       true,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if relaySidecarOpts.CredentialsDir == "" || relaySidecarOpts.NodeSocket == "" {
			return fmt.Errorf("%s: --credentials-dir and --node are required", relaysidecar.Command)
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		relaySidecarOpts.Logf = func(format string, a ...any) { fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...) }
		return relaysidecar.Run(ctx, relaySidecarOpts)
	},
}

var relaySidecarInstallDir string

// relaySidecarInstallCmd writes the lease's credentials, read from
// stdin, into the sidecar's tmpfs, mode 0400: the node runs it with
// `podman exec -i` as the sidecar's own user, so the credentials are
// never on a command line or in an environment.
var relaySidecarInstallCmd = &cobra.Command{
	Use:          "install --dir <dir>",
	Short:        "Install a lease's credentials from stdin into the sidecar's tmpfs",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if relaySidecarInstallDir == "" {
			return fmt.Errorf("%s install: --dir is required", relaysidecar.Command)
		}
		return relaysidecar.Install(cmd.InOrStdin(), relaySidecarInstallDir)
	},
}

func init() {
	f := relaySidecarCmd.Flags()
	f.StringVar(&relaySidecarOpts.CredentialsDir, "credentials-dir", "", "the sidecar's tmpfs, where the node installs the lease's credentials")
	f.StringVar(&relaySidecarOpts.NodeSocket, "node", "", "the lease's socket to the node")
	f.StringVar(&relaySidecarOpts.ControlSocket, "control", "", "where to serve the push tool's control socket")
	f.StringVar(&relaySidecarOpts.Listen, "listen", "", "the loopback address to serve the session on (127.0.0.1:47419)")
	relaySidecarInstallCmd.Flags().StringVar(&relaySidecarInstallDir, "dir", "", "the sidecar's credential tmpfs")
	relaySidecarCmd.AddCommand(relaySidecarInstallCmd)
	rootCmd.AddCommand(relaySidecarCmd)
}
