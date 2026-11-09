package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/supervisor"
)

// quietSession holds its lease and keeps what its mailbox brings.
type quietSession struct {
	got chan Message
}

func (s *quietSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	for {
		select {
		case m := <-spec.Mailbox.C():
			s.got <- m
		case <-ctx.Done():
			return SessionResult{SessionID: "sess-quiet"}, ctx.Err()
		}
	}
}

// A session that shows no progress is escalated through its mailbox on
// stall after QuietAfter, then — still quiet — stopped and handed back
// stalled, and its actor releases everything (PR 3.3).
func TestAQuietSessionIsEscalatedThenStalled(t *testing.T) {
	sess := &quietSession{got: make(chan Message, 4)}
	g, _ := actorRig(t, sess)
	g.rt.SupervisorMode = supervisor.Cancel
	g.rt.QuietAfter = 300 * time.Millisecond
	g.rt.SuperviseEvery = 20 * time.Millisecond
	job := g.offer(t, nil)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	select {
	case m := <-sess.got:
		if m.Kind != MsgEscalate || m.On != escalate.OnStall || !strings.Contains(m.Text, "no progress") {
			t.Fatalf("the mailbox brought %+v", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the quiet session was never escalated")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the quiet session was never stopped")
	}
	hb := onlyHandBack(t, g, job)
	if hb.Cause != "stalled" || !strings.Contains(hb.Reason, "after escalating") || hb.SessionRef != "sess-quiet" {
		t.Fatalf("hand-back %+v", hb)
	}
	assertReleased(t, g, job)
}

// The reconcile, beside a live actor, removes a pod of the node's that no
// actor holds and an old job directory no actor holds; the live lease's
// own are left (PR 3.3).
func TestTheReconcileRemovesWhatNoActorHolds(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g, sb := actorRig(t, sess)
	g.rt.SupervisorMode = supervisor.Cancel
	g.rt.ReconcileEvery = 30 * time.Millisecond
	g.rt.SuperviseEvery = time.Hour
	sb.Standing = []sandbox.PodInfo{{Name: PodName("lse_gone"), Labels: map[string]string{PodLeaseLabel: "lse_gone"}}}
	orphan := g.rt.leaseDir("lse_gone")
	os.MkdirAll(orphan, 0o700)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(orphan, old, old)
	job := g.offer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	waitFor(t, "the orphan pod's removal", func() bool { _, ok := sb.Called("remove " + PodName("lse_gone")); return ok })
	waitFor(t, "the orphan directory's removal", func() bool { _, err := os.Stat(orphan); return os.IsNotExist(err) })
	if _, err := os.Stat(g.rt.leaseDir(g.fake.LeaseOf(job))); err != nil {
		t.Fatalf("the live lease's directory went: %v", err)
	}
	if slices.Contains(sb.Removed, PodName(g.fake.LeaseOf(job))) {
		t.Fatal("the live lease's pod went")
	}
	close(sess.hold)
	<-done
}

// A stray lease-egress process -- its lease's actor.json names it, but
// the lease is no longer live and no hand-off names it either -- is
// stopped by the reconcile, and the reconcile line counts it
// (acceptance check 4 of the goal).
func TestTheReconcileStopsAStrayEgressProcess(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("no /proc")
	}
	sess := &stubSession{hold: make(chan struct{})}
	g, _ := actorRig(t, sess)
	g.rt.SupervisorMode = supervisor.Cancel
	g.rt.ReconcileEvery = 30 * time.Millisecond
	g.rt.SuperviseEvery = time.Hour
	var lines []string
	var mu sync.Mutex
	g.rt.Logf = func(format string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, a...))
		mu.Unlock()
		t.Logf(format, a...)
	}

	cmd := exec.Command("sleep", "100")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stray process: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	// the test is its parent, as a real orphan's would not be: reap it
	// as soon as it dies, or it sits a zombie and kill(pid, 0) never
	// reports ESRCH.
	go cmd.Wait()

	stray := g.rt.leaseDir("lse_stray")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := actor.Record{EgressPID: pid, EgressControl: "/tmp/stray.sock"}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(stray, actor.FileName), b, 0o600); err != nil {
		t.Fatal(err)
	}

	job := g.offer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(-pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the stray egress process %d outlived the reconcile", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "the reconcile line counting the stray egress process", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range lines {
			if strings.Contains(l, "stopped 1 egress process") {
				return true
			}
		}
		return false
	})

	// the live lease's own job directory is untouched
	if _, err := os.Stat(g.rt.leaseDir(g.fake.LeaseOf(job))); err != nil {
		t.Fatalf("the live lease's directory went: %v", err)
	}

	close(sess.hold)
	<-done
}

// The supervisor's mode is read live from node.yaml (ADR-044 amendment,
// 2026-11-09): changing `v1.supervisor` from warn to tidy while the
// Runtime runs makes the very next reconcile remove a pod and a job
// directory no actor holds, with no process restart, while a live
// lease whose session stays quiet past 2xQuietAfter is still neither
// escalated nor handed back -- tidy reconciles like cancel, but never
// acts on the escalate/stall ladder, same as warn.
func TestTheSupervisorsModeIsReadLiveFromNodeYAML(t *testing.T) {
	sess := &quietSession{got: make(chan Message, 4)}
	g, sb := actorRig(t, sess)
	g.rt.SupervisorMode = supervisor.Warn
	g.rt.QuietAfter = 100 * time.Millisecond
	g.rt.SuperviseEvery = 20 * time.Millisecond
	g.rt.ReconcileEvery = 30 * time.Millisecond

	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(path, []byte("v1:\n  supervisor: warn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.rt.SupervisorConfigPath = path
	g.rt.SupervisorPoll = 20 * time.Millisecond

	sb.Standing = []sandbox.PodInfo{{Name: PodName("lse_gone"), Labels: map[string]string{PodLeaseLabel: "lse_gone"}}}
	orphan := g.rt.leaseDir("lse_gone")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(orphan, old, old)

	job := g.offer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()

	// warn alone: the reconcile finds the orphan but only logs it, and
	// the quiet session is never escalated
	time.Sleep(150 * time.Millisecond)
	if _, ok := sb.Called("remove " + PodName("lse_gone")); ok {
		t.Fatal("the orphan pod went while still in warn mode")
	}
	select {
	case m := <-sess.got:
		t.Fatalf("the mailbox brought %+v while in warn mode", m)
	default:
	}

	// node.yaml now names tidy: the live watcher picks it up on its
	// next poll and calls SetMode, with no restart of the Runtime
	if err := os.WriteFile(path, []byte("v1:\n  supervisor: tidy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the orphan pod's removal once tidy takes effect", func() bool {
		_, ok := sb.Called("remove " + PodName("lse_gone"))
		return ok
	})
	waitFor(t, "the orphan directory's removal once tidy takes effect", func() bool {
		_, err := os.Stat(orphan)
		return os.IsNotExist(err)
	})

	if _, err := os.Stat(g.rt.leaseDir(g.fake.LeaseOf(job))); err != nil {
		t.Fatalf("the live lease's directory went: %v", err)
	}
	if slices.Contains(sb.Removed, PodName(g.fake.LeaseOf(job))) {
		t.Fatal("the live lease's pod went")
	}

	// tidy still never acts on the escalate/stall ladder: well past
	// 2xQuietAfter the quiet session is neither escalated nor handed
	// back, and Run has not returned (no restart)
	time.Sleep(3 * g.rt.QuietAfter)
	select {
	case m := <-sess.got:
		t.Fatalf("tidy escalated the quiet session: %+v", m)
	default:
	}
	select {
	case <-done:
		t.Fatal("the Runtime stopped; tidy must reconcile without restarting it")
	default:
	}

	cancel()
	<-done
}
