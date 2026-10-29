package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// actorRig is a rig whose node runs its leases as job actors, with a
// scheduler and a fake sandbox runtime.
func actorRig(t *testing.T, sess Session) (*rig, *sandbox.Fake) {
	g := newRig(t, sess)
	sb := &sandbox.Fake{}
	g.rt.Actors = true
	g.rt.Sandbox = sb
	g.rt.PodWatchEvery = 10 * time.Millisecond
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 1})
	return g, sb
}

func onlyHandBack(t *testing.T, g *rig, job string) meshapi.HandBackRequest {
	t.Helper()
	calls := g.fake.Calls("hand-back")
	if len(calls) != 1 || calls[0].LeaseID != g.fake.LeaseOf(job) {
		t.Fatalf("hand-backs %+v", calls)
	}
	if n := len(g.fake.Calls("fail")) + len(g.fake.Calls("finish")); n != 0 {
		t.Fatalf("%d finishes and failures beside the hand-back", n)
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(calls[0].Body, &hb)
	return hb
}

// assertReleased: the actor left nothing behind — no job directory, no
// room in the scheduler, no live lease.
func assertReleased(t *testing.T, g *rig, job string) {
	t.Helper()
	if _, err := os.Stat(g.rt.leaseDir(g.fake.LeaseOf(job))); !os.IsNotExist(err) {
		t.Fatalf("the job directory outlived the actor: %v", err)
	}
	if held := g.rt.Scheduler.Leases(); len(held) != 0 {
		t.Fatalf("the scheduler still holds %+v", held)
	}
	g.rt.mu.Lock()
	n := g.rt.leases.Len()
	g.rt.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d leases still live", n)
	}
}

// A job actor runs its lease through taken, preparing and running — with
// actor.json on disk naming what it holds — reports, then releases
// everything and is gone.
func TestAJobActorRunsItsLeaseAndReleases(t *testing.T) {
	var during actor.Record
	sess := &stubSession{calls: []string{"conform/submit"}, inspect: func(spec SessionSpec) {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(spec.Workspace), actor.FileName))
		if err != nil {
			t.Errorf("no actor.json beside the workspace: %v", err)
			return
		}
		json.Unmarshal(b, &during)
	}, result: SessionResult{SessionID: "sess-1", Text: "done"}}
	g, _ := actorRig(t, sess)
	job := g.offer(t, nil)
	run(t, g, context.Background())
	if during.State != actor.Running || during.JobID != job || during.LeaseID != g.fake.LeaseOf(job) {
		t.Fatalf("actor.json while the session ran: %+v", during)
	}
	for _, h := range []string{"room", "the relay grant", "the sessionproxy"} {
		if !slices.Contains(during.Holds, h) {
			t.Fatalf("the actor did not hold %q: %v", h, during.Holds)
		}
	}
	if len(g.fake.Calls("finish")) != 1 {
		t.Fatalf("%d finishes", len(g.fake.Calls("finish")))
	}
	if n := len(g.latchkey.revoked); n == 0 {
		t.Fatal("releasing did not revoke the grant's git token")
	}
	assertReleased(t, g, job)
}

// panicSession panics in the middle of a lease.
type panicSession struct{}

func (panicSession) Run(context.Context, SessionSpec) (SessionResult, error) {
	panic("the harness broke")
}

// A panic in the lease is recovered into its report — a hand-back with
// cause panic — and then its release: nothing it held outlives it.
func TestAPanicIsRecoveredIntoReleasing(t *testing.T) {
	g, _ := actorRig(t, panicSession{})
	job := g.offer(t, nil)
	run(t, g, context.Background())
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CausePanic || !strings.Contains(hb.Reason, "the harness broke") {
		t.Fatalf("hand-back %+v", hb)
	}
	assertReleased(t, g, job)
}

// podSession runs its agent detached in the lease's pod, as a sandboxed
// session does, and waits for it to end.
type podSession struct {
	sb      *sandbox.Fake
	started chan string
}

func (s *podSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	pod := sandbox.Pod{Name: spec.Pod}
	agent := sandbox.AgentName(pod)
	if err := s.sb.StartAgent(ctx, pod, sandbox.AgentSpec{Name: agent, Image: "mesh-agent"}); err != nil {
		return SessionResult{}, err
	}
	s.started <- agent
	<-ctx.Done()
	return SessionResult{SessionID: "sess-pod"}, ctx.Err()
}

// A pod killed mid-run — its agent exits 137, its sidecar left standing —
// is not the work's red: the lease is handed back pod_killed, and the
// actor removes the pod with its sidecar as it releases.
func TestAPodKilledMidRunHandsBackPodKilled(t *testing.T) {
	sess := &podSession{started: make(chan string, 1)}
	g, sb := actorRig(t, sess)
	sess.sb = sb
	sb.Agent = func(ctx context.Context, _ sandbox.AgentSpec) (sandbox.Exit, error) {
		<-ctx.Done()
		return sandbox.Exit{}, nil
	}
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Pods = []json.RawMessage{json.RawMessage(`{"name":"postgres","image":"postgres:17-alpine","env":["POSTGRES_PASSWORD=mysecret"]}`)}
	})
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	var agent string
	select {
	case agent = <-sess.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the session never started its agent")
	}
	pod := PodName(g.fake.LeaseOf(job))
	if specs := sb.PodSpecs(); len(specs) != 1 || specs[0].Name != pod || specs[0].Labels[PodLeaseLabel] != g.fake.LeaseOf(job) {
		t.Fatalf("pods %+v", specs)
	}
	if len(sb.Sidecars) != 1 || sb.Sidecars[0].Name != "postgres" {
		t.Fatalf("sidecars %+v", sb.Sidecars)
	}
	sb.KillAgent(agent, 137)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the lease outlived its killed pod")
	}
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CausePodKilled || !strings.Contains(hb.Reason, "exited 137") {
		t.Fatalf("hand-back %+v", hb)
	}
	if !slices.Contains(sb.Removed, pod) {
		t.Fatalf("the pod was not removed: %v", sb.Removed)
	}
	assertReleased(t, g, job)
}

// A session killed from outside (SIGKILL, with no pod) is pod_killed
// too; one the actor stopped itself, on Mesh's cancel, is cancelled.
func TestOnlyAKillTheActorDidNotCauseIsPodKilled(t *testing.T) {
	t.Run("killed from outside", func(t *testing.T) {
		g, _ := actorRig(t, &stubSession{err: errors.New("claude: signal: killed"), result: SessionResult{SessionID: "sess-k"}})
		job := g.offer(t, nil)
		run(t, g, context.Background())
		if hb := onlyHandBack(t, g, job); hb.Cause != meshapi.CausePodKilled || hb.SessionRef != "sess-k" {
			t.Fatalf("hand-back %+v", hb)
		}
	})
	t.Run("without actors it is cannot_run, as before", func(t *testing.T) {
		g := newRig(t, &stubSession{err: errors.New("claude: signal: killed")})
		job := g.offer(t, nil)
		run(t, g, context.Background())
		if hb := onlyHandBack(t, g, job); hb.Cause != meshapi.CauseCannotRun {
			t.Fatalf("hand-back %+v", hb)
		}
	})
	t.Run("stopped by the actor", func(t *testing.T) {
		sess := &stubSession{hold: make(chan struct{})}
		g, _ := actorRig(t, sess)
		g.rt.HeartbeatEvery = time.Hour
		job := g.offer(t, nil)
		done := make(chan error, 1)
		go func() { done <- g.rt.Run(context.Background()) }()
		waitFor(t, "the session", func() bool { return len(sess.specsSoFar()) == 1 && g.fake.Streams() > 0 })
		g.fake.StreamCancel(g.fake.LeaseOf(job), "superseded", "replaced")
		<-done
		if hb := onlyHandBack(t, g, job); hb.Cause != meshapi.CauseCancelled {
			t.Fatalf("hand-back %+v", hb)
		}
		assertReleased(t, g, job)
	})
}

// A node restarted mid-job finds the actor it left behind: the lease is
// handed back node_restart, its pod and its job directory removed,
// before the node takes anything new.
func TestARestartMidJobReleasesTheLeftoverActor(t *testing.T) {
	g, sb := actorRig(t, &stubSession{calls: []string{"conform/submit"}})
	// the node that stopped: an actor running lse_old in its pod
	dir := g.rt.leaseDir("lse_old")
	a, err := actor.Start(dir, actor.Record{LeaseID: "lse_old", JobID: "job_old"})
	if err != nil {
		t.Fatal(err)
	}
	a.Enter(actor.Preparing)
	pod := PodName("lse_old")
	a.HoldPod(pod, nil)
	a.Enter(actor.Running)
	sb.Standing = []sandbox.PodInfo{{Name: pod, Labels: map[string]string{PodLeaseLabel: "lse_old"}}}
	job := g.offer(t, nil)
	run(t, g, context.Background())
	calls := g.fake.Calls("hand-back")
	if len(calls) == 0 || calls[0].LeaseID != "lse_old" {
		t.Fatalf("hand-backs %+v", calls)
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(calls[0].Body, &hb)
	if hb.Cause != meshapi.CauseNodeRestart || !strings.Contains(hb.Reason, "running") {
		t.Fatalf("hand-back %+v", hb)
	}
	if !slices.Contains(sb.Removed, pod) {
		t.Fatalf("the leftover pod was not removed: %v", sb.Removed)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the leftover job directory is still there: %v", err)
	}
	if i, _ := sb.Called("remove " + pod); i < 0 {
		t.Fatal("no remove call")
	}
	if len(g.fake.Calls("finish")) != 1 {
		t.Fatalf("the new lease after the restart: %d finishes", len(g.fake.Calls("finish")))
	}
	assertReleased(t, g, job)
}
