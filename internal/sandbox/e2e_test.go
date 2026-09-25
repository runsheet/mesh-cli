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
	sess, err := sb.Open("e2e/1", "build")
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
	want := map[string]string{"gh": "FAIL", "ghdir": "FAIL", "token": "FAIL", "other": "FAIL", "docker": "FAIL", "https": "OK", "lan": "FAIL", "hostloop": "FAIL", "hostgw": "FAIL"}
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
	for _, name := range env {
		if slices.Contains([]string{"PWD", "SHLVL", "_", "OLDPWD"}, name) {
			continue
		}
		if !slices.Contains(allowed, name) && !strings.HasPrefix(name, ToolEnvPrefix) {
			t.Errorf("env holds %s, which is not allow-listed (all: %v)", name, env)
		}
	}
	if slices.Contains(env, "MESH_DECOY") {
		t.Errorf("MESH_DECOY reached the pod")
	}
}
