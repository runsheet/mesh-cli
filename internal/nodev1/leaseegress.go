package nodev1

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// errEgressDied is the lease's egress process (ADR-051 decision 1)
// ending on its own, never stopped by the actor: its own process group,
// outside the node's and the pod's, so nothing but its own exit or a
// signal from outside the node ends it this way.
var errEgressDied = errors.New("the lease's egress process died")

// egressDiedError is errEgressDied with the exit code the process was
// killed with, 0 when it simply exited.
type egressDiedError struct {
	code int
	why  string
}

func (e *egressDiedError) Error() string { return e.why }
func (e *egressDiedError) Unwrap() error { return errEgressDied }

// egressDied is whether o's cause is the lease's egress process dying
// on its own (ADR-051 decision 3), and the hand-back reason for it.
func egressDied(o outcome) (string, bool) {
	var ee *egressDiedError
	if errors.As(o.cause, &ee) {
		return ee.Error(), true
	}
	if errors.Is(o.cause, errEgressDied) {
		return o.cause.Error(), true
	}
	return "", false
}

// egressAlive is whether a lease's egress process (ADR-051 decision 1),
// named by its pid from setEgress, is still running: its own process
// group, the same one stopLeftoverEgress signals. A pid of 0 or less (a
// lease with no out-of-process egress) is never alive.
func egressAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}

// stopLeftoverEgress stops a lease's egress process left running with
// no actor to own it: a node that stopped without releasing the lease
// (ADR-051 decision 5, from releaseLeftovers at start-up), or one a
// running node's supervisor finds no actor and no hand-off names
// (ADR-051 decision 3, from egressRuntime.Stop, the supervisor's
// Egress). Its own process group, since it never shares the
// node's or the pod's (ADR-051 decision 1). SIGTERM, a bounded wait,
// then SIGKILL; gone already (ESRCH) is not an error.
func stopLeftoverEgress(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("stop %d: %w", pid, err)
	}
	deadline := time.Now().Add(egressStopWait)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	deadline = time.Now().Add(egressStopWait)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("process group %d still there after SIGKILL", pid)
}
