package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// A lease with no TLS interception is left running across an upgrade:
// its pod, sidecar and agent untouched, nothing reported to Mesh, and
// its brief, sessionproxy socket, push heads and turn ledger written to
// its own hand-off file instead.
func TestUpgradeWritesHandoffAndLeavesLeaseRunning(t *testing.T) {
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
	id := g.fake.LeaseOf(job)
	pod := PodName(id)

	g.rt.Upgrade("the node is updating")

	if slices.Contains(sb.Removed, pod) {
		t.Fatalf("the pod was removed on an upgrade that only wrote a hand-off: %v", sb.Removed)
	}
	if specs := sb.PodSpecs(); len(specs) != 1 || specs[0].Name != pod {
		t.Fatalf("pods %+v", specs)
	}
	if len(sb.Sidecars) != 1 || sb.Sidecars[0].Name != "postgres" {
		t.Fatalf("sidecars %+v", sb.Sidecars)
	}
	if n := len(g.fake.Calls("hand-back")) + len(g.fake.Calls("finish")) + len(g.fake.Calls("fail")); n != 0 {
		t.Fatalf("%d calls to Mesh on an upgrade that only wrote a hand-off", n)
	}

	data, err := os.ReadFile(filepath.Join(g.rt.leaseDir(id), handoffFile))
	if err != nil {
		t.Fatalf("no hand-off file: %v", err)
	}
	var st handoffState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("hand-off file: %v", err)
	}
	if st.Brief == nil || st.Brief.JobID != job || st.Brief.LeaseID != id {
		t.Fatalf("hand-off brief %+v", st.Brief)
	}
	if st.SocketDir == "" {
		t.Fatalf("hand-off socket dir is empty")
	}

	// let the lease end so the test does not leak it: its outcome past
	// this point is not what this test is about.
	sb.KillAgent(agent, 137)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the lease outlived its killed pod")
	}
}

// A lease whose tunnel terminates TLS trusts a CA this process minted
// and holds nowhere but memory: an upgrade can never pick it back up,
// so it is handed back node_update, the same cause a drain's wait
// hands every lease back with, and no hand-off file is left for it.
func TestUpgradeHandsBackTLSInterceptingLeases(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g, sb := actorRig(t, sess)
	g.rt.Pods = &sandbox.Sandbox{Runtime: sb, Logf: t.Logf, Config: sandbox.Config{
		Image:          "mesh-node:test",
		Limits:         sandbox.Limits{CPUs: 2, Memory: "6g", Pids: 4096},
		ServicesLimits: sandbox.Limits{CPUs: 2, Memory: "4g"},
		Registries:     sandbox.DefaultRegistries,
		ServiceTimeout: 5 * time.Second,
	}}
	registryHostRig(t, g)
	job := g.offer(t, registryBrief)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the session", func() bool { return len(sess.specsSoFar()) == 1 })
	id := g.fake.LeaseOf(job)

	g.rt.Upgrade("the node is updating")

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the TLS-intercepting lease outlived its upgrade")
	}

	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseNodeUpdate {
		t.Fatalf("hand-back %+v", hb)
	}
	if _, err := os.Stat(filepath.Join(g.rt.leaseDir(id), handoffFile)); !os.IsNotExist(err) {
		t.Fatalf("a TLS-intercepting lease left a hand-off file: %v", err)
	}
	assertReleased(t, g, job)
}
