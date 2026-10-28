package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

func laneRunnerOn(t *testing.T, rt *sandbox.Fake, yml string) (laneSandboxRunner, *sandbox.Session) {
	t.Helper()
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	sb.Config.ServiceTimeout = 2 * time.Second
	dir := t.TempDir()
	writeServices(t, dir, yml)
	logf := func(string, ...any) {}
	first, err := OpenSessionPod(context.Background(), sb, dir, "pr-1", harness.Kind("validate"), sandbox.GitIdentity{}, logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	return laneSandboxRunner{
		sandboxRunner: sandboxRunner{sess: first.Session, dir: dir},
		open: func(ctx context.Context, name string) (*SessionPod, error) {
			return OpenSessionPod(ctx, sb, dir, "pr-1-"+name, harness.Kind("validate"), sandbox.GitIdentity{}, logf)
		},
	}, first.Session
}

// Each lane is a session of its own with the checkout's own services:
// a pod with its own Postgres, removed when the lane is released.
func TestALaneHasItsOwnPodAndServices(t *testing.T) {
	rt := &sandbox.Fake{}
	lr, _ := laneRunnerOn(t, rt, servicesYML)
	var _ validate.LaneRunner = lr
	r, release, err := lr.Lane(context.Background(), "integration-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Pods) != 2 {
		t.Fatalf("pods = %d, want the run's and the lane's", len(rt.Pods))
	}
	var postgres int
	for _, sc := range rt.Sidecars {
		if strings.HasPrefix(sc.Image, "postgres") {
			postgres++
		}
	}
	if postgres != 2 {
		t.Fatalf("postgres sidecars = %d, want one per pod", postgres)
	}
	if r.(goCacheRunner).Runner.(sandboxRunner).sess == lr.sess {
		t.Fatal("the lane runs in the run's own session")
	}
	release()
	if !slices.Contains(rt.Removed, rt.Pods[1].Name) {
		t.Fatalf("lane pod %s not removed on release: %v", rt.Pods[1].Name, rt.Removed)
	}
}

// A lane whose required service does not start is no lane: a shard is
// never run without its database.
func TestALaneWithoutItsRequiredServiceIsRefused(t *testing.T) {
	rt := &sandbox.Fake{}
	lr, _ := laneRunnerOn(t, rt, servicesYML)
	rt.SidecarErr = map[string]error{"postgres": errors.New("pull access denied")}
	_, _, err := lr.Lane(context.Background(), "integration-1")
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("err %v, want the required service named", err)
	}
	if !slices.Contains(rt.Removed, rt.Pods[len(rt.Pods)-1].Name) {
		t.Fatalf("the refused lane's pod was left: %v", rt.Removed)
	}
}
