//go:build !windows

package commands

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/nodev1"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// stubUpgradeSession is a minimal nodev1.Session: it blocks on hold
// (closed to let the lease finish) and then answers result.
type stubUpgradeSession struct {
	hold   chan struct{}
	result nodev1.SessionResult

	mu     sync.Mutex
	starts int
}

func (s *stubUpgradeSession) Run(ctx context.Context, spec nodev1.SessionSpec) (nodev1.SessionResult, error) {
	s.mu.Lock()
	s.starts++
	s.mu.Unlock()
	select {
	case <-s.hold:
	case <-ctx.Done():
		return nodev1.SessionResult{}, ctx.Err()
	}
	return s.result, nil
}

func (s *stubUpgradeSession) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// TestMeshNodeUpgradeV1SignalLeavesLeaseRunningToFinish is `mesh node
// upgrade --api v1` end to end against a real v1 node process: a real
// pidfile (the test's own pid, node.RunState.Started as `mesh node run
// --api v1` leaves it), the real restart handler noderun.go registers
// (onRestartSignal checking node.upgrade-ask before calling
// rt.UpgradeSignal, wired by hand here since nodeRunCmd.RunE builds its
// own nodev1.ClaudeSession with no seam for a fake one, but reading the
// same marker the real handler does), and v1Service{}.Restart -- the
// same SIGHUP `mesh node upgrade --api v1` sends, marked
// node.upgrade-ask first so it is told apart from `mesh node set`'s
// reload of the same signal (adr-044's amendment) -- run against that
// pid, never rt.UpgradeSignal called directly. The lease in flight is
// left running (adr-044's hand-off): no finish or hand-back is
// recorded until the held session is let go, at which point Mesh gets
// exactly one finish and no hand-back for the job.
func TestMeshNodeUpgradeV1SignalLeavesLeaseRunningToFinish(t *testing.T) {
	fake := meshapitest.NewFake(t)
	mesh := httptest.NewServer(fake)
	t.Cleanup(mesh.Close)

	_, nodeKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	sess := &stubUpgradeSession{hold: make(chan struct{}), result: nodev1.SessionResult{SessionID: "sess-1", Text: "done"}}
	rt := &nodev1.Runtime{
		SigningKey: nodeKey,
		API:        &meshapi.Client{BaseURL: mesh.URL, Token: func(context.Context) (string, error) { return "machine", nil }},
		Node: meshapi.RegisterRequest{
			Name: "test-node", Version: "v0.0.0-test",
			CapacityTotal: meshapi.Capacity{CPUs: 4, MemGB: 8, Slots: 1},
			SigningKey:    "ed25519:AAAA",
		},
		WorkRoot:      t.TempDir(),
		MeshBinary:    "/usr/local/bin/mesh",
		Session:       sess,
		Once:          true,
		Logf:          t.Logf,
		Backoff:       10 * time.Millisecond,
		Actors:        true,
		Sandbox:       &sandbox.Fake{},
		PodWatchEvery: 10 * time.Millisecond,
		Scheduler:     scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 1}),
	}

	// a minimal brief: no git checkout, no product/model servers, no
	// finish tool -- settle() synthesizes {"text": sess.result.Text} as
	// the finish with none of that, so the only thing under test is the
	// hand-off itself (lease.go's work(), settle.go).
	job := fake.Offer(func(b *meshapi.Brief) {
		b.Git = nil
		b.Servers = nil
		b.CredentialRef = ""
		b.Access = nil
		b.FinishTool = ""
		b.FinishSchema = nil
	})

	state := node.RunState{Dir: t.TempDir()}
	pid := os.Getpid()
	if err := v1Started(state, pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v1Stopped(state, pid) })

	// UpgradeSignal (nodev1/upgrade_signal.go) calls UpgradeExit once
	// every live lease's hand-off is written -- the observable proof,
	// without ending this test binary, that the SIGHUP the Restart call
	// below sends actually reached rt.UpgradeSignal through the real
	// restart handler, not a direct call to it.
	var mu sync.Mutex
	var exits []int
	origExit := nodev1.UpgradeExit
	nodev1.UpgradeExit = func(code int) { mu.Lock(); exits = append(exits, code); mu.Unlock() }
	defer func() { nodev1.UpgradeExit = origExit }()
	exitCount := func() int { mu.Lock(); defer mu.Unlock(); return len(exits) }

	// mirrors noderun.go's own restart handler: SIGHUP alone does not
	// say which command meant it (`mesh node set`'s reload sends it
	// too, adr-044's amendment), so the handler reads and clears
	// node.upgrade-ask -- written by v1Service.Restart below, before it
	// signals -- to tell this SIGHUP was the upgrade's.
	stop := onRestartSignal(func() {
		if takeV1UpgradeAsk(state) {
			rt.UpgradeSignal("`mesh node upgrade` asked")
		}
	})
	defer stop()

	done := make(chan error, 1)
	go func() { done <- rt.Run(context.Background()) }()

	deadline := time.Now().Add(10 * time.Second)
	for sess.started() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := sess.started(); n != 1 {
		t.Fatalf("session starts %d, want exactly one before the upgrade", n)
	}
	if st := rt.Status(); len(st.Leases) != 1 || st.Leases[0].JobID != job {
		t.Fatalf("status before the upgrade %+v, want the one lease of job %s", st, job)
	}

	var out strings.Builder
	if err := (v1Service{}).Restart(context.Background(), state, &out); err != nil {
		t.Fatalf("v1Service.Restart: %v", err)
	}
	if !strings.Contains(out.String(), "upgrading") {
		t.Fatalf("Restart's message %q, want it to say the node is upgrading", out.String())
	}

	// Restart only sends the signal; the handler runs asynchronously.
	// Wait for UpgradeSignal to have written every lease's hand-off and
	// called UpgradeExit -- the proof that the real SIGHUP path (not a
	// direct call) carried the signal through.
	deadline = time.Now().Add(10 * time.Second)
	for exitCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	n, last := len(exits), append([]int(nil), exits...)
	mu.Unlock()
	if n != 1 || last[0] != 0 {
		t.Fatalf("UpgradeExit calls %v, want exactly one with code 0", last)
	}

	// the lease is left running, nothing yet reported to Mesh
	if n := len(fake.Calls("finish")) + len(fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d calls to Mesh right after the upgrade signal, want none yet: the upgrade should leave the lease running", n)
	}

	close(sess.hold)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil once the handed-off lease finishes", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lease outlived the upgrade signal")
	}

	if n := len(fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes, want the lease's one", n)
	}
	if n := len(fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d hand-backs, want none: the upgrade signal should have left the lease running to finish on its own", n)
	}
}

// v1UpgradeFixtureService is a fake restart for the nodeUpgrade.run
// accounting tests below: it records each call and, when newStatus is
// set, writes it to run's node.status as the restarted process's own
// publish would -- the stand-in for a real second process adopting
// the hand-off and calling OnStatus.
type v1UpgradeFixtureService struct {
	exe       string
	calls     []string
	err       error
	newStatus *nodev1.Status
}

func (s *v1UpgradeFixtureService) Restart(_ context.Context, run node.RunState, out io.Writer) error {
	b, _ := os.ReadFile(s.exe)
	head, _, _ := strings.Cut(string(b), "\n")
	s.calls = append(s.calls, "restart with "+head)
	if s.err != nil {
		return s.err
	}
	if s.newStatus != nil {
		if err := writeV1Status(run, *s.newStatus); err != nil {
			return err
		}
	}
	return nil
}

// v1UpgradeFixture is upgradeFixture's own machine, wired for a Mesh v1
// node: V1Status reads run's node.status (nodev1state.go), and the poll
// after the restart is sped up for the test.
func v1UpgradeFixture(t *testing.T) (*nodeUpgrade, *updateFixture, *v1UpgradeFixtureService) {
	t.Helper()
	f := newUpdateFixture(t)
	svc := &v1UpgradeFixtureService{exe: f.exe}
	run := f.u.Run
	u := &nodeUpgrade{
		Updater:        f.u.Updater,
		Latest:         f.u.Latest,
		Running:        f.u.Running,
		Run:            run,
		Version:        binaryVersion,
		Image:          f.u.Image,
		Service:        svc,
		Out:            f.out,
		V1Status:       func() (nodev1.Status, bool) { return readV1Status(run) },
		V1AdoptPoll:    time.Millisecond,
		V1AdoptTimeout: 2 * time.Second,
	}
	return u, f, svc
}

// Against a running v1 node whose node.status names a lease the
// restart's hand-off cannot adopt (no relay sidecar, or a tunnel that
// intercepts -- drain.go's Adoptable), `mesh node upgrade --api v1`
// refuses: it names the lease, swaps no binary and restarts nothing.
// --force proceeds anyway.
func TestNodeUpgradeV1RefusesUnadoptableLease(t *testing.T) {
	noDrain(t)
	u, f, svc := v1UpgradeFixture(t)
	before := nodev1.Status{PID: 1, Leases: []nodev1.LeaseStatus{
		{LeaseID: "l-1", JobID: "j-1", Kind: "build", Adoptable: false},
		{LeaseID: "l-2", JobID: "j-2", Kind: "build", Adoptable: true},
	}}
	if err := writeV1Status(f.u.Run, before); err != nil {
		t.Fatal(err)
	}
	bin := fakeBinary(t, "mesh v9.9.9")

	err := u.run(context.Background(), "", bin, false)
	if err == nil || !strings.Contains(err.Error(), "l-1") {
		t.Fatalf("err = %v, want it to name the unadoptable lease l-1", err)
	}
	f.unchanged(t)
	if len(svc.calls) != 0 {
		t.Fatalf("restarted despite the refusal: %q", svc.calls)
	}

	svc.newStatus = &nodev1.Status{PID: 2, Leases: []nodev1.LeaseStatus{
		{LeaseID: "l-2", JobID: "j-2", Kind: "build", Adoptable: true},
	}}
	if err := u.run(context.Background(), "", bin, true); err != nil {
		t.Fatalf("upgrade with --force: %v\n%s", err, f.out)
	}
	if len(svc.calls) != 1 {
		t.Fatalf("restarts %q, want one after --force", svc.calls)
	}
	want, _ := os.ReadFile(bin)
	if got, err := os.ReadFile(f.exe); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("executable = %q, %v; want the new build", got, err)
	}
}

// `mesh node upgrade --api v1` prints how many of the leases running
// before the swap the new process's status says it adopted; fewer than
// were running is a non-zero exit, not a quiet success.
func TestNodeUpgradeV1ReportsAdoptedCount(t *testing.T) {
	noDrain(t)
	before := nodev1.Status{PID: 1, Leases: []nodev1.LeaseStatus{
		{LeaseID: "l-1", JobID: "j-1", Kind: "build", Adoptable: true},
		{LeaseID: "l-2", JobID: "j-2", Kind: "build", Adoptable: true},
	}}

	u, f, svc := v1UpgradeFixture(t)
	if err := writeV1Status(f.u.Run, before); err != nil {
		t.Fatal(err)
	}
	svc.newStatus = &nodev1.Status{PID: 2, Leases: []nodev1.LeaseStatus{
		{LeaseID: "l-1", JobID: "j-1", Kind: "build", Adoptable: true},
		{LeaseID: "l-2", JobID: "j-2", Kind: "build", Adoptable: true},
	}}
	if err := u.run(context.Background(), "", fakeBinary(t, "mesh v9.9.9"), false); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, f.out)
	}
	if !strings.Contains(f.out.String(), "2 of 2") {
		t.Fatalf("output lacks the adopted count:\n%s", f.out)
	}

	u2, f2, svc2 := v1UpgradeFixture(t)
	if err := writeV1Status(f2.u.Run, before); err != nil {
		t.Fatal(err)
	}
	svc2.newStatus = &nodev1.Status{PID: 2, Leases: []nodev1.LeaseStatus{
		{LeaseID: "l-1", JobID: "j-1", Kind: "build", Adoptable: true},
	}}
	err := u2.run(context.Background(), "", fakeBinary(t, "mesh v9.9.9"), false)
	if err == nil || !strings.Contains(err.Error(), "1 of 2") {
		t.Fatalf("err = %v, want it to name the shortfall 1 of 2", err)
	}
}
