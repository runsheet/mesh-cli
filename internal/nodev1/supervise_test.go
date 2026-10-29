package nodev1

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

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
