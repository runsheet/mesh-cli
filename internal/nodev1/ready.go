package nodev1

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// socketReadyWait bounds how long a lease waits for its sessionproxy's
// socket to answer before its session starts.
const socketReadyWait = 10 * time.Second

// waitListening returns once a dial of the unix socket at sock succeeds:
// the lease's sessionproxy — and the node tools it serves in process —
// answers before the session is started or sent its first message, so
// the session's first MCP call never meets a socket that is not there
// yet (Mesh v1 spec, "The git relay and the Latchkey token relay": the
// relay opens in the actor's preparing state, before running). It
// gives up after wait, or when ctx ends.
func waitListening(ctx context.Context, sock string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var d net.Dialer
	backoff := 5 * time.Millisecond
	for {
		c, err := d.DialContext(ctx, "unix", sock)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("the lease's socket %s did not answer within %s: %w", sock, wait, err)
			}
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 200*time.Millisecond)
	}
}
