package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
	"projectmesh.io/mesh-cli/internal/sockettest"
)

// TestRelaySidecarE2E runs a lease's relay as a sidecar in a real
// isolated pod (projectmesh-io/api ADR 001 amendment "relay sidecar")
// and probes it from the agent's container:
//
//  1. the agent cannot reach the internet directly;
//  2. it reaches a host the lease's egress names through the sidecar,
//     whose tunnel is the node's (in this test's process);
//  3. a host the egress does not name is refused;
//  4. no credential is visible to it: no process of the sidecar's (the
//     pod shares no PID namespace), no token in any /proc/*/environ it
//     can read or in its own environment, no credential tmpfs or lease
//     socket in its mounts, no runtime socket.
//
// From the sidecar's side, its credential tmpfs is 0700 and the file
// 0400, both its own UID, which is not the agent's.
//
// It needs podman, the node image (MESH_SANDBOX_IMAGE, else
// mesh-node:dev), Go to build a Linux mesh binary, and the internet
// (example.com), so it runs only with MESH_SANDBOX_E2E=1 — on dl360,
// not on a Mac without podman.
func TestRelaySidecarE2E(t *testing.T) {
	if os.Getenv("MESH_SANDBOX_E2E") != "1" {
		t.Skip("MESH_SANDBOX_E2E=1 runs a real pod")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the relay sidecar's e2e runs on a Linux node")
	}
	image := os.Getenv("MESH_SANDBOX_IMAGE")
	if image == "" {
		image = DefaultImage("dev")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rt := &Podman{}
	if _, err := rt.Version(ctx); err != nil {
		t.Fatalf("podman: %v", err)
	}
	// the node's own binary, built for this machine
	bin := filepath.Join(t.TempDir(), "mesh")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "projectmesh.io/mesh-cli/cmd/mesh")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building mesh: %v\n%s", err, out)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}

	const secret = "mesh-session-e2e-secret-0f9c2a51"
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	h, _ := key.Handoff()
	creds, _ := json.Marshal(relaysidecar.Credentials{Lease: "e2e-relay", Key: h,
		Session: relaysidecar.Token{Value: secret, ExpiresAt: time.Now().Add(time.Hour)}})

	// the node's side of the lease's socket: the tunnel to example.com
	// alone
	dir := sockettest.Dir(t, "rl")
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	tunnel := &egress.Tunnel{Policy: egress.NewPolicy([]string{"example.com"}, nil, nil), Logf: t.Logf}
	node := &relaysidecar.Node{Tunnel: tunnel, Logf: t.Logf,
		Renew: func(context.Context, relaysidecar.RenewRequest) (relaysidecar.Renewal, error) {
			return relaysidecar.Renewal{Session: relaysidecar.Token{Value: secret, ExpiresAt: time.Now().Add(time.Hour)}}, nil
		}}
	if err := node.Listen(filepath.Join(dir, "n")); err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	ctl := filepath.Join(dir, "r")
	if err := os.Mkdir(ctl, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := rt.ChownForRelay(ctx, ctl); err != nil {
		t.Fatal(err)
	}
	defer rt.RemoveTree(context.Background(), dir)

	sb := &Sandbox{Runtime: rt, Config: Config{Image: image, Limits: Limits{CPUs: 1, Memory: "1g", Pids: 512}, Network: Isolated()}}
	sess, err := sb.Open("e2e/relay", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ws := t.TempDir()
	if err := os.Chmod(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	const port = relaysidecar.DefaultPort
	if err := sess.StartRelay(ctx, RelaySpec{Binary: bin, SocketDir: dir, Checkout: ws,
		Args: []string{relaysidecar.Command, "--credentials-dir", RelayCredentialsDir,
			"--node", RelaySocketDir + "/n", "--control", RelaySocketDir + "/r/s",
			"--listen", "127.0.0.1:" + strconv.Itoa(port)}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.InstallRelay(ctx, bytes.NewReader(creds)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(filepath.Join(ctl, "s")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("podman", "logs", RelayName(Pod{Name: sess.PodName()})).CombinedOutput()
			t.Fatalf("the relay sidecar did not start:\n%s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// the sidecar's own side: its user, its tmpfs and its file
	relay := RelayName(Pod{Name: sess.PodName()})
	out, err := exec.CommandContext(ctx, "podman", "exec", relay, "sh", "-c",
		"id -u; stat -c '%a %u' "+RelayCredentialsDir+" "+RelayCredentialsDir+"/"+relaysidecar.CredentialsFile).CombinedOutput()
	if err != nil {
		t.Fatalf("reading the sidecar: %v\n%s", err, out)
	}
	want := fmt.Sprintf("%d\n700 %d\n400 %d", RelayUID, RelayUID, RelayUID)
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("the sidecar's user and credential modes:\n%s\nwant\n%s", got, want)
	}

	probe := func(name, cmd string) string {
		return fmt.Sprintf("if %s >/dev/null 2>&1; then echo 'OK %s'; else echo 'FAIL %s'; fi\n", cmd, name, name)
	}
	proxy := "http://127.0.0.1:" + strconv.Itoa(port)
	var script strings.Builder
	script.WriteString(probe("direct", "curl -sS --max-time 10 --noproxy '*' https://example.com"))
	script.WriteString(probe("allowed", "curl -fsS --max-time 30 -x "+proxy+" https://example.com"))
	script.WriteString(probe("denied", "curl -fsS --max-time 30 -x "+proxy+" https://example.org"))
	script.WriteString(probe("sidecarproc", "grep -qs relay-sidecar /proc/[0-9]*/cmdline"))
	script.WriteString(probe("environ", "grep -qs "+secret+" /proc/[0-9]*/environ"))
	script.WriteString(probe("ownenv", "env | grep -q "+secret))
	script.WriteString(probe("tmpfsmount", "grep -q "+RelayCredentialsDir+" /proc/self/mountinfo"))
	script.WriteString(probe("socketmount", "grep -q "+RelaySocketDir+" /proc/self/mountinfo"))
	script.WriteString(probe("credfile", "cat "+RelayCredentialsDir+"/"+relaysidecar.CredentialsFile))
	script.WriteString(probe("fstoken", "grep -rqs "+secret+" /tmp /run /etc /home \"$HOME\" "+ws))
	script.WriteString(probe("podmansock", "ls /run/podman/podman.sock /var/run/docker.sock /run/user/*/podman/podman.sock"))
	script.WriteString(probe("sameuid", "test \"$(id -u)\" = "+strconv.Itoa(RelayUID)))
	var stdout, stderr bytes.Buffer
	err = sess.RunAgent(ctx, RunSpec{Workspace: ws, Command: []string{"sh", "-c", script.String()}, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatalf("the agent: %v\n%s", err, stderr.String())
	}
	got := map[string]string{}
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		if res, name, ok := strings.Cut(sc.Text(), " "); ok {
			got[name] = res
		}
	}
	for name, w := range map[string]string{
		"direct": "FAIL", "allowed": "OK", "denied": "FAIL",
		"sidecarproc": "FAIL", "environ": "FAIL", "ownenv": "FAIL",
		"tmpfsmount": "FAIL", "socketmount": "FAIL", "credfile": "FAIL", "fstoken": "FAIL",
		"podmansock": "FAIL", "sameuid": "FAIL",
	} {
		if got[name] != w {
			t.Errorf("probe %s = %q, want %s (all: %v)", name, got[name], w, got)
		}
	}
}
