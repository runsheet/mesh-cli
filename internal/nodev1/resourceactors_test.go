package nodev1

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/resources"
)

// releaseUpdater has a release due from the first look.
type releaseUpdater struct {
	mu        sync.Mutex
	installed []string
}

func (u *releaseUpdater) Due(context.Context) (string, error) { return "v9.9.9", nil }
func (u *releaseUpdater) Install(_ context.Context, v string) error {
	u.mu.Lock()
	u.installed = append(u.installed, v)
	u.mu.Unlock()
	return nil
}

// A self-update drains the node; a lease still running past the drain
// wait is handed back node_update — no attempt spent — and once its actor
// has released, the release is installed and Run ends for the restart
// (PR 3.4).
func TestSelfUpdateDrainsThenHandsBackNodeUpdate(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g, _ := actorRig(t, sess)
	g.rt.Once = false
	g.rt.HeartbeatEvery = time.Hour
	u := &releaseUpdater{}
	g.rt.UpdateEvery = time.Hour
	g.rt.DrainWait = 200 * time.Millisecond
	// the update is due before the first take: the lease must already be
	// running, so offer it and hold the updater's first look back
	job := g.offer(t, nil)
	gate := make(chan struct{})
	g.rt.Updater = gatedUpdater{gate: gate, u: u}
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the session", func() bool { return len(sess.specsSoFar()) == 1 })
	close(gate)
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("Run = %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the update never finished")
	}
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseNodeUpdate || !strings.Contains(hb.Reason, "v9.9.9") {
		t.Fatalf("hand-back %+v", hb)
	}
	if len(u.installed) != 1 {
		t.Fatalf("installed %v", u.installed)
	}
	assertReleased(t, g, job)
	// a take already in flight when the drain came may finish; none
	// starts after it
	takes := len(g.fake.Calls("take"))
	time.Sleep(100 * time.Millisecond)
	if n := len(g.fake.Calls("take")); n != takes {
		t.Fatalf("the drained node went on taking: %d takes, then %d", takes, n)
	}
}

// gatedUpdater answers its first look only once the gate opens.
type gatedUpdater struct {
	gate chan struct{}
	u    resources.Updater
}

func (g gatedUpdater) Due(ctx context.Context) (string, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return g.u.Due(ctx)
}
func (g gatedUpdater) Install(ctx context.Context, v string) error { return g.u.Install(ctx, v) }
