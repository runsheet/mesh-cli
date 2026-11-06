//go:build !windows

package commands

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/nodev1"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/sockettest"
)

// podLeaseSession is a minimal nodev1.Session for a pod-mode lease: it
// starts the lease's agent on the fake sandbox runtime and then blocks
// until its context ends, the way a real agent container runs until the
// node stops it.
type podLeaseSession struct {
	sb     *sandbox.Fake
	mu     sync.Mutex
	starts int
}

func (s *podLeaseSession) Run(ctx context.Context, spec nodev1.SessionSpec) (nodev1.SessionResult, error) {
	pod := sandbox.Pod{Name: spec.Pod}
	agent := sandbox.AgentName(pod)
	if err := s.sb.StartAgent(ctx, pod, sandbox.AgentSpec{Name: agent, Image: "mesh-agent"}); err != nil {
		return nodev1.SessionResult{}, err
	}
	s.mu.Lock()
	s.starts++
	s.mu.Unlock()
	<-ctx.Done()
	return nodev1.SessionResult{SessionID: "sess-pod"}, ctx.Err()
}

func (s *podLeaseSession) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// stubLatchkey answers a lease session's token trade with an opaque
// token and every other route with an empty object: the relay sidecar
// path needs a session token to start, nothing more of Latchkey here.
func stubLatchkey(t *testing.T, fake *meshapitest.Fake) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth/session-token/request" {
			mu.Lock()
			n++
			tok := fmt.Sprintf("mst_%d", n)
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"value": tok, "expires_in": 600, "token_type": "DPoP"})
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	fake.Session = func(leaseID, jkt string) (*meshapi.SessionCredential, int) {
		mu.Lock()
		n++
		sid := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		mu.Unlock()
		return &meshapi.SessionCredential{
			SessionID: sid, RequestURL: srv.URL + "/oauth/session-token/request?session=" + sid + "&v=1",
			RequestToken: "lksr_" + sid, ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
		}, http.StatusCreated
	}
}

// startAdoptControlFake stands in for a lease's relay sidecar's control
// socket (relaysidecar.ReplaceCredentialsClient's server side): it always
// answers Replaced true, which is all a node's own `waitListening` check
// at lease start, and later its ReplaceCredentials call at adoption, ask
// of it.
func startAdoptControlFake(t *testing.T, dir string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "r"), 0o755); err != nil {
		return err
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "r", "s"))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc(relaysidecar.ReplaceCredentialsPath, func(w http.ResponseWriter, r *http.Request) {
		var req relaysidecar.ReplaceCredentialsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(relaysidecar.ReplaceCredentialsResult{Replaced: true})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return nil
}

// driveAdoptObserve POSTs one observed call/answer pair to a listening
// relaysidecar.Node's /relay/observe, as a lease's relay sidecar would
// once the product answered the finish tool.
func driveAdoptObserve(t *testing.T, socket, server, request, response string) {
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

// TestV1UpgradeAdoptsSidecarLeases starts two real mesh node run --api v1
// --sandbox --relay-sidecar processes (two nodev1.Runtimes, run
// in-process, over one shared WorkRoot the way adr-044's hand-off and
// adoption are wired between two real OS processes) with two running
// leases, signals the first the way `mesh node upgrade` does, and the
// second process adopts both: neither lease is ever cancelled in the
// first process's log, and the second process's log shows both adopted.
// An adopted lease is then let finish after the swap: its job is
// reported finished to the fake Mesh API exactly once, and its combined
// pre-upgrade + post-upgrade agent output is in order with no
// duplication or gap at the hand-off seam.
func TestV1UpgradeAdoptsSidecarLeases(t *testing.T) {
	fake := meshapitest.NewFake(t)
	mesh := httptest.NewServer(fake)
	t.Cleanup(mesh.Close)
	stubLatchkey(t, fake)

	_, nodeKey1, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	workRoot := t.TempDir()
	socketRoot := sockettest.Dir(t, "lease")

	// Both runtimes run in this process, so the first one's goroutines
	// outlive its stubbed exit; none may log once the test has returned.
	var quiet atomic.Bool
	defer quiet.Store(true)

	sb := &sandbox.Fake{}
	sb.Agent = func(ctx context.Context, _ sandbox.AgentSpec) (sandbox.Exit, error) {
		<-ctx.Done()
		return sandbox.Exit{}, nil
	}
	pods := &sandbox.Sandbox{Runtime: sb, Logf: func(format string, args ...any) {
		if !quiet.Load() {
			t.Logf(format, args...)
		}
	}, Config: sandbox.Config{
		Image:          "mesh-node:test",
		Limits:         sandbox.Limits{CPUs: 2, Memory: "6g", Pids: 4096},
		ServicesLimits: sandbox.Limits{CPUs: 2, Memory: "4g"},
		Registries:     sandbox.DefaultRegistries,
		ServiceTimeout: 5 * time.Second,
	}}

	sess1 := &podLeaseSession{sb: sb}
	var logMu1 sync.Mutex
	var logs1 []string
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	rt1 := &nodev1.Runtime{
		SigningKey: nodeKey1,
		API:        &meshapi.Client{BaseURL: mesh.URL, Token: func(context.Context) (string, error) { return "machine", nil }},
		Node: meshapi.RegisterRequest{
			Name: "test-node", Version: "v0.0.0-test",
			CapacityTotal: meshapi.Capacity{CPUs: 4, MemGB: 8, Slots: 2},
			SigningKey:    "ed25519:AAAA",
		},
		WorkRoot:   workRoot,
		SocketRoot: socketRoot,
		MeshBinary: "/usr/local/bin/mesh",
		Session:    sess1,
		Logf: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			logMu1.Lock()
			logs1 = append(logs1, msg)
			logMu1.Unlock()
			if !quiet.Load() {
				t.Log("rt1: " + msg)
			}
		},
		Backoff:       10 * time.Millisecond,
		Actors:        true,
		Sandbox:       sb,
		Pods:          pods,
		RelaySidecar:  true,
		PodWatchEvery: 10 * time.Millisecond,
		Scheduler:     scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 2}),
	}

	briefMut := func(b *meshapi.Brief) {
		b.Pods = []json.RawMessage{json.RawMessage(`{"name":"postgres","image":"postgres:17-alpine","env":["POSTGRES_PASSWORD=mysecret"]}`)}
		b.Git = nil
		b.CredentialRef = ""
		b.Access = nil
		b.FinishTool = "submit"
		b.FinishSchema = nil
	}
	job1 := fake.Offer(briefMut)
	job2 := fake.Offer(briefMut)

	// Watch the shared socket root for each lease's own socket
	// directory (its exact name depends on the node's own id, set only
	// once registration happens, so it cannot be predicted ahead of
	// time) and fake its relay sidecar's control socket the moment it
	// appears -- the same directory a hand-off names verbatim, so this
	// one fake serves both the lease's own start-up and, later, the
	// second process's adoption.
	var dirMu sync.Mutex
	var sidecarDirs []string
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go func() {
		seen := map[string]bool{}
		for {
			select {
			case <-watchCtx.Done():
				return
			default:
			}
			entries, _ := os.ReadDir(socketRoot)
			for _, e := range entries {
				if !e.IsDir() || seen[e.Name()] {
					continue
				}
				seen[e.Name()] = true
				dir := filepath.Join(socketRoot, e.Name())
				if err := startAdoptControlFake(t, dir); err != nil {
					t.Logf("starting the fake control socket at %s: %v", dir, err)
					continue
				}
				dirMu.Lock()
				sidecarDirs = append(sidecarDirs, dir)
				dirMu.Unlock()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	dirsSoFar := func() []string {
		dirMu.Lock()
		defer dirMu.Unlock()
		return append([]string(nil), sidecarDirs...)
	}

	done1 := make(chan error, 1)
	go func() { done1 <- rt1.Run(ctx1) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		if st := rt1.Status(); len(st.Leases) == 2 && sess1.started() == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the two leases never both started their agents (%d started): %+v", sess1.started(), rt1.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}

	lease1 := fake.LeaseOf(job1)
	lease2 := fake.LeaseOf(job2)
	if lease1 == "" || lease2 == "" || lease1 == lease2 {
		t.Fatalf("lease ids %q, %q", lease1, lease2)
	}

	pre1 := []byte("pre-upgrade output for lease 1\n")
	pre2 := []byte("pre-upgrade output for lease 2\n")
	leaseOutput := func(id string) string { return filepath.Join(workRoot, "leases", id, "agent-output.log") }
	if err := os.WriteFile(leaseOutput(lease1), pre1, 0o600); err != nil {
		t.Fatalf("writing lease 1's pre-upgrade agent output: %v", err)
	}
	if err := os.WriteFile(leaseOutput(lease2), pre2, 0o600); err != nil {
		t.Fatalf("writing lease 2's pre-upgrade agent output: %v", err)
	}

	state := node.RunState{Dir: t.TempDir()}
	pid := os.Getpid()
	if err := v1Started(state, pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v1Stopped(state, pid) })

	var exitMu sync.Mutex
	var exits []int
	origExit := nodev1.UpgradeExit
	nodev1.UpgradeExit = func(code int) { exitMu.Lock(); exits = append(exits, code); exitMu.Unlock() }
	defer func() { nodev1.UpgradeExit = origExit }()
	exitCount := func() int { exitMu.Lock(); defer exitMu.Unlock(); return len(exits) }

	stop := onRestartSignal(func() {
		if takeV1UpgradeAsk(state) {
			rt1.UpgradeSignal("`mesh node upgrade` asked")
		}
	})
	defer stop()

	var out strings.Builder
	if err := (v1Service{}).Restart(context.Background(), state, &out); err != nil {
		t.Fatalf("v1Service.Restart: %v", err)
	}

	deadline = time.Now().Add(15 * time.Second)
	for exitCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := exitCount(); n != 1 {
		t.Fatalf("UpgradeExit calls %d, want exactly 1", n)
	}

	// Check 1: neither lease is ever cancelled in rt1's own log, and
	// nothing is reported to Mesh yet -- the upgrade should have left
	// both running, their state handed off instead.
	logMu1.Lock()
	rt1Logs := append([]string(nil), logs1...)
	logMu1.Unlock()
	for _, l := range rt1Logs {
		if strings.Contains(l, "cancelled") || strings.Contains(l, "handed back") {
			t.Fatalf("rt1 cancelled or handed back a lease on upgrade: %q", l)
		}
	}
	if n := len(fake.Calls("finish")) + len(fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d calls to Mesh right after the upgrade signal, want none: the upgrade should leave both leases running", n)
	}

	// Each lease's hand-off names its relay sidecar's socket directory
	// and how far into its agent output the first process had read.
	readHandoff := func(id string) (dir string, offset int64) {
		b, err := os.ReadFile(filepath.Join(workRoot, "leases", id, "handoff.json"))
		if err != nil {
			t.Fatalf("lease %s left no hand-off: %v", id, err)
		}
		var h struct {
			SidecarDir        string `json:"sidecarDir"`
			AgentStdoutOffset int64  `json:"agentStdoutOffset"`
		}
		if err := json.Unmarshal(b, &h); err != nil {
			t.Fatalf("lease %s's hand-off: %v", id, err)
		}
		return h.SidecarDir, h.AgentStdoutOffset
	}
	dir1, off1 := readHandoff(lease1)
	dir2, off2 := readHandoff(lease2)
	if off1 != int64(len(pre1)) || off2 != int64(len(pre2)) {
		t.Fatalf("hand-off offsets %d, %d, want %d, %d: the first process's read point", off1, off2, len(pre1), len(pre2))
	}
	leaseDirs := []string{dir1, dir2}
	for _, d := range leaseDirs {
		deadline = time.Now().Add(15 * time.Second)
		for !containsString(dirsSoFar(), d) {
			if time.Now().After(deadline) {
				t.Fatalf("lease sidecar dir %s has no fake control socket (dirs %v)", d, dirsSoFar())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	post1 := []byte("post-upgrade output for lease 1\n")
	post2 := []byte("post-upgrade output for lease 2\n")
	appendFile := func(path string, b []byte) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("appending to %s: %v", path, err)
		}
		defer f.Close()
		if _, err := f.Write(b); err != nil {
			t.Fatalf("appending to %s: %v", path, err)
		}
	}
	appendFile(leaseOutput(lease1), post1)
	appendFile(leaseOutput(lease2), post2)

	_, nodeKey2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var logMu2 sync.Mutex
	var logs2 []string
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	rt2 := &nodev1.Runtime{
		SigningKey: nodeKey2,
		API:        &meshapi.Client{BaseURL: mesh.URL, Token: func(context.Context) (string, error) { return "machine", nil }},
		Node: meshapi.RegisterRequest{
			Name: "test-node", Version: "v0.0.0-test",
			CapacityTotal: meshapi.Capacity{CPUs: 4, MemGB: 8, Slots: 2},
			SigningKey:    "ed25519:BBBB",
		},
		WorkRoot:   workRoot,
		SocketRoot: socketRoot,
		MeshBinary: "/usr/local/bin/mesh",
		Session:    &podLeaseSession{sb: sb},
		Logf: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			logMu2.Lock()
			logs2 = append(logs2, msg)
			logMu2.Unlock()
			if !quiet.Load() {
				t.Log("rt2: " + msg)
			}
		},
		Backoff:       10 * time.Millisecond,
		Actors:        true,
		Sandbox:       sb,
		Pods:          pods,
		RelaySidecar:  true,
		PodWatchEvery: 10 * time.Millisecond,
		Scheduler:     scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 2}),
	}

	done2 := make(chan error, 1)
	go func() { done2 <- rt2.Run(ctx2) }()

	adoptedCount := func() int {
		logMu2.Lock()
		defer logMu2.Unlock()
		n := 0
		for _, l := range logs2 {
			if strings.Contains(l, "adopted at start") {
				n++
			}
		}
		return n
	}
	deadline = time.Now().Add(15 * time.Second)
	for adoptedCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := adoptedCount(); n != 2 {
		logMu2.Lock()
		t.Fatalf("rt2 adopted %d lease(s), want 2; logs: %v", n, logs2)
		logMu2.Unlock()
	}

	if st := rt2.Status(); len(st.Leases) != 2 {
		t.Fatalf("rt2's status holds %d lease(s) after adoption, want 2: %+v", len(st.Leases), st)
	}
	logMu2.Lock()
	rt2Logs := append([]string(nil), logs2...)
	logMu2.Unlock()
	for _, want := range []string{
		fmt.Sprintf("lease %s: resumed its agent output, %d byte(s) past offset %d", lease1, len(post1), len(pre1)),
		fmt.Sprintf("lease %s: resumed its agent output, %d byte(s) past offset %d", lease2, len(post2), len(pre2)),
	} {
		if !containsSubstring(rt2Logs, want) {
			t.Fatalf("rt2 never logged %q: the tail must resume exactly at the hand-off seam; logs: %v", want, rt2Logs)
		}
	}
	// The combined output, read while the adopted leases still run (their
	// lease dirs go at release): pre-upgrade then post-upgrade, once each.
	for _, c := range []struct {
		id        string
		pre, post []byte
	}{{lease1, pre1, post1}, {lease2, pre2, post2}} {
		got, err := os.ReadFile(leaseOutput(c.id))
		if err != nil {
			t.Fatalf("reading lease %s's agent output: %v", c.id, err)
		}
		if want := append(append([]byte{}, c.pre...), c.post...); !bytes.Equal(got, want) {
			t.Fatalf("lease %s's agent output = %q, want %q (no gap or duplication at the hand-off seam)", c.id, got, want)
		}
	}

	for _, dir := range leaseDirs {
		socket := filepath.Join(dir, "n")
		socketDeadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(socket); err == nil {
				break
			}
			if time.Now().After(socketDeadline) {
				t.Fatalf("the adopted lease's node socket never appeared at %s", socket)
			}
			time.Sleep(10 * time.Millisecond)
		}
		driveAdoptObserve(t, socket, "conform",
			`{"id":1,"method":"tools/call","params":{"name":"submit"}}`,
			`{"id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
	}

	deadline = time.Now().Add(15 * time.Second)
	for len(fake.Calls("finish")) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Check 2: each job is reported finished exactly once, and the
	// combined pre-upgrade + post-upgrade agent output is in order with
	// no duplication or gap at the hand-off seam.
	if n := len(fake.Calls("finish")); n != 2 {
		t.Fatalf("%d finishes, want exactly 2 (one per adopted lease)", n)
	}
	if n := len(fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d hand-backs, want none: both leases should have been adopted and finished", n)
	}
	seenLeases := map[string]int{}
	for _, c := range fake.Calls("finish") {
		seenLeases[c.LeaseID]++
	}
	if seenLeases[lease1] != 1 || seenLeases[lease2] != 1 {
		t.Fatalf("finish calls by lease %+v, want exactly one each for %s and %s", seenLeases, lease1, lease2)
	}

	cancel1()
	select {
	case <-done1:
	case <-time.After(10 * time.Second):
		t.Log("rt1 did not stop promptly after its context was cancelled")
	}
	cancel2()
	select {
	case <-done2:
	case <-time.After(10 * time.Second):
		t.Log("rt2 did not stop promptly after its context was cancelled")
	}
}

func containsString(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func containsSubstring(xs []string, sub string) bool {
	for _, s := range xs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
