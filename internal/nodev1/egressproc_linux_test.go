//go:build linux

package nodev1

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
)

// startTestEgress starts a lease's real `mesh lease-egress` subprocess
// the way the node's work loop does (relaypod.go's startEgressProcess),
// for a test to find and stop by its bare pid alone -- the way a node
// that just restarted, or the supervisor's reconcile, would (ADR-051
// decisions 3 and 5).
func startTestEgress(t *testing.T, lease string) *relaySidecar {
	t.Helper()
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("no /proc")
	}
	dir, err := os.MkdirTemp("", "egproc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	rs := &relaySidecar{r: &Runtime{Logf: t.Logf, MeshBinary: testMeshBinary(t)},
		b:    &meshapi.Brief{LeaseID: lease, Egress: &meshapi.Egress{Hosts: []string{"proxy.golang.org"}}},
		dir:  dir,
		node: &relaysidecar.Node{}}
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := key.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	creds := relaysidecar.Credentials{Key: handoff, Session: relaysidecar.Token{Value: lease + "-token"}}
	policy := egress.NewPolicy([]string{"proxy.golang.org"}, nil, nil)
	noop := func(egress.Call) {}
	if err := rs.startEgressProcess(context.Background(), policy, rs.b, creds, noop, noop); err != nil {
		t.Fatal(err)
	}
	return rs
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d is still alive after 5s", pid)
}

// isLeaseEgress knows the real subprocess by its argv, the one
// startEgressProcess gives it -- not this test binary's own pid, and
// not a pid nothing answers for.
func TestIsLeaseEgressMatchesTheRealProcessArgv(t *testing.T) {
	rs := startTestEgress(t, "lease-argv")
	t.Cleanup(func() { rs.close() })
	pid := rs.egressCmd.Process.Pid
	if !isLeaseEgress(pid) {
		t.Fatalf("isLeaseEgress(%d) is false for the real lease-egress process", pid)
	}
	if isLeaseEgress(os.Getpid()) {
		t.Fatal("isLeaseEgress(this test binary's own pid) is true")
	}
	if isLeaseEgress(999999999) {
		t.Fatal("isLeaseEgress(a pid that does not exist) is true")
	}
}

// egressRuntime.List finds a running lease-egress process by its argv
// alone -- no actor.json is consulted here, only /proc -- and Stop
// ends it the same way stopLeftoverEgress does, the path the
// supervisor's reconcile uses on an orphan no live lease names
// (ADR-051 decisions 3 and 5).
func TestEgressRuntimeListAndStop(t *testing.T) {
	rs := startTestEgress(t, "lease-list")
	pid := rs.egressCmd.Process.Pid
	t.Cleanup(func() { rs.close() })

	rt := egressRuntime{}
	procs, err := rt.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range procs {
		if p.PID == pid {
			found = true
		}
	}
	if !found {
		t.Fatalf("List() %v does not name the running process %d", procs, pid)
	}

	if err := rt.Stop(pid); err != nil {
		t.Fatal(err)
	}
	waitDead(t, pid)

	procs, err = rt.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == pid {
			t.Fatalf("List() still names the stopped process %d", pid)
		}
	}
}

// stopLeftoverEgress ends an egress process by its bare pid alone --
// there is no *exec.Cmd to signal across a restart, only the pid actor.json
// (or the supervisor's List) named -- and treats a pid already gone, or
// pid 0, as success rather than an error (ADR-051 decision 5).
func TestStopLeftoverEgress(t *testing.T) {
	rs := startTestEgress(t, "lease-leftover")
	pid := rs.egressCmd.Process.Pid
	t.Cleanup(func() { rs.close() })

	if err := stopLeftoverEgress(pid); err != nil {
		t.Fatal(err)
	}
	waitDead(t, pid)

	if err := stopLeftoverEgress(pid); err != nil {
		t.Fatalf("stopping an already-gone pid: %v", err)
	}
	if err := stopLeftoverEgress(0); err != nil {
		t.Fatalf("stopping pid 0: %v", err)
	}
}

// stopEgress ends the actor's own hold on the egress process and
// removes its socket directory; a second call, or one before the
// process ever started, is a safe no-op (ADR-051 decision 3).
func TestStopEgressRemovesTheSocketDirectory(t *testing.T) {
	rs := startTestEgress(t, "lease-stop")
	pid := rs.egressCmd.Process.Pid
	dir := rs.egressDir

	if err := rs.stopEgress(); err != nil {
		t.Fatal(err)
	}
	waitDead(t, pid)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the socket directory %s is still there", dir)
	}
	if rs.egressCmd != nil || rs.egressDir != "" {
		t.Fatal("stopEgress left the sidecar's own fields set")
	}
	if err := rs.stopEgress(); err != nil {
		t.Fatalf("stopping a second time: %v", err)
	}
	if err := (&relaySidecar{}).stopEgress(); err != nil {
		t.Fatalf("stopping a sidecar whose egress never started: %v", err)
	}
}

// watchEgress ends the lease, as a killed pod does, when the egress
// process exits on its own -- an external kill, never stopEgress
// (ADR-051 decision 3).
func TestWatchEgressEndsTheLeaseWhenItExitsOnItsOwn(t *testing.T) {
	rs := startTestEgress(t, "lease-watch")
	t.Cleanup(func() { rs.close() })
	pid := rs.egressCmd.Process.Pid

	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	watched := make(chan struct{})
	go func() { rs.watchEgress(ctx, cancel); close(watched) }()

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-watched:
	case <-time.After(5 * time.Second):
		t.Fatal("watchEgress did not return once the process exited")
	}

	var ke *killedError
	cause := context.Cause(ctx)
	if !errors.As(cause, &ke) || !strings.Contains(ke.why, errEgressKilled.Error()) {
		t.Fatalf("cancel cause %v, want a *killedError naming %v", cause, errEgressKilled)
	}
}

// watchEgress does nothing when stopEgress is what ended the process:
// the lease is already ending on its own terms, never surprised by an
// infrastructure loss it caused itself.
func TestWatchEgressDoesNothingWhenStopEgressEndedIt(t *testing.T) {
	rs := startTestEgress(t, "lease-watch-stop")

	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	watched := make(chan struct{})
	go func() { rs.watchEgress(ctx, cancel); close(watched) }()

	if err := rs.stopEgress(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-watched:
	case <-time.After(5 * time.Second):
		t.Fatal("watchEgress did not return once stopEgress ended the process")
	}
	if ctx.Err() != nil {
		t.Fatalf("stopEgress's own exit ended the lease: %v", context.Cause(ctx))
	}
}

// watchEgress returns without acting once ctx ends first -- the lease
// ending some other way -- never racing to blame the egress process
// for it.
func TestWatchEgressReturnsWhenCtxEndsFirst(t *testing.T) {
	rs := startTestEgress(t, "lease-watch-ctx")
	t.Cleanup(func() { rs.close() })

	ctx, cancel := context.WithCancelCause(context.Background())
	watched := make(chan struct{})
	go func() { rs.watchEgress(ctx, cancel); close(watched) }()
	cancel(errors.New("the lease ended some other way"))
	select {
	case <-watched:
	case <-time.After(5 * time.Second):
		t.Fatal("watchEgress did not return once ctx ended")
	}
}
