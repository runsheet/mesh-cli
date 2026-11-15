package nodev1

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// The lease's egress process exiting because its own lease is already
// ending — the lease's context cancelled with its own cause by the time
// the exit is seen, the two ready at once — is never an egress death:
// watchEgress raises nothing, and the lease reports the cause that
// really ended it (dl360, 2026-11-16: leases handed back egress_died
// seconds after they started running).
func TestAnEgressExitAfterItsLeaseEndedIsNotAnEgressDeath(t *testing.T) {
	why := errors.New("the lease ended some other way")
	for i := 0; i < 200; i++ {
		done := make(chan struct{})
		close(done)
		rs := &relaySidecar{egressCmd: &exec.Cmd{}, egressDone: done}
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(why)
		var raised error
		rs.watchEgress(ctx, func(err error) { raised = err })
		if raised != nil {
			t.Fatalf("try %d: watchEgress raised %v after the lease had ended (%v)", i, raised, context.Cause(ctx))
		}
	}

	// a genuine death — the lease still live — is still raised, and
	// reported with a cause Mesh takes
	done := make(chan struct{})
	close(done)
	rs := &relaySidecar{egressCmd: &exec.Cmd{}, egressDone: done}
	var raised error
	rs.watchEgress(context.Background(), func(err error) { raised = err })
	if !errors.Is(raised, errEgressDied) {
		t.Fatalf("a live lease's egress exit raised %v, want an egress death", raised)
	}
	if _, ok := egressDied(outcome{cause: raised}); !ok {
		t.Fatal("the egress death is not read as one")
	}
}
