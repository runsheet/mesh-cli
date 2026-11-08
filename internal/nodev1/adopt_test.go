package nodev1

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// fakeSidecarControl stands in for a lease's relay sidecar's control
// socket (relaysidecar.ReplaceCredentialsClient's server side): it
// records every ReplaceCredentialsRequest it is asked to take and
// answers Replaced true, unless refuse names why it should not.
type fakeSidecarControl struct {
	mu     sync.Mutex
	calls  []relaysidecar.ReplaceCredentialsRequest
	refuse string
	// block, set, is read (under mu) and waited on before the handler
	// answers a request: a test can hold adoptOne in flight, past the
	// point it recorded its call, to drive the window releaseLeftovers
	// must not race.
	block chan struct{}
	ln    net.Listener
	srv   *http.Server
}

func startFakeSidecarControl(t *testing.T, dir string) *fakeSidecarControl {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, relayControlDir), 0o755); err != nil {
		t.Fatalf("making the sidecar's control dir: %v", err)
	}
	f := &fakeSidecarControl{}
	ln, err := net.Listen("unix", filepath.Join(dir, relayControlDir, relayControlSocket))
	if err != nil {
		t.Fatalf("listening on the fake sidecar's control socket: %v", err)
	}
	f.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc(relaysidecar.ReplaceCredentialsPath, func(w http.ResponseWriter, r *http.Request) {
		var req relaysidecar.ReplaceCredentialsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, req)
		refuse := f.refuse
		block := f.block
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		res := relaysidecar.ReplaceCredentialsResult{Replaced: refuse == ""}
		if refuse != "" {
			res.Refused = refuse
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *fakeSidecarControl) replacements() []relaysidecar.ReplaceCredentialsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relaysidecar.ReplaceCredentialsRequest(nil), f.calls...)
}

// mintLease takes a lease the way adoptHandoffs expects to find one
// already taken: offered to the fake, then taken directly on the API
// client, bypassing Runtime.Run entirely.
func mintLease(t *testing.T, g *rig, mut func(*meshapi.Brief)) (string, *meshapi.Brief) {
	t.Helper()
	g.offer(t, mut)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tk, err := g.rt.API.Take(ctx, meshapi.TakeRequest{NodeID: "test-node", Harnesses: []string{"claude"}})
	if err != nil || tk.Lease == nil || tk.Brief == nil {
		t.Fatalf("taking a lease to seed a hand-off for: %v", err)
	}
	return tk.Lease.LeaseID, tk.Brief
}

func writeHandoffFixture(t *testing.T, g *rig, id string, st *handoffState) {
	t.Helper()
	dir := g.rt.leaseDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("making the lease directory: %v", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("marshalling the hand-off fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, handoffFile), data, 0o600); err != nil {
		t.Fatalf("writing the hand-off fixture: %v", err)
	}
}

func sessionJKTs(t *testing.T, g *rig, leaseID string) []string {
	t.Helper()
	var jkts []string
	for _, c := range g.fake.Calls("session") {
		if c.LeaseID != leaseID {
			continue
		}
		var req meshapi.SessionRequest
		json.Unmarshal(c.Body, &req)
		jkts = append(jkts, req.JKT)
	}
	return jkts
}

// A node that starts and finds a lease's hand-off on disk picks it back
// up: a fresh session (never the hand-off's old key), its sidecar's
// credentials replaced, its room in the scheduler re-taken, and its
// agent output resumed from the offset the hand-off named -- then the
// lease runs on to finish exactly as it would have without the restart.
func TestNodeAdoptsHandoffAtStart(t *testing.T) {
	g := newRig(t, nil)
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 1})

	leaseID, brief := mintLease(t, g, func(b *meshapi.Brief) { b.FinishTool = "submit" })

	// Simulate the pre-upgrade process's own session key: adoption must
	// never reuse this one.
	if _, err := openSession(context.Background(), g.rt.API, leaseID, nil); err != nil {
		t.Fatalf("opening the lease's original session: %v", err)
	}

	sidecarDir := filepath.Join(t.TempDir(), "sidecar")
	sidecar := startFakeSidecarControl(t, sidecarDir)

	if err := os.MkdirAll(g.rt.leaseDir(leaseID), 0o700); err != nil {
		t.Fatalf("making the lease directory: %v", err)
	}
	old, new := []byte("old output\n"), []byte("new output\n")
	if err := os.WriteFile(g.rt.agentOutputPath(leaseID), append(old, new...), 0o600); err != nil {
		t.Fatalf("seeding the lease's agent output: %v", err)
	}
	offset := int64(len(old))

	writeHandoffFixture(t, g, leaseID, &handoffState{
		Brief:             brief,
		SidecarDir:        sidecarDir,
		AgentStdoutOffset: offset,
		Turns:             &handoffTurns{Seen: 3, Given: []int64{1, 2, 3}, Closed: []int64{1}},
		PushHeads:         map[string]string{"origin/main": "deadbeef"},
	})

	var logMu sync.Mutex
	var logs []string
	g.rt.Logf = func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		logMu.Lock()
		logs = append(logs, msg)
		logMu.Unlock()
		t.Log(msg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.rt.adoptHandoffs(ctx)

	nodeSocket := filepath.Join(sidecarDir, relayNodeSocket)
	waitFor(t, "the adopted lease's node socket", func() bool {
		_, err := os.Stat(nodeSocket)
		return err == nil
	})

	waitFor(t, "the scheduler to hold the adopted lease's room", func() bool {
		for _, h := range g.rt.Scheduler.Leases() {
			if h.Owner == leaseID {
				return true
			}
		}
		return false
	})

	// adoptOne reattaches the lease's renewal pull and its Stop gate too,
	// not only its sidecar's credentials and its scheduler hold: the
	// adopted node socket answers both exactly as a fresh start's would.
	var renewal relaysidecar.Renewal
	postNodeSocket(t, nodeSocket, relaysidecar.RenewPath, "{}", &renewal)
	if renewal.Session.Value == "" {
		t.Fatalf("Renew on the adopted node socket answered no session token: %+v", renewal)
	}

	var stop stopAnswer
	postNodeSocket(t, nodeSocket, relaysidecar.StopPath, "", &stop)
	if stop.Decision != "block" {
		t.Fatalf("Stop on the adopted node socket answered %+v before the lease finished, want a block", stop)
	}

	driveObserve(t, nodeSocket, "conform", `{"id":1,"method":"tools/call","params":{"name":"submit"}}`,
		`{"id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)

	waitFor(t, "the adopted lease to finish", func() bool {
		return len(g.fake.Calls("finish")) > 0
	})

	jkts := sessionJKTs(t, g, leaseID)
	if len(jkts) != 2 {
		t.Fatalf("expected 2 session calls for the lease (pre-upgrade and adopted), got %d: %v", len(jkts), jkts)
	}
	if jkts[0] == jkts[1] || jkts[0] == "" || jkts[1] == "" {
		t.Fatalf("adoption reused the hand-off's old DPoP key: %v", jkts)
	}

	if calls := sidecar.replacements(); len(calls) != 1 {
		t.Fatalf("expected exactly one credential replacement, got %d", len(calls))
	}

	logMu.Lock()
	gotOffsetLog := false
	for _, l := range logs {
		if bytes.Contains([]byte(l), []byte("resumed its agent output")) {
			gotOffsetLog = true
		}
	}
	logMu.Unlock()
	if !gotOffsetLog {
		t.Fatalf("no log line about resuming agent output past the saved offset; got %v", logs)
	}

	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes", n)
	}
	if n := len(g.fake.Calls("hand-back")) + len(g.fake.Calls("fail")); n != 0 {
		t.Fatalf("%d hand-backs/fails beside the finish", n)
	}
	if g.fake.Ended(leaseID) != "finish" {
		t.Fatalf("lease ended %q, not finish", g.fake.Ended(leaseID))
	}

	waitFor(t, "the scheduler to release the adopted lease's room", func() bool {
		return len(g.rt.Scheduler.Leases()) == 0
	})

	// A lease whose egress runs as a process of its own (ADR-051
	// decisions 1 and 4) is adopted the same way, plus two more things:
	// adoptOne pushes that process fresh credentials -- a new DPoP key,
	// never the hand-off's old one -- over its control socket, and wires
	// the adopted node socket's CONNECT to forward onto the egress
	// process's own node socket.
	t.Run("with a live egress process", func(t *testing.T) {
		g := newRig(t, nil)
		g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 1})

		leaseID, brief := mintLease(t, g, func(b *meshapi.Brief) { b.FinishTool = "submit" })

		if _, err := openSession(context.Background(), g.rt.API, leaseID, nil); err != nil {
			t.Fatalf("opening the lease's original session: %v", err)
		}
		preUpgrade := sessionJKTs(t, g, leaseID)
		if len(preUpgrade) != 1 || preUpgrade[0] == "" {
			t.Fatalf("expected one pre-upgrade session JKT, got %v", preUpgrade)
		}

		sidecarDir := filepath.Join(t.TempDir(), "sidecar")
		startFakeSidecarControl(t, sidecarDir)

		egressDir := filepath.Join(t.TempDir(), "egress")
		const marker = "through the egress tunnel\n"
		egressProc := startFakeEgressProcess(t, egressDir, marker)

		// egressAlive signals the process group by pid, so the fixture
		// needs a real, alive process of its own -- a throwaway one, with
		// no part in the fake control/node protocol above.
		cmd := exec.Command("sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting a throwaway process to stand in for the egress process: %v", err)
		}
		t.Cleanup(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		})

		writeHandoffFixture(t, g, leaseID, &handoffState{
			Brief:         brief,
			SidecarDir:    sidecarDir,
			EgressPID:     cmd.Process.Pid,
			EgressControl: filepath.Join(egressDir, egressControlSocket),
		})

		g.rt.adoptHandoffs(context.Background())

		nodeSocket := filepath.Join(sidecarDir, relayNodeSocket)
		waitFor(t, "the adopted lease's node socket", func() bool {
			_, err := os.Stat(nodeSocket)
			return err == nil
		})

		waitFor(t, "fresh credentials pushed to the egress process", func() bool {
			return len(egressProc.credentials()) == 1
		})

		calls := egressProc.credentials()
		if len(calls) != 1 {
			t.Fatalf("expected exactly one credentials push to the egress process, got %d", len(calls))
		}
		gotJKT := dpop.Thumbprint(calls[0].Key.JWK)
		if gotJKT == "" || gotJKT == preUpgrade[0] {
			t.Fatalf("the egress process was pushed the hand-off's old key (%q), not a fresh one", gotJKT)
		}

		conn, err := net.Dial("unix", nodeSocket)
		if err != nil {
			t.Fatalf("dialing the adopted lease's node socket: %v", err)
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
			t.Fatalf("writing the CONNECT: %v", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatalf("reading the CONNECT response: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT through the adopted node socket answered %d, want 200", resp.StatusCode)
		}
		got := make([]byte, len(marker))
		if _, err := io.ReadFull(br, got); err != nil {
			t.Fatalf("reading the egress process's marker through the tunnel: %v", err)
		}
		if string(got) != marker {
			t.Fatalf("got %q through the tunnel, want %q", got, marker)
		}
	})
}

// driveObserve POSTs one observed call/answer pair to a listening
// relaysidecar.Node's /relay/observe, as the sidecar itself would once
// the product answered the finish tool.
func driveObserve(t *testing.T, socket, server, request, response string) {
	t.Helper()
	body, err := json.Marshal(relaysidecar.Observed{Server: server, Request: json.RawMessage(request), Response: json.RawMessage(response)})
	if err != nil {
		t.Fatalf("marshalling the observed call: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", socket)
	}}}
	resp, err := client.Post("http://sidecar"+relaysidecar.ObservePath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("posting the observed call: %v", err)
	}
	resp.Body.Close()
}

// postNodeSocket POSTs body (raw JSON, "" for none) to path on socket
// and decodes the JSON answer into out, failing the test on anything
// but a 200.
func postNodeSocket(t *testing.T, socket, path, body string, out any) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", socket)
	}}}
	resp, err := client.Post("http://sidecar"+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("posting to %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s answered %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decoding the answer from %s: %v", path, err)
	}
}

// fakeEgressProcess stands in for a lease's own-process egress (ADR-051
// decisions 1 and 4): its control socket answers
// LeaseEgressCredentialsPath, recording every request, and its node
// socket takes a CONNECT the way the real process's tunnel would,
// answering 200 then writing marker -- what a test dialing the adopted
// lease's own node socket should see forwarded through adoptOne's
// tunnel.
type fakeEgressProcess struct {
	mu     sync.Mutex
	calls  []LeaseEgressCredentialsRequest
	marker string
	ctlSrv *http.Server
	nodeLn net.Listener
}

func startFakeEgressProcess(t *testing.T, dir, marker string) *fakeEgressProcess {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("making the fake egress process's directory: %v", err)
	}
	f := &fakeEgressProcess{marker: marker}

	ctlLn, err := net.Listen("unix", filepath.Join(dir, egressControlSocket))
	if err != nil {
		t.Fatalf("listening on the fake egress process's control socket: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+LeaseEgressCredentialsPath, func(w http.ResponseWriter, r *http.Request) {
		var req LeaseEgressCredentialsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, req)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(LeaseEgressCredentialsResult{Replaced: true})
	})
	f.ctlSrv = &http.Server{Handler: mux}
	go f.ctlSrv.Serve(ctlLn)
	t.Cleanup(func() { f.ctlSrv.Close() })

	nodeLn, err := net.Listen("unix", filepath.Join(dir, egressNodeSocket))
	if err != nil {
		t.Fatalf("listening on the fake egress process's node socket: %v", err)
	}
	f.nodeLn = nodeLn
	go func() {
		for {
			conn, err := nodeLn.Accept()
			if err != nil {
				return
			}
			go f.serveNode(conn)
		}
	}()
	t.Cleanup(func() { nodeLn.Close() })
	return f
}

func (f *fakeEgressProcess) serveNode(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	defer req.Body.Close()
	if req.Method != http.MethodConnect {
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	io.WriteString(conn, f.marker)
}

func (f *fakeEgressProcess) credentials() []LeaseEgressCredentialsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LeaseEgressCredentialsRequest(nil), f.calls...)
}

// A hand-off a node cannot pick back up -- unreadable, or its sidecar
// refuses the node's new credentials -- is handed back node_update
// instead, and its job directory removed, rather than left stuck.
func TestNodeHandsBackUnadoptableHandoff(t *testing.T) {
	t.Run("corrupt JSON", func(t *testing.T) {
		g := newRig(t, nil)
		g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 1})
		job := g.offer(t, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		tk, err := g.rt.API.Take(ctx, meshapi.TakeRequest{NodeID: "test-node", Harnesses: []string{"claude"}})
		cancel()
		if err != nil || tk.Lease == nil {
			t.Fatalf("taking a lease: %v", err)
		}
		leaseID := tk.Lease.LeaseID
		dir := g.rt.leaseDir(leaseID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("making the lease directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, handoffFile), []byte("{ not json"), 0o600); err != nil {
			t.Fatalf("writing the corrupt hand-off: %v", err)
		}

		g.rt.adoptHandoffs(context.Background())

		hb := onlyHandBack(t, g, job)
		if hb.Cause != meshapi.CauseNodeUpdate {
			t.Fatalf("hand-back cause %q, want %q", hb.Cause, meshapi.CauseNodeUpdate)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("the job directory outlived the unreadable hand-off: %v", err)
		}
	})

	t.Run("sidecar refuses", func(t *testing.T) {
		g := newRig(t, nil)
		g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 1})
		leaseID, brief := mintLease(t, g, nil)

		sidecarDir := filepath.Join(t.TempDir(), "sidecar")
		sidecar := startFakeSidecarControl(t, sidecarDir)
		sidecar.mu.Lock()
		sidecar.refuse = "this node is not who it was"
		sidecar.mu.Unlock()

		writeHandoffFixture(t, g, leaseID, &handoffState{Brief: brief, SidecarDir: sidecarDir})

		g.rt.adoptHandoffs(context.Background())

		waitFor(t, "the hand-back for the refused sidecar", func() bool {
			return len(g.fake.Calls("hand-back")) == 1
		})
		var hb meshapi.HandBackRequest
		json.Unmarshal(g.fake.Calls("hand-back")[0].Body, &hb)
		if hb.Cause != meshapi.CauseNodeUpdate {
			t.Fatalf("hand-back cause %q, want %q", hb.Cause, meshapi.CauseNodeUpdate)
		}
		if n := len(g.fake.Calls("finish")) + len(g.fake.Calls("fail")); n != 0 {
			t.Fatalf("%d finishes/fails beside the hand-back", n)
		}
		waitFor(t, "the lease's job directory to be removed", func() bool {
			_, err := os.Stat(g.rt.leaseDir(leaseID))
			return os.IsNotExist(err)
		})
	})
}

// With no leases directory at all -- an ordinary start, nothing left
// running by an earlier process -- adoptHandoffs still logs its one
// aggregate line, "adopted 0 of 0", rather than nothing: a start that
// found no hand-offs is as much a fact worth a line in node.log as one
// that found several.
func TestAdoptHandoffsLogsAggregateCountWithNone(t *testing.T) {
	g := newRig(t, nil)

	var logMu sync.Mutex
	var logs []string
	g.rt.Logf = func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		logMu.Lock()
		logs = append(logs, msg)
		logMu.Unlock()
		t.Log(msg)
	}

	g.rt.adoptHandoffs(context.Background())

	logMu.Lock()
	defer logMu.Unlock()
	assertOneAggregateAdoptLine(t, logs, 0, 0)
}

// With a mix of hand-offs on disk -- one a node can adopt, one it
// cannot -- adoptHandoffs logs exactly one aggregate line naming how
// many of how many it adopted, never one line per lease: "adopted 1 of
// 2 hand-off(s) at start".
func TestAdoptHandoffsLogsAggregateCountWithHandoffs(t *testing.T) {
	g := newRig(t, nil)

	adoptableID, brief := mintLease(t, g, func(b *meshapi.Brief) { b.FinishTool = "submit" })
	sidecarDir := filepath.Join(t.TempDir(), "sidecar")
	sidecar := startFakeSidecarControl(t, sidecarDir)
	block := make(chan struct{})
	sidecar.mu.Lock()
	sidecar.block = block
	sidecar.mu.Unlock()
	t.Cleanup(func() { close(block) })
	writeHandoffFixture(t, g, adoptableID, &handoffState{Brief: brief, SidecarDir: sidecarDir})

	corruptID, _ := mintLease(t, g, nil)
	corruptDir := g.rt.leaseDir(corruptID)
	if err := os.MkdirAll(corruptDir, 0o700); err != nil {
		t.Fatalf("making the corrupt lease's directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, handoffFile), []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("writing the corrupt hand-off: %v", err)
	}

	var logMu sync.Mutex
	var logs []string
	g.rt.Logf = func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		logMu.Lock()
		logs = append(logs, msg)
		logMu.Unlock()
		t.Log(msg)
	}

	g.rt.adoptHandoffs(context.Background())

	waitFor(t, "the corrupt hand-off's hand-back", func() bool {
		return len(g.fake.Calls("hand-back")) == 1
	})

	logMu.Lock()
	defer logMu.Unlock()
	assertOneAggregateAdoptLine(t, logs, 1, 2)
}

// assertOneAggregateAdoptLine fails unless logs holds exactly one line
// counting hand-offs adopted at start, naming n of m.
func assertOneAggregateAdoptLine(t *testing.T, logs []string, n, m int) {
	t.Helper()
	want := fmt.Sprintf("nodev1: adopted %d of %d hand-off(s) at start", n, m)
	count := 0
	for _, l := range logs {
		if l == want {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d occurrence(s) of %q, want exactly 1; logs: %v", count, want, logs)
	}
}

// A lease adoptHandoffs is adopting survives releaseLeftovers, which
// Run calls right behind it, even while adoptOne is still waiting on
// the sidecar's control socket -- still short of r.leases.Put -- the
// exact on-disk state and ordering Upgrade and Run leave: actor.json
// unchanged (Running, never reported) beside a fresh handoff.json.
// Before the fix, adoptHandoffs gave releaseLeftovers nothing to tell
// an adopting lease apart from a truly abandoned one, and
// releaseLeftovers -- a synchronous, local-filesystem-only pass with
// no reason to wait on anything -- read that same actor.json first and
// handed the lease back node_restart, deleting its directory out from
// under the goroutine still adopting it.
func TestAdoptedLeaseSurvivesReleaseLeftovers(t *testing.T) {
	// never taken through Run's own take loop -- mintLease bypasses it --
	// so this stub's Run is never called; Run still needs a non-nil
	// Session to start at all.
	g, _ := actorRig(t, &stubSession{})

	leaseID, brief := mintLease(t, g, func(b *meshapi.Brief) { b.FinishTool = "submit" })

	sidecarDir := filepath.Join(t.TempDir(), "sidecar")
	sidecar := startFakeSidecarControl(t, sidecarDir)
	block := make(chan struct{})
	sidecar.mu.Lock()
	sidecar.block = block
	sidecar.mu.Unlock()

	dir := g.rt.leaseDir(leaseID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("making the lease directory: %v", err)
	}
	// the state Upgrade leaves a non-intercepting lease in: actor.json
	// unchanged, still Running and not yet reported, its pod, sidecar
	// and agent never stopped.
	rec := actor.Record{LeaseID: leaseID, JobID: brief.JobID, Kind: brief.Kind, State: actor.Running, Since: time.Now(), Taken: time.Now(), PID: 1}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshalling actor.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, actor.FileName), data, 0o600); err != nil {
		t.Fatalf("writing actor.json: %v", err)
	}

	writeHandoffFixture(t, g, leaseID, &handoffState{Brief: brief, SidecarDir: sidecarDir})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()

	// adoptOne is now blocked inside the sidecar's control handler, past
	// the point it recorded its call to the fake: releaseLeftovers, run
	// synchronously by Run right behind adoptHandoffs, has already had
	// its one chance to race it by now.
	waitFor(t, "adoptOne's credential replacement to reach the sidecar", func() bool {
		return len(sidecar.replacements()) > 0
	})

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("releaseLeftovers removed the adopted lease's directory while adoption was still in flight: %v", err)
	}
	if n := len(g.fake.Calls("hand-back")); n != 0 {
		t.Fatalf("releaseLeftovers handed the adopted lease back while it was still being adopted: %d hand-back(s)", n)
	}

	close(block)

	nodeSocket := filepath.Join(sidecarDir, relayNodeSocket)
	waitFor(t, "the adopted lease's node socket", func() bool {
		_, err := os.Stat(nodeSocket)
		return err == nil
	})
	waitFor(t, "the scheduler to hold the adopted lease's room", func() bool {
		for _, h := range g.rt.Scheduler.Leases() {
			if h.Owner == leaseID {
				return true
			}
		}
		return false
	})

	driveObserve(t, nodeSocket, "conform", `{"id":1,"method":"tools/call","params":{"name":"submit"}}`,
		`{"id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)

	waitFor(t, "the adopted lease to finish", func() bool {
		return len(g.fake.Calls("finish")) > 0
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop once its context was cancelled")
	}

	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes", n)
	}
	if n := len(g.fake.Calls("hand-back")) + len(g.fake.Calls("fail")); n != 0 {
		t.Fatalf("%d hand-backs/fails beside the one finish", n)
	}
	if g.fake.Ended(leaseID) != "finish" {
		t.Fatalf("lease ended %q, not finish", g.fake.Ended(leaseID))
	}

	waitFor(t, "the job directory to be removed once the adopted lease released", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	})
}
