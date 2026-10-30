package sandbox

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

// An isolated pod has no network but its loopback and shares only its
// network namespace between its containers: no PID, IPC or UTS, so the
// agent cannot see the relay sidecar's processes. No network rules are
// loaded, since there is no network to hold to them.
func TestAnIsolatedPodHasNoNetworkAndSharesOnlyIt(t *testing.T) {
	args := PodCreateArgs(PodSpec{Name: "p", Network: Isolated()})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--network none", "--share net", "--userns keep-id:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("pod create lacks %q: %s", want, joined)
		}
	}
	for _, never := range []string{"pasta", "--dns", "pid", "ipc"} {
		if strings.Contains(joined, never) {
			t.Errorf("pod create has %q: %s", never, joined)
		}
	}
	var ran [][]string
	p := &Podman{Exec: func(_ context.Context, args, _ []string, _ io.Reader, _, _ io.Writer) error {
		ran = append(ran, args)
		return nil
	}}
	if _, err := p.CreatePod(context.Background(), PodSpec{Name: "p", UID: 1000, GID: 1000, Network: Isolated()}); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0][0] != "pod" {
		t.Fatalf("an isolated pod ran %q, want its pod create alone", ran)
	}
}

// The relay sidecar runs as a UID of its own, never the agent's, on a
// read-only root with every capability dropped and no new privileges;
// its credential tmpfs is its own, mode 0700; the node's binary and the
// checkout are read-only; nothing of a container runtime is mounted;
// and no credential is on either command line.
func TestTheRelaySidecarIsConfined(t *testing.T) {
	pod := Pod{Name: "lse-1"}
	args := RelayRunArgs(pod, RelaySpec{Image: "mesh-node:1", Binary: "/opt/mesh", SocketDir: "/tmp/mesh-1/abc", Checkout: "/w/leases/l/work",
		Args: []string{"relay-sidecar", "--credentials-dir", RelayCredentialsDir}})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--user 64000:64000", "--read-only", "--cap-drop all", "--security-opt no-new-privileges", "--unsetenv-all",
		"--mount type=tmpfs,destination=/run/mesh-relay,tmpfs-size=1m,tmpfs-mode=0700,chown=true",
		"--volume /opt/mesh:" + RelayBinary + ":ro",
		"--volume /w/leases/l/work:" + RelayCheckout + ":ro",
		"--volume /tmp/mesh-1/abc:" + RelaySocketDir + ":rw",
		"--pod lse-1", "--name lse-1-relay",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the relay sidecar's run lacks %q:\n%s", want, joined)
		}
	}
	for _, never := range []string{"--privileged", "--cap-add", "podman.sock", "docker.sock", "--pid ", "--pid=", "--ipc", "--network"} {
		if strings.Contains(joined, never) {
			t.Errorf("the relay sidecar's run has %q:\n%s", never, joined)
		}
	}
	if RelayUID == DefaultImageUID {
		t.Fatal("the relay sidecar's UID is the agent's")
	}
	install := RelayInstallArgs(pod)
	if !slices.Contains(install, "-i") || !slices.Contains(install, "64000:64000") || install[len(install)-1] != RelayCredentialsDir {
		t.Fatalf("the install is %q", install)
	}
	// the agent's run names nothing of the sidecar's
	agent := strings.Join(AgentRunArgs(pod, AgentSpec{Name: "a", Image: "mesh-node:1", Command: []string{"claude"}}), " ")
	for _, never := range []string{RelayCredentialsDir, RelaySocketDir, "64000"} {
		if strings.Contains(agent, never) {
			t.Fatalf("the agent's run names %q: %s", never, agent)
		}
	}
}

// Session.StartRelay makes the pod when nothing has, and the install
// hands the runtime the credentials on stdin, never on argv.
func TestSessionStartsAndInstallsTheRelay(t *testing.T) {
	f := &Fake{}
	sb := &Sandbox{Runtime: f, Config: Config{Image: "mesh-node:1", Network: Isolated()}}
	sess, err := sb.Open("lse-1", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	dir := t.TempDir()
	if err := sess.StartRelay(context.Background(), RelaySpec{Binary: dir, SocketDir: dir}); err != nil {
		t.Fatal(err)
	}
	if err := sess.InstallRelay(context.Background(), strings.NewReader(`{"lease":"lse-1"}`)); err != nil {
		t.Fatal(err)
	}
	if len(f.Pods) != 1 || !f.Pods[0].Network.None || len(f.Relays) != 1 || f.Relays[0].Image != "mesh-node:1" {
		t.Fatalf("pods %+v relays %+v", f.Pods, f.Relays)
	}
	if len(f.RelayCredentials) != 1 || string(f.RelayCredentials[0]) != `{"lease":"lse-1"}` {
		t.Fatalf("installed %q", f.RelayCredentials)
	}
}
