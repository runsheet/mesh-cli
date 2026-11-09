package commands

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/nodev1"
)

var leaseEgressOpts struct {
	Control string
	Node    string
	Lane    string
}

// leaseEgressCmd is a lease's egress, as a process of its own
// (mesh-specs adr-051): the node starts it outside the sandbox pod, in
// its own process group, as the node's own user. It owns the lease's
// CA and tunnel and serves the CONNECT route of both the node socket
// (the sidecar's) and the lane socket; the node drives it over the
// control socket (nodev1.LeaseEgressServer).
var leaseEgressCmd = &cobra.Command{
	Use:          "lease-egress --control <socket> --node <socket>",
	Short:        "A lease's egress, as a process of its own",
	Hidden:       true,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if leaseEgressOpts.Control == "" || leaseEgressOpts.Node == "" {
			return fmt.Errorf("lease-egress: --control and --node are required")
		}
		// Its node's exit closes the read end of this process's own
		// stderr (a pipe, cmd.StderrPipe() in startEgressProcess); the
		// next logf write after that is a write to fd 2, whose default
		// disposition -- per os/signal's SIGPIPE doc -- is to raise
		// SIGPIPE and kill the process outright, before stopEgress ever
		// gets to use the line, across exactly the restart ADR-051
		// exists to survive. A write to the lane or node socket, by
		// contrast, already fails with an ordinary EPIPE and no signal;
		// this Ignore is for stderr alone.
		signal.Ignore(syscall.SIGPIPE)
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		logf := func(format string, a ...any) { fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...) }

		tunnel := &egress.Tunnel{Logf: logf}
		if addr := os.Getenv(nodev1.TestEgressDialEnv); addr != "" {
			dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			tunnel.Dial = dial
			if serverName := os.Getenv(nodev1.TestEgressUpstreamServerNameEnv); serverName != "" {
				cfg := &tls.Config{ServerName: serverName}
				if ca := os.Getenv(nodev1.TestEgressUpstreamCAEnv); ca != "" {
					pool := x509.NewCertPool()
					if !pool.AppendCertsFromPEM([]byte(ca)) {
						return fmt.Errorf("lease-egress: %s: not a PEM certificate", nodev1.TestEgressUpstreamCAEnv)
					}
					cfg.RootCAs = pool
				}
				tunnel.Upstream = &http.Transport{DialContext: dial, TLSClientConfig: cfg}
			}
		}
		srv := nodev1.NewLeaseEgressServer(tunnel, logf)

		nodeClose, err := nodev1.ServeTunnelSocket(tunnel, leaseEgressOpts.Node)
		if err != nil {
			return fmt.Errorf("lease-egress: node socket: %w", err)
		}
		defer nodeClose.Close()

		if leaseEgressOpts.Lane != "" {
			laneClose, err := nodev1.ServeTunnelSocket(tunnel, leaseEgressOpts.Lane)
			if err != nil {
				return fmt.Errorf("lease-egress: lane socket: %w", err)
			}
			defer laneClose.Close()
		}

		return srv.ServeControl(ctx, leaseEgressOpts.Control)
	},
}

func init() {
	f := leaseEgressCmd.Flags()
	f.StringVar(&leaseEgressOpts.Control, "control", "", "the lease's own control socket, where the node drives this process")
	f.StringVar(&leaseEgressOpts.Node, "node", "", "the lease's node socket, where its session's CONNECT lands")
	f.StringVar(&leaseEgressOpts.Lane, "lane", "", "the lease's lane socket, where a run_suite lane's CONNECT lands")
	rootCmd.AddCommand(leaseEgressCmd)
}
