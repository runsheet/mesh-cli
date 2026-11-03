package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// fakeSidecarControl stands in for a lease's relay sidecar's control
// socket (relaysidecar.ReplaceCredentialsClient's server side): it
// records every ReplaceCredentialsRequest it is asked to take and
// answers Replaced true, unless refuse names why it should not.
type fakeSidecarControl struct {
	mu      sync.Mutex
	calls   []relaysidecar.ReplaceCredentialsRequest
	refuse  string
	ln      net.Listener
	srv     *http.Server
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
		f.mu.Unlock()
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
