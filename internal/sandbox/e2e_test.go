package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestProbeSessionE2E runs a real pod on podman and probes it from the
// inside: the operator's gh config, the node's token, another workspace
// and the Docker socket cannot be read; the environment holds only the
// allow-listed names; https://example.com answers; 192.168.1.1 and the
// host do not. It needs podman and the node image (MESH_SANDBOX_IMAGE,
// else mesh-node:dev), so it runs only with MESH_SANDBOX_E2E=1.
func TestProbeSessionE2E(t *testing.T) {
	if os.Getenv("MESH_SANDBOX_E2E") != "1" {
		t.Skip("MESH_SANDBOX_E2E=1 runs a real pod")
	}
	image := os.Getenv("MESH_SANDBOX_IMAGE")
	if image == "" {
		image = DefaultImage("dev")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rt := &Podman{}
	if _, err := rt.Version(ctx); err != nil {
		t.Fatalf("podman: %v", err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// another item's workspace beside this one, with a file in it
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "secret.txt"), []byte("other item"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a service on the host, on every address
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	hostIP := ""
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				hostIP = ipn.IP.String()
				break
			}
		}
	}

	t.Setenv("MESH_DECOY", "decoy")
	t.Setenv("GH_TOKEN", "ghp_operator")
	sb := &Sandbox{Runtime: rt, Config: Config{Image: image, Limits: Limits{CPUs: 1, Memory: "1g", Pids: 512}, Network: DefaultNetwork()}}
	sess, err := sb.Open("e2e/1", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ws := t.TempDir()

	probe := func(name, cmd string) string {
		return fmt.Sprintf("if %s >/dev/null 2>&1; then echo 'OK %s'; else echo 'FAIL %s'; fi\n", cmd, name, name)
	}
	var script strings.Builder
	script.WriteString(probe("gh", "cat "+filepath.Join(userHome, ".config/gh/hosts.yml")))
	script.WriteString(probe("ghdir", "ls "+filepath.Join(userHome, ".config/gh")))
	script.WriteString(probe("token", "cat "+filepath.Join(userHome, ".mesh/token")))
	script.WriteString(probe("other", "cat "+filepath.Join(other, "secret.txt")))
	script.WriteString(probe("docker", "test -e /var/run/docker.sock"))
	script.WriteString(probe("https", "curl -fsS --max-time 20 https://example.com"))
	script.WriteString(probe("lan", "curl -sS --max-time 5 http://192.168.1.1"))
	script.WriteString(probe("hostloop", fmt.Sprintf("curl -sS --max-time 5 http://host.containers.internal:%d", port)))
	script.WriteString(probe("hostgw", fmt.Sprintf("curl -sS --max-time 5 telnet://%s:%d", DNSForward, port)))
	if hostIP != "" {
		script.WriteString(probe("hostip", fmt.Sprintf("curl -sS --max-time 5 telnet://%s:%d", hostIP, port)))
	}
	// the job container holds no GitHub credential (mesh-cli ADR-038):
	// the item's ghs_ token the job's env carried is in neither its env
	// nor any file of its HOME or workspace
	script.WriteString(probe("envtoken", "env | grep -q ghs_"))
	script.WriteString(probe("fstoken", "grep -rqs ghs_ \"$HOME\" "+ws))
	script.WriteString("echo ENV; env | cut -d= -f1\n")

	var out, errb bytes.Buffer
	err = sess.Run(ctx, RunSpec{
		Workspace: ws,
		Env:       []string{"GH_TOKEN=ghs_item", "GH_CONFIG_DIR=/x", "ANTHROPIC_API_KEY=k", "MESH_DECOY=job-decoy"},
		Command:   []string{"sh", "-c", script.String()},
		Stdout:    &out, Stderr: &errb,
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	got := map[string]string{}
	var env []string
	inEnv := false
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "ENV":
			inEnv = true
		case inEnv:
			if line != "" {
				env = append(env, line)
			}
		default:
			if res, name, ok := strings.Cut(line, " "); ok {
				got[name] = res
			}
		}
	}
	want := map[string]string{"envtoken": "FAIL", "fstoken": "FAIL", "gh": "FAIL", "ghdir": "FAIL", "token": "FAIL", "other": "FAIL", "docker": "FAIL", "https": "OK", "lan": "FAIL", "hostloop": "FAIL", "hostgw": "FAIL"}
	if hostIP != "" {
		want["hostip"] = "FAIL"
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("probe %s = %q, want %s (all: %v)", k, got[k], v, got)
		}
	}
	sort.Strings(env)
	// the shell itself sets PWD, SHLVL and _ ; everything else is ours
	allowed := append(append([]string{}, BaseEnv...), AllowedEnv...)
	for _, kv := range LimitEnv(sb.Config.Limits) {
		name, _, _ := strings.Cut(kv, "=")
		allowed = append(allowed, name)
	}
	for _, name := range env {
		if slices.Contains([]string{"PWD", "SHLVL", "_", "OLDPWD"}, name) {
			continue
		}
		if !slices.Contains(allowed, name) && !strings.HasPrefix(name, ToolEnvPrefix) {
			t.Errorf("env holds %s, which is not allow-listed (all: %v)", name, env)
		}
	}
	for _, name := range HeldOutEnv {
		if slices.Contains(env, name) {
			t.Errorf("the job container's env holds %s (HeldOutEnv)", name)
		}
	}
	if slices.Contains(env, "MESH_DECOY") {
		t.Errorf("MESH_DECOY reached the pod")
	}
}

// TestLimitEnvE2E runs a real pod with Limits{CPUs: 2} and reads
// GOMAXPROCS back out of the container's own environment: the tools
// inside see the --cpus limit, not the host's core count. It needs
// podman and the node image, so it runs only with MESH_SANDBOX_E2E=1.
func TestLimitEnvE2E(t *testing.T) {
	if os.Getenv("MESH_SANDBOX_E2E") != "1" {
		t.Skip("MESH_SANDBOX_E2E=1 runs a real pod")
	}
	image := os.Getenv("MESH_SANDBOX_IMAGE")
	if image == "" {
		image = DefaultImage("dev")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rt := &Podman{}
	if _, err := rt.Version(ctx); err != nil {
		t.Fatalf("podman: %v", err)
	}
	sb := &Sandbox{Runtime: rt, Config: Config{Image: image, Limits: Limits{CPUs: 2, Memory: "1g", Pids: 512}, Network: DefaultNetwork()}}
	sess, err := sb.Open("e2e/limitenv", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	err = sess.Run(ctx, RunSpec{
		Workspace: t.TempDir(),
		Command:   []string{"sh", "-c", "env | grep GOMAXPROCS"},
		Stdout:    &out, Stderr: &errb,
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "GOMAXPROCS=2" {
		t.Fatalf("env | grep GOMAXPROCS = %q, want GOMAXPROCS=2", got)
	}
}

// TestServicesSidecarsE2E starts the Postgres fixture (.mesh/services.yml)
// and the Redis fixture (a test workflow's services:) as sidecars in a
// real pod, waits for their health checks, and connects to
// 127.0.0.1:5432 and 127.0.0.1:6379 from inside the agent's container;
// after the session no pod of it is left (`podman pod ls`). It needs
// podman and the node image, so it runs only with MESH_SANDBOX_E2E=1.
func TestServicesSidecarsE2E(t *testing.T) {
	if os.Getenv("MESH_SANDBOX_E2E") != "1" {
		t.Skip("MESH_SANDBOX_E2E=1 runs a real pod")
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
	var svcs []Service
	for _, fixture := range []string{"file-postgres", "workflow-redis"} {
		got, err := Resolve(filepath.Join("testdata", fixture), nil)
		if err != nil {
			t.Fatal(err)
		}
		svcs = append(svcs, got.Services...)
	}
	label := fmt.Sprintf("e2e-services/%d", time.Now().UnixNano())
	sb := &Sandbox{Runtime: rt, Config: Config{Image: image, Limits: Limits{CPUs: 1, Memory: "1g", Pids: 512},
		ServicesLimits: Limits{CPUs: 2, Memory: "2g"}, Network: DefaultNetwork()}}
	sess, err := sb.Open(label, "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sts, err := sess.Start(ctx, svcs, 3*time.Minute)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, st := range sts {
		if !st.Ready {
			t.Fatalf("service %s not ready: %s", st.Name, st.Why)
		}
	}
	var out, errb bytes.Buffer
	script := "for p in 5432 6379; do if timeout 5 bash -c \"exec 3<>/dev/tcp/127.0.0.1/$p\"; then echo \"OK $p\"; else echo \"FAIL $p\"; fi; done\n"
	if err := sess.Run(ctx, RunSpec{Workspace: t.TempDir(), Command: []string{"bash", "-c", script}, Stdout: &out, Stderr: &errb}); err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	for _, want := range []string{"OK 5432", "OK 6379"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("from the agent's container: want %q, got\n%s", want, out.String())
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	ls, err := rt.output(ctx, []string{"pod", "ls", "-q", "--filter", "label=mesh.session=" + label}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ls != "" {
		t.Fatalf("podman pod ls after the session: %s", ls)
	}
}
