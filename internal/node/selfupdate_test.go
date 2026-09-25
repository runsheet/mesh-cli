package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// fakeUpdater records what the loop asks of it, in order; at the swap it
// records whether the loop was drained with nothing left running.
type fakeUpdater struct {
	mu       sync.Mutex
	loop     *Loop
	events   []string
	fetchErr error
	swapErr  error
	pending  bool
}

func (u *fakeUpdater) add(e string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.events = append(u.events, e)
}

func (u *fakeUpdater) Events() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.events...)
}

func (u *fakeUpdater) Fetch(_ context.Context, version string) ([]byte, error) {
	u.add("fetch " + version)
	if u.fetchErr != nil {
		return nil, u.fetchErr
	}
	return []byte("mesh " + version), nil
}

func (u *fakeUpdater) Begin(from, to string) error {
	u.add("begin " + from + " " + to)
	return nil
}

func (u *fakeUpdater) Swap(bin []byte) error {
	state := "running"
	if u.loop.drained() {
		state = "drained"
	}
	u.add("swap " + string(bin) + " " + state)
	return u.swapErr
}

func (u *fakeUpdater) Refuse(from, to, reason string) error {
	u.add("refuse " + to + ": " + reason)
	return nil
}

func (u *fakeUpdater) Confirm() (bool, error) {
	u.add("confirm")
	return u.pending, nil
}

// updateLoop is the drain loop of p (builds block until release is
// closed) running v1.0.0, with a fake updater; its node in acme is asked
// to run desired.
func (p *priorityFake) updateLoop(desired string) (*Loop, *fakeUpdater, chan struct{}, *strings.Builder) {
	p.register()
	if desired != "" {
		p.Nodes[p.scopes[0].NodeID]["desiredVersion"] = desired
	}
	loop, logs := p.loop(1)
	release := make(chan struct{})
	loop.Dispatch = func(ctx context.Context, _ gateway.WorkItem) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	loop.Version = "v1.0.0"
	u := &fakeUpdater{loop: loop}
	loop.Update = u
	return loop, u, release, logs
}

func (p *priorityFake) lastHeartbeat() map[string]any {
	p.t.Helper()
	hb := p.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		p.t.Fatal("no heartbeat")
	}
	return hb[len(hb)-1].Input
}

// (a) A desired version other than the running one is fetched, the node
// drains — the running build finishes first — then the binary is swapped
// and Run returns ErrRestartForUpdate.
func TestSelfUpdateDrainsThenSwapsThenRestarts(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "long-one", 1, false)
	loop, u, release, _ := p.updateLoop("")
	loop.Tick(context.Background())
	if loop.Running() != 1 {
		t.Fatalf("running = %d, want 1", loop.Running())
	}
	p.Nodes[p.scopes[0].NodeID]["desiredVersion"] = "v1.1.0"
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("Run returned %v with a build running", err)
	case <-time.After(200 * time.Millisecond):
	}
	if !loop.Draining() {
		t.Fatal("not draining while the update waits")
	}
	for _, e := range u.Events() {
		if strings.HasPrefix(e, "swap") || strings.HasPrefix(e, "begin") {
			t.Fatalf("swapped with a build running: %v", u.Events())
		}
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("Run = %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the update")
	}
	want := []string{"confirm", "fetch v1.1.0", "begin v1.0.0 v1.1.0", "swap mesh v1.1.0 drained"}
	if got := u.Events(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events %q, want %q", got, want)
	}
}

// (b) The desired version is the running one: nothing happens.
func TestSelfUpdateDesiredIsRunning(t *testing.T) {
	p := newPriorityFake(t)
	loop, u, _, _ := p.updateLoop("v1.0.0")
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if got := u.Events(); strings.Join(got, "|") != "confirm" || loop.Draining() {
		t.Fatalf("events %q, draining %v", got, loop.Draining())
	}
}

// (c) No desired version: no drain, no fetch, no swap. Nor for a dev
// build, whatever is asked.
func TestSelfUpdateEmptyDesiredDoesNothing(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "one", 1, false)
	loop, u, release, _ := p.updateLoop("")
	defer close(release)
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if got := u.Events(); strings.Join(got, "|") != "confirm" || loop.Draining() || loop.Running() != 1 {
		t.Fatalf("events %q, draining %v, running %d", got, loop.Draining(), loop.Running())
	}

	p.Nodes[p.scopes[0].NodeID]["desiredVersion"] = "v1.1.0"
	loop.Version = "dev"
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if got := u.Events(); len(got) != 1 || loop.Draining() {
		t.Fatalf("dev build: events %q, draining %v", got, loop.Draining())
	}
}

// (d) A refused release is never swapped in: the node goes on claiming,
// the next heartbeat carries the reason, and the same version is not
// tried again on the next tick — only after an hour, or when the
// platform asks for another.
func TestSelfUpdateRefusedKeepsRunning(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	loop, u, release, _ := p.updateLoop("v1.1.0")
	defer close(release)
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	loop.Now = func() time.Time { return now }
	u.fetchErr = &selfupdate.Error{Kind: selfupdate.ErrSignature, Reason: "SHA256SUMS signature does not verify"}
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	want := []string{"confirm", "fetch v1.1.0", "refuse v1.1.0: SHA256SUMS signature does not verify"}
	if got := u.Events(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events %q, want %q", got, want)
	}
	if loop.Draining() {
		t.Fatal("draining after a refused update")
	}

	p.addItem(orgID, "g", "after", 1, false)
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if c := p.CallsNamed("claimWorkItem"); len(c) != 1 || c[0].Input["aggregateId"] != "after" {
		t.Fatalf("claims after the refusal: %+v", c)
	}
	if got := p.lastHeartbeat()["updateError"]; got != "SHA256SUMS signature does not verify" {
		t.Fatalf("heartbeat updateError = %v", got)
	}
	if n := len(u.Events()); n != len(want) {
		t.Fatalf("the refused version was tried again: %q", u.Events())
	}

	// an hour on it is tried again
	now = now.Add(time.Hour)
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if got := u.Events(); got[len(got)-2] != "fetch v1.1.0" {
		t.Fatalf("not retried after an hour: %q", got)
	}
	// another version is tried at once
	p.Nodes[p.scopes[0].NodeID]["desiredVersion"] = "v1.2.0"
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if got := u.Events(); got[len(got)-2] != "fetch v1.2.0" {
		t.Fatalf("a new desired version was not tried: %q", got)
	}
	// and nothing asked clears the error
	p.Nodes[p.scopes[0].NodeID]["desiredVersion"] = ""
	loop.Tick(context.Background())
	loop.Tick(context.Background())
	if got, ok := p.lastHeartbeat()["updateError"]; ok {
		t.Fatalf("updateError %v with nothing asked", got)
	}
}

// A release whose signature does not verify, run through the real
// ReleaseUpdater, is refused before any drain: the executable on disk is
// unchanged, no .prev is written, and the heartbeat carries the reason.
func TestSelfUpdateBadSignatureLeavesTheExecutable(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	loop, _, release, _ := p.updateLoop("v1.1.0")
	defer close(release)
	state, exe := updateState(t)
	bin := []byte("mesh v1.1.0")
	loop.Update = &ReleaseUpdater{
		Source: assetSource{
			selfupdate.SumsAsset:   []byte("0000  mesh-linux-amd64\n"),
			selfupdate.BundleAsset: []byte("not a bundle"),
			"mesh-linux-amd64":     bin,
		},
		State: state, Exe: exe, GOOS: "linux", GOARCH: "amd64",
	}
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if loop.Draining() {
		t.Fatal("draining after a refused update")
	}
	if b, err := os.ReadFile(exe); err != nil || string(b) != "v1.0.0" {
		t.Fatalf("executable = %q, %v; want it unchanged", b, err)
	}
	if _, err := os.Stat(exe + ".prev"); !os.IsNotExist(err) {
		t.Fatalf(".prev written on a refusal: %v", err)
	}
	loop.Tick(context.Background())
	want := "SHA256SUMS.sigstore.json is not a valid Sigstore bundle"
	if got := p.lastHeartbeat()["updateError"]; got != want {
		t.Fatalf("heartbeat updateError = %v, want %q", got, want)
	}
}

type assetSource map[string][]byte

func (s assetSource) Asset(_ context.Context, version, name string) ([]byte, error) {
	if b, ok := s[name]; ok {
		return b, nil
	}
	return nil, &selfupdate.Error{Kind: selfupdate.ErrMissingAsset, Reason: "release " + version + " has no " + name}
}

// A swap that fails after the drain resumes the node and reports why.
func TestSelfUpdateFailedSwapResumes(t *testing.T) {
	p := newPriorityFake(t)
	loop, u, release, _ := p.updateLoop("v1.1.0")
	defer close(release)
	u.swapErr = errors.New("swap: keep previous: permission denied")
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if loop.Draining() || loop.restartRequested() {
		t.Fatalf("draining %v, restart %v after a failed swap", loop.Draining(), loop.restartRequested())
	}
	if got := loop.UpdateError(); got != "swap: keep previous: permission denied" {
		t.Fatalf("UpdateError = %q", got)
	}
}

// A platform without NodeIndex.desiredVersion is no update, logged once;
// the node goes on heartbeating and listing its nodes.
func TestSelfUpdatePlatformWithoutDesiredVersion(t *testing.T) {
	p := newPriorityFake(t)
	p.UnknownFields["desiredVersion"] = true
	loop, u, _, logs := p.updateLoop("")
	loop.Tick(context.Background())
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if got := strings.Count(logs.String(), "no desired version"); got != 1 {
		t.Fatalf("logged %d times:\n%s", got, logs)
	}
	if got := u.Events(); len(got) != 1 {
		t.Fatalf("events %q", got)
	}
	gw := gateway.New(p.URL(), "rs_test")
	if nodes, err := gw.Nodes(context.Background(), orgID); err != nil || len(nodes) != 1 {
		t.Fatalf("Nodes = %+v, %v", nodes, err)
	}
	if _, err := gw.GetDesiredVersion(context.Background(), p.scopes[0].NodeID); !gateway.IsUnsupported(err) {
		t.Fatalf("GetDesiredVersion error %v, want unsupported", err)
	}
}

// GetDesiredVersion reads the node's field.
func TestGetDesiredVersion(t *testing.T) {
	p := newPriorityFake(t)
	p.register()
	p.Nodes[p.scopes[0].NodeID]["desiredVersion"] = "v1.4.0"
	gw := gateway.New(p.URL(), "rs_test")
	if v, err := gw.GetDesiredVersion(context.Background(), p.scopes[0].NodeID); err != nil || v != "v1.4.0" {
		t.Fatalf("GetDesiredVersion = %q, %v", v, err)
	}
	if v, err := gw.GetDesiredVersion(context.Background(), p.scopes[1].NodeID); err != nil || v != "" {
		t.Fatalf("GetDesiredVersion (none asked) = %q, %v", v, err)
	}
	nodes, err := gw.Nodes(context.Background(), orgID)
	if err != nil || len(nodes) != 1 || nodes[0].DesiredVersion != "v1.4.0" {
		t.Fatalf("Nodes = %+v, %v", nodes, err)
	}
}

func updateState(t *testing.T) (*selfupdate.StateFile, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "mesh")
	if err := os.WriteFile(exe, []byte("v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &selfupdate.StateFile{Path: filepath.Join(dir, "update.json")}, exe
}

// (e) A start that finds two unconfirmed starts before it rolls back to
// .prev and says why; the rolled-back binary reports it on its heartbeat
// and does not try that version again.
func TestCheckStartRollsBackAfterTwoFailedStarts(t *testing.T) {
	f, exe := updateState(t)
	if err := f.BeginUpdate("v1.0.0", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := selfupdate.Swap(exe, []byte("v1.1.0")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		c, err := CheckStart(f, exe)
		if err != nil || c.RolledBack || c.Failed != "" {
			t.Fatalf("start: %+v, %v", c, err)
		}
	}
	c, err := CheckStart(f, exe)
	if err != nil || !c.RolledBack || c.From != "v1.0.0" || c.To != "v1.1.0" {
		t.Fatalf("third start: %+v, %v", c, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "v1.0.0" {
		t.Fatalf("exe = %q after rollback", b)
	}
	const reason = "v1.1.0 did not start; rolled back to v1.0.0"
	if c.Reason != reason || !c.Final || c.String() != "self-update: v1.1.0 failed to start; rolled back to v1.0.0" {
		t.Fatalf("rollback check: %+v", c)
	}

	// the previous binary starts: it reports the rollback and leaves v1.1.0 be
	c, err = CheckStart(f, exe)
	if err != nil || c.RolledBack || c.Failed != "v1.1.0" || !c.Final || c.Reason != reason {
		t.Fatalf("restart after rollback: %+v, %v", c, err)
	}
	p := newPriorityFake(t)
	loop, u, _, _ := p.updateLoop("v1.1.0")
	now := time.Now()
	loop.Now = func() time.Time { return now }
	loop.NoteUpdateFailure(c.Failed, c.Reason, c.Final)
	loop.Tick(context.Background())
	if got := p.lastHeartbeat()["updateError"]; got != reason {
		t.Fatalf("heartbeat updateError = %v", got)
	}
	now = now.Add(3 * time.Hour)
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if got := u.Events(); len(got) != 1 {
		t.Fatalf("a rolled-back version was tried again: %q", got)
	}
}

// (f) The first heartbeat that goes through confirms a pending update:
// the state ends it and later starts count nothing.
func TestFirstHeartbeatConfirmsTheUpdate(t *testing.T) {
	f, exe := updateState(t)
	if err := f.BeginUpdate("v1.0.0", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if c, err := CheckStart(f, exe); err != nil || c.RolledBack {
		t.Fatalf("start: %+v, %v", c, err)
	}
	p := newPriorityFake(t)
	loop, _, _, logs := p.updateLoop("")
	loop.Version = "v1.1.0"
	loop.Update = &ReleaseUpdater{State: f, Exe: exe}
	loop.Tick(context.Background())
	s, err := f.Load()
	if err != nil || s.Pending() || s.ConfirmedAt == nil || s.Attempts != 0 || s.LastError != "" {
		t.Fatalf("state after the first heartbeat: %+v, %v", s, err)
	}
	if !strings.Contains(logs.String(), "v1.1.0 is up; update confirmed") {
		t.Fatalf("logs:\n%s", logs)
	}
	if _, ok := p.lastHeartbeat()["updateError"]; ok {
		t.Fatal("a confirmed update reports an error")
	}
	if n, err := f.NoteStart(); err != nil || n != 0 {
		t.Fatalf("NoteStart after confirm = %d, %v", n, err)
	}
}

// A heartbeat that does not go through confirms nothing.
func TestFailedHeartbeatDoesNotConfirm(t *testing.T) {
	p := newPriorityFake(t)
	loop, u, _, _ := p.updateLoop("")
	for _, sc := range p.scopes {
		delete(p.Nodes, sc.NodeID) // unregistered: the fake refuses the heartbeat
	}
	loop.Tick(context.Background())
	if got := u.Events(); len(got) != 0 {
		t.Fatalf("events %q", got)
	}
}

// ReleaseUpdater refuses a release with an asset missing, and never swaps.
func TestReleaseUpdaterRefusesAMissingAsset(t *testing.T) {
	_, exe := updateState(t)
	u := &ReleaseUpdater{Source: assetSource{}, Exe: exe, GOOS: "linux", GOARCH: "amd64"}
	_, err := u.Fetch(context.Background(), "v1.1.0")
	if !errors.Is(err, selfupdate.ErrMissingAsset) {
		t.Fatalf("Fetch = %v, want ErrMissingAsset", err)
	}
}
