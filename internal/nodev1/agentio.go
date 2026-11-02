package nodev1

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// agentStdinName and agentStdoutName are the agent's real stdio files
// under a lease's own directory (leasePod.dir): a named pipe for its
// stdin, a plain file for its stdout.
const (
	agentStdinName  = "agent.stdin"
	agentStdoutName = "agent.stdout"
)

// runWithAgentIO runs agent with its real stdin and stdout backed by
// files in dir — a named pipe for stdin, a plain file for stdout — in
// place of the harness's own in-memory pipes (adr-044's closing
// paragraph): the bytes the harness writes to stdin are copied into
// the pipe, and everything the agent writes to its real stdout is
// read back by tailing the file from offset 0 into the harness's
// stdout, so the harness sees the same stream it always has, just
// carried over a file instead of a pipe that dies with the node.
// Adopting the session itself across a restart — reloading its
// credentials and the lease's state from disk — is staged separately;
// this only moves the bytes already in flight onto disk.
func runWithAgentIO(ctx context.Context, dir string, stdin io.Reader, stdout io.Writer, agent func(stdin, stdout *os.File) error) error {
	stdinPath := filepath.Join(dir, agentStdinName)
	stdoutPath := filepath.Join(dir, agentStdoutName)

	os.Remove(stdinPath) // a stale pipe from an earlier run of this lease
	if err := unix.Mkfifo(stdinPath, 0o600); err != nil {
		return fmt.Errorf("the agent's stdin pipe: %w", err)
	}
	// opened O_RDWR first so the pipe always has a writer: the
	// O_RDONLY open below (the agent's real stdin) would otherwise
	// block until one exists. A single O_RDWR fd used for both ends
	// would never see EOF (it is its own writer); the separate
	// O_RDONLY fd handed to the agent does, once w is closed below.
	w, err := os.OpenFile(stdinPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("the agent's stdin pipe: %w", err)
	}
	r, err := os.OpenFile(stdinPath, os.O_RDONLY, 0)
	if err != nil {
		w.Close()
		return fmt.Errorf("the agent's stdin pipe: %w", err)
	}
	defer r.Close()

	out, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		w.Close()
		return fmt.Errorf("the agent's stdout file: %w", err)
	}
	defer out.Close()

	copied := make(chan struct{})
	go func() {
		defer close(copied)
		io.Copy(w, stdin)
		w.Close()
	}()
	go func() {
		select {
		case <-ctx.Done():
			w.Close()
		case <-copied:
		}
	}()

	var running atomic.Bool
	running.Store(true)
	fl, err := sessionunit.Follow(ctx, stdoutPath, 0, running.Load)
	if err != nil {
		w.Close()
		return err
	}
	defer fl.Close()
	tailed := make(chan struct{})
	go func() {
		defer close(tailed)
		io.Copy(stdout, fl)
	}()

	runErr := agent(r, out)
	running.Store(false)
	<-tailed
	return runErr
}
