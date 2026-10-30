package relaysidecar

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Command is the mesh subcommand the sidecar container runs.
const Command = "relay-sidecar"

// DefaultPort is the sidecar's loopback port in the pod: the git
// bridge's, which the checkout's insteadOf and the harness's base URL
// already name.
const DefaultPort = 47419

// credentialsWait bounds how long the sidecar waits for the node to
// install its credentials.
const credentialsWait = 2 * time.Minute

// Options are the sidecar container's.
type Options struct {
	// CredentialsDir is the sidecar's tmpfs, where the node installs
	// CredentialsFile.
	CredentialsDir string
	// NodeSocket is the lease's socket to the node.
	NodeSocket string
	// ControlSocket is where the sidecar serves the push tool.
	ControlSocket string
	// Listen is the loopback address the session reaches it at.
	Listen string
	// Logf takes its lines; never a credential.
	Logf func(format string, args ...any)
	// Ready, when set, hears the address served once it serves.
	Ready func(addr string)
}

// Run waits for the credentials, then serves until ctx ends.
func Run(ctx context.Context, o Options) error {
	if o.Listen == "" {
		o.Listen = fmt.Sprintf("127.0.0.1:%d", DefaultPort)
	}
	if host, _, err := net.SplitHostPort(o.Listen); err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("relaysidecar: %q is not a loopback address", o.Listen)
	}
	path := filepath.Join(o.CredentialsDir, CredentialsFile)
	creds, err := waitCredentials(ctx, path)
	if err != nil {
		return err
	}
	s, err := New(creds, o.NodeSocket)
	if err != nil {
		return err
	}
	s.Logf = o.Logf
	defer s.Close()
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	if o.ControlSocket != "" {
		if err := s.ServeControl(o.ControlSocket); err != nil {
			ln.Close()
			return err
		}
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln) }()
	if o.Ready != nil {
		o.Ready(ln.Addr().String())
	}
	if o.Logf != nil {
		o.Logf("relaysidecar: lease %s: serving on %s", creds.Lease, ln.Addr())
	}
	select {
	case <-ctx.Done():
		s.Close()
		<-errc
		return nil
	case err := <-errc:
		return err
	}
}

// waitCredentials loads the credentials once the node has installed
// them.
func waitCredentials(ctx context.Context, path string) (Credentials, error) {
	deadline := time.Now().Add(credentialsWait)
	for {
		c, err := Load(path)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return Credentials{}, err
		}
		if time.Now().After(deadline) {
			return Credentials{}, fmt.Errorf("relaysidecar: no credentials at %s after %s", path, credentialsWait)
		}
		select {
		case <-ctx.Done():
			return Credentials{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
