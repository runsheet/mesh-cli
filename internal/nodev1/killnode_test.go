package nodev1

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A node's death -- SIGKILL, an OOM kill, a crash -- never touches a
// lease's egress process or a connection already in flight through it
// (ADR-051 decisions 1, 3 and 5). Every other test in this package
// stands the node's death in with a second in-process *Runtime; this
// one is the real thing: a second OS process, built from the real mesh
// binary's own test harness, genuinely killed with SIGKILL, never
// asked to release anything. It starts a lease's egress process the
// way the node's work loop does, begins an HTTPS transfer through the
// egress process's own lane socket -- the one leaseEgressCmd's --lane
// serves directly, "where a run_suite lane's CONNECT lands", never
// through the node's own forwarding (openLaneTunnel/forwardConnect,
// which are goroutines of the node process and die with it, as they
// should: that forwarding is a convenience for a host reachable only
// while the node runs, not the thing decisions 1, 3 and 5 promise
// survives) -- kills the node process mid-transfer, and finds the
// egress process still alive and the transfer still completing.
func TestANodesDeathNeverTouchesItsLeasesEgressProcess(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("no /proc")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := testMeshBinary(t)

	part2 := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "part1-")
		w.(http.Flusher).Flush()
		<-part2
		io.WriteString(w, "part2")
	}))
	t.Cleanup(upstream.Close)

	const host = "proxy.golang.org"
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
	sock := fields[2]
	t.Cleanup(func() { syscall.Kill(egressPID, syscall.SIGKILL) })

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "CONNECT "+host+":443 HTTP/1.1\r\nHost: "+host+":443\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT through the lane socket: %v, %+v", err, resp)
	}
	tlsConn := tls.Client(&bufferedConn{c, br}, &tls.Config{ServerName: host, InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("the TLS handshake through the lane socket: %v", err)
	}
	io.WriteString(tlsConn, "GET / HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
	tresp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("reading the response headers: %v", err)
	}
	// tresp.Body de-chunks the transfer; the raw bytes on the wire
	// carry chunk framing round the flushed first write.
	first := make([]byte, len("part1-"))
	if _, err := io.ReadFull(tresp.Body, first); err != nil || string(first) != "part1-" {
		t.Fatalf("the first chunk, before the kill: %q, %v", first, err)
	}

	// the transfer is in flight: kill the node outright, the way an OS
	// kill or a crash would, never a drain or a release.
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

	close(part2)
	rest, err := io.ReadAll(tresp.Body)
	if err != nil || string(rest) != "part2" {
		t.Fatalf("the in-flight transfer did not complete after the node died: %q, %v", rest, err)
	}
}
