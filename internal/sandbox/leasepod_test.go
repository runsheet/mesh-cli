package sandbox

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A pinned session's pod has the pinned name and labels beside its own;
// its agent runs on SetLimits' CPUs and memory, named AgentName in the
// pod, and an Exec runs beside it in the same pod; neither removes the
// pod, which Close does.
func TestAPinnedSessionRunsItsAgentBesideItsExecs(t *testing.T) {
	f := &Fake{}
	sb := testSandbox(t, f)
	sess, err := sb.Open("lease-1", "conform/echo", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	sess.Pin("mesh-v1-lease-1", map[string]string{"mesh.job": "job-1", "mesh.node": "node-1"})
	sess.SetLimits(Limits{CPUs: 1, Memory: "2048m"})
	if sess.PodName() != "mesh-v1-lease-1" {
		t.Fatalf("pod name %q before the pod", sess.PodName())
	}
	ws := t.TempDir()
	if _, err := sess.Start(context.Background(), nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := sess.Exec(context.Background(), RunSpec{Workspace: ws, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	f.Agent = func(_ context.Context, spec AgentSpec) (Exit, error) {
		if strings.HasSuffix(spec.Name, "-agent") {
			return Exit{Code: 143}, nil
		}
		return Exit{}, nil
	}
	err = sess.RunAgent(context.Background(), RunSpec{Workspace: ws, Command: []string{"claude"}, Vars: []string{"GIT_CONFIG_COUNT=1"}})
	var ee *ExitError
	if !asExit(err, &ee) || ee.Code != 143 {
		t.Fatalf("agent end %v", err)
	}
	pods := f.PodSpecs()
	if len(pods) != 1 || pods[0].Name != "mesh-v1-lease-1" {
		t.Fatalf("pods %+v", pods)
	}
	for k, want := range map[string]string{SessionLabel: "lease-1", "mesh.kind": "conform/echo", "mesh.job": "job-1", "mesh.node": "node-1"} {
		if pods[0].Labels[k] != want {
			t.Errorf("label %s = %q", k, pods[0].Labels[k])
		}
	}
	agents := f.AgentSpecs()
	if len(agents) != 2 || agents[0].Name != "mesh-v1-lease-1-exec-1" || agents[1].Name != "mesh-v1-lease-1-agent" {
		t.Fatalf("containers %+v", agents)
	}
	for _, a := range agents {
		if a.Limits.CPUs != 1 || a.Limits.Memory != "2048m" || a.Limits.Pids != DefaultPids {
			t.Fatalf("%s limits %+v", a.Name, a.Limits)
		}
	}
	if !slices.Contains(agents[1].Env, "GIT_CONFIG_COUNT=1") {
		t.Fatalf("the agent's vars: %v", agents[1].Env)
	}
	if len(f.Removed) != 0 {
		t.Fatalf("the pod was removed under the session: %v", f.Removed)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.Removed, "mesh-v1-lease-1") {
		t.Fatalf("close removed %v", f.Removed)
	}
}

// An unpinned session's pods are named and labelled as ever.
func TestAnUnpinnedSessionIsAsBefore(t *testing.T) {
	f := &Fake{}
	sess, err := testSandbox(t, f).Open("item-1", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Run(context.Background(), RunSpec{Workspace: t.TempDir(), Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	pods := f.PodSpecs()
	if len(pods) != 1 || !strings.HasPrefix(pods[0].Name, "mesh-item-1-") || len(pods[0].Labels) != 2 {
		t.Fatalf("pods %+v", pods)
	}
	if a := f.AgentSpecs()[0]; a.Limits != (Limits{CPUs: DefaultCPUs, Memory: DefaultMemory, Pids: DefaultPids}) {
		t.Fatalf("limits %+v", a.Limits)
	}
}

// A live SetAgentLimits change — node.yaml's sandbox.memory edited and
// the watcher reapplying it — never reaches a session whose pod already
// runs: SetLimits read the sandbox's limits once, at the pod's own
// creation, and keeps that snapshot. A session opened afterwards, as a
// new pod's creation, reads the new limits (ADR-007 amendment, a node's
// pod limits live).
func TestALiveLimitsChangeKeepsARunningPodButGivesTheNextOneTheNew(t *testing.T) {
	f := &Fake{}
	sb := testSandbox(t, f)

	running, err := sb.Open("lease-1", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	// pod creation: the session reads the sandbox's limits as they are now
	running.SetLimits(sb.AgentLimits())
	ws := t.TempDir()
	if err := running.Run(context.Background(), RunSpec{Workspace: ws, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if got := f.AgentSpecs()[0].Limits; got.Memory != DefaultMemory {
		t.Fatalf("the running pod's own limits: %+v, want memory %s", got, DefaultMemory)
	}

	// node.yaml's sandbox.memory changes while the pod above still runs
	sb.SetAgentLimits(Limits{CPUs: DefaultCPUs, Memory: "9999m", Pids: DefaultPids})

	if got := running.limits(); got.Memory != DefaultMemory {
		t.Fatalf("the running pod's session after the change: %+v, want memory %s (its original)", got, DefaultMemory)
	}
	if err := running.Exec(context.Background(), RunSpec{Workspace: ws, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.AgentSpecs() {
		if a.Limits.Memory != DefaultMemory {
			t.Fatalf("the running pod's own container %s: memory %s, want its original %s", a.Name, a.Limits.Memory, DefaultMemory)
		}
	}

	// the next pod created — a new session — is given the new limit
	next, err := sb.Open("lease-2", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	next.SetLimits(sb.AgentLimits())
	if err := next.Run(context.Background(), RunSpec{Workspace: t.TempDir(), Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	specs := f.AgentSpecs()
	if got := specs[len(specs)-1].Limits.Memory; got != "9999m" {
		t.Fatalf("the next pod's limits: memory %s, want 9999m (the new one)", got)
	}
}

func asExit(err error, target **ExitError) bool {
	e, ok := err.(*ExitError)
	if ok {
		*target = e
	}
	return ok
}
