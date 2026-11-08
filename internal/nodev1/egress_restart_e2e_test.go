//go:build e2e

package nodev1

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
)

// TestLeaseEgressSurvivesANodeRestartE2E takes
// TestANodesDeathNeverTouchesItsLeasesEgressProcess (killnode_test.go)
// one step further: it is not enough that the egress process a node's
// restart leaves running (ADR-051 decisions 1, 3 and 5-- the real
// trigger is a systemd KillMode=process restart or `mesh node upgrade
// --api v1`, SIGKILL here as the fake node harness already proves it
// survives that) is still *there*; it has to still work. This forces
// the one code path only the node's death can expose: a CONNECT the
// policy refuses writes a line to the egress process's own stderr
// (leaseEgressCmd's logf), which is a pipe (cmd.StderrPipe(), read by
// a goroutine of startEgressProcess's) whose read end died with the
// node that held it open. Before leaseEgressCmd ignored SIGPIPE, that
// write's EPIPE carried the signal's default disposition and killed
// the egress process outright -- so the adopting node's next
// Credentials call, and the next session's request through the lane
// socket, would have found nothing there to adopt. This proves both
// still work: the refused CONNECT does not take the process down, the
// adopting node's Credentials call over the control socket succeeds,
// and a plain request through the lane socket still completes.
func TestLeaseEgressSurvivesANodeRestartE2E(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("no /proc")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := testMeshBinary(t)

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	const host = "proxy.golang.org"
	const disallowedHost = "not-allowed.invalid"
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(),
		fakeNodeEnv+"=1",
		fakeNodeMeshBinaryEnv+"="+bin,
		fakeNodeHostEnv+"="+host,
		TestEgressDialEnv+"="+upstream.Listener.Addr().String(),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	nodeDone := make(chan struct{})
	go func() { cmd.Wait(); close(nodeDone) }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-nodeDone
	})

	lineCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		lineCh <- line
	}()
	var line string
	select {
	case line = <-lineCh:
	case <-time.After(20 * time.Second):
		t.Fatal("the fake node never became ready")
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "READY" {
		t.Fatalf("the fake node's ready line: %q", line)
	}
	egressPID, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("the fake node's egress pid: %q: %v", line, err)
	}
	laneSock := fields[2]
	ctlSock := filepath.Join(filepath.Dir(laneSock), egressControlSocket)
	t.Cleanup(func() { syscall.Kill(egressPID, syscall.SIGKILL) })

	// the node dies outright -- never asked to release or hand
	// anything off, as a systemd KillMode=process restart or `mesh
	// node upgrade --api v1` would leave it.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("killing the fake node: %v", err)
	}
	select {
	case <-nodeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake node did not die")
	}

	if err := syscall.Kill(egressPID, 0); err != nil {
		t.Fatalf("the egress process (pid %d) died with the node: %v", egressPID, err)
	}

	// force the vulnerable path: a CONNECT the policy refuses logs on
	// the egress process's own stderr, now a pipe whose read end died
	// with the node.
	rc, err := net.Dial("unix", laneSock)
	if err != nil {
		t.Fatal(err)
	}
	rc.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(rc, "CONNECT "+disallowedHost+":443 HTTP/1.1\r\nHost: "+disallowedHost+":443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(rc), nil)
	rc.Close()
	if err != nil {
		t.Fatalf("CONNECT to a disallowed host through the lane socket: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT to a disallowed host: status %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(egressPID, 0); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the egress process (pid %d) died logging a refused CONNECT after the node died", egressPID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// the adopting node's own path: Credentials over the control
	// socket succeeds with no error.
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := key.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	client := LeaseEgressClient{Socket: ctlSock}
	credRes, err := client.Credentials(context.Background(), LeaseEgressCredentialsRequest{
		Key:     handoff,
		Session: relaysidecar.Token{Value: "adopted-token"},
	})
	if err != nil {
		t.Fatalf("Credentials over the control socket: %v", err)
	}
	if !credRes.Replaced {
		t.Fatalf("Credentials over the control socket: %+v, want Replaced", credRes)
	}

	// a request through the lane socket still succeeds after adoption.
	c, err := net.Dial("unix", laneSock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "CONNECT "+host+":443 HTTP/1.1\r\nHost: "+host+":443\r\n\r\n")
	br := bufio.NewReader(c)
	resp2, err := http.ReadResponse(br, nil)
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT through the lane socket after adoption: %v, %+v", err, resp2)
	}
	tlsConn := tls.Client(&bufferedConn{c, br}, &tls.Config{ServerName: host, InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("the TLS handshake through the lane socket after adoption: %v", err)
	}
	io.WriteString(tlsConn, "GET / HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
	tresp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("reading the response through the lane socket after adoption: %v", err)
	}
	body, err := io.ReadAll(tresp.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("the response through the lane socket after adoption: %q, %v", body, err)
	}
}
