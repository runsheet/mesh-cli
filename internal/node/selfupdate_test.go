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
	"projectmesh.io/mesh-cli/internal/sandbox"
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
	imageErr error
	pending  bool
	// confirmErrs is every NoteConfirmError reason, in order
	confirmErrs []string
}

// Image is the loop's Image: it records the version and whether the loop
// was draining when the image was ensured.
func (u *fakeUpdater) Image(_ context.Context, version string) (sandbox.Image, error) {
	state := "running"
	if u.loop.Draining() {
		state = "draining"
	}
	u.add("image " + version + " " + state)
	if u.imageErr != nil {
		return sandbox.Image{}, u.imageErr
	}
	return sandbox.Image{Ref: sandbox.DefaultImage(version), Pulled: true}, nil
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

func (u *fakeUpdater) NoteConfirmError(reason string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.confirmErrs = append(u.confirmErrs, reason)
	return nil
}

func (u *fakeUpdater) ConfirmErrs() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.confirmErrs...)
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
	loop.Image = u.Image
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

// (a) A desired version other than the running one is fetched and its
// image ensured, the binary is swapped with a build still running, and
// Run returns ErrRestartForUpdate at once — no drain: the build's
// context is cancelled, so its session detaches for the next process to
// adopt (adr-009).
func TestSelfUpdateSwapsAndExitsWithoutWaiting(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "long-one", 1, false)
	loop, u, release, _ := p.updateLoop("")
	defer close(release)
	// the build runs in the node's context, as every session Run starts
	loop.Tick(loop.Context(context.Background()))
	if loop.Running() != 1 {
		t.Fatalf("running = %d, want 1", loop.Running())
	}
	p.Nodes[p.scopes[0].NodeID]["desiredVersion"] = "v1.1.0"
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("Run = %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run waited for the running build")
	}
	if loop.Draining() {
		t.Fatal("the update drained")
	}
	want := []string{"confirm", "fetch v1.1.0", "image v1.1.0 running", "begin v1.0.0 v1.1.0", "swap mesh v1.1.0 running"}
	if got := u.Events(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events %q, want %q", got, want)
	}
}

// A sandboxed node's sessions end with it (their pod is not yet their
// unit): its update still drains — the running build finishes first —
// then swaps and restarts.
func TestSelfUpdateSandboxedDrainsThenSwapsThenRestarts(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "long-one", 1, false)
	loop, u, release, _ := p.updateLoop("")
	loop.Sandbox = &gateway.SandboxInfo{Mode: SandboxOn}
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
	// the new version's node image is pulled before the drain begins
	want := []string{"confirm", "fetch v1.1.0", "image v1.1.0 running", "begin v1.0.0 v1.1.0", "swap mesh v1.1.0 drained"}
	if got := u.Events(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events %q, want %q", got, want)
	}
}

// A node image that cannot be pulled is the update's error: no drain, no
// swap, the node stays on its binary and claims as before, and the
// heartbeat carries it as updateError.
func TestSelfUpdateFailedImagePullIsAnUpdateError(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	loop, u, release, _ := p.updateLoop("v1.1.0")
	defer close(release)
	u.imageErr = errors.New("node image ghcr.io/runsheet/mesh-node:v1.1.0: pull failed: Error: initializing source: unauthorized")
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	want := []string{"confirm", "fetch v1.1.0", "image v1.1.0 running", "refuse v1.1.0: " + u.imageErr.Error()}
	if got := u.Events(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events %q, want %q", got, want)
	}
	if loop.Draining() || loop.restartRequested() {
		t.Fatalf("draining %v, restart %v after a failed pull", loop.Draining(), loop.restartRequested())
	}
	p.addItem(orgID, "g", "after", 1, false)
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if c := p.CallsNamed("claimWorkItem"); len(c) != 1 || c[0].Input["aggregateId"] != "after" {
		t.Fatalf("claims after the failed pull: %+v", c)
	}
	if got := p.lastHeartbeat()["updateError"]; got != u.imageErr.Error() {
		t.Fatalf("heartbeat updateError = %v", got)
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
	// the build runs in the node's context, as every session Run starts
	loop.Tick(loop.Context(context.Background()))
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
	// a dev build reports its commit rather than dev (version.Resolved): it
	// still does not follow desiredVersion
	for _, v := range []string{"0895d40", "0895d40-dirty"} {
		loop.Version = v
		loop.Tick(context.Background())
		loop.updateWG.Wait()
		if got := u.Events(); len(got) != 1 || loop.Draining() {
			t.Fatalf("sha build %s: events %q, draining %v", v, got, loop.Draining())
		}
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
		c, err := CheckStart(f, exe, selfupdate.DefaultConfirmWithin)
		if err != nil || c.RolledBack || c.Failed != "" {
			t.Fatalf("start: %+v, %v", c, err)
		}
	}
	c, err := CheckStart(f, exe, selfupdate.DefaultConfirmWithin)
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
	c, err = CheckStart(f, exe, selfupdate.DefaultConfirmWithin)
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
	if c, err := CheckStart(f, exe, selfupdate.DefaultConfirmWithin); err != nil || c.RolledBack {
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

// slowGateway is a platform whose heartbeats time out until up: the
// mini at load 200 (goal upgrades-survive-a-slow-platform).
type slowGateway struct {
	Gateway
	mu    sync.Mutex
	up    bool
	beats int
}

func (g *slowGateway) HeartbeatNode(ctx context.Context, nodeID, version string, running int, extras ...gateway.HeartbeatExtras) error {
	g.mu.Lock()
	up := g.up
	g.beats++
	g.mu.Unlock()
	if !up {
		return context.DeadlineExceeded
	}
	return g.Gateway.HeartbeatNode(ctx, nodeID, version, running, extras...)
}

func (g *slowGateway) setUp() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.up = true
}

// confirmClock is a clock tests move by hand, safe beside Run.
type confirmClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *confirmClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *confirmClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// slowConfirmLoop is updateLoop's loop confirming v1.1.0 inside window,
// on a platform whose heartbeats time out, with a fake clock.
func (p *priorityFake) slowConfirmLoop(window time.Duration) (*Loop, *fakeUpdater, *slowGateway, *confirmClock, *strings.Builder) {
	loop, u, _, logs := p.updateLoop("")
	loop.Version = "v1.1.0"
	loop.Confirming = "v1.1.0"
	loop.ConfirmWithin = window
	u.pending = true
	gw := &slowGateway{Gateway: loop.GW}
	loop.GW = gw
	clock := &confirmClock{t: time.Date(2026, 9, 28, 5, 53, 0, 0, time.UTC)}
	loop.Now = clock.Now
	loop.Started = clock.Now()
	return loop, u, gw, clock, logs
}

// A pending update whose heartbeats time out for two minutes, inside a
// five-minute window, keeps trying — each failure logged with its error
// and recorded — and confirms once with the first heartbeat through:
// a slow platform is not a binary that failed.
func TestSlowPlatformConfirmsInsideTheWindow(t *testing.T) {
	p := newPriorityFake(t)
	loop, u, gw, clock, logs := p.slowConfirmLoop(5 * time.Minute)
	for range 4 { // two fake minutes of timeouts, a tick each 30 s
		loop.Tick(context.Background())
		if err := loop.unconfirmedNow(); err != nil {
			t.Fatalf("at %s: %v", clock.Now().Sub(loop.Started), err)
		}
		clock.Add(30 * time.Second)
	}
	if got := u.Events(); len(got) != 0 {
		t.Fatalf("events while the platform is slow: %q", got)
	}
	errs := u.ConfirmErrs()
	if len(errs) == 0 || len(errs) != gw.beats {
		t.Fatalf("NoteConfirmError %q: want one per failed heartbeat", errs)
	}
	for _, e := range errs {
		if e != "context deadline exceeded" {
			t.Fatalf("NoteConfirmError %q", errs)
		}
	}
	failed := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "self-update: confirming v1.1.0: heartbeat") {
			failed++
			if !strings.Contains(line, "context deadline exceeded (the platform is slow); retrying for") {
				t.Fatalf("failed attempt logged as %q", line)
			}
		}
	}
	if failed != gw.beats || !strings.Contains(logs.String(), "retrying for 3m30s more") {
		t.Fatalf("%d failed attempts logged:\n%s", failed, logs)
	}
	gw.setUp()
	// Run ticks at once: the heartbeat goes through and confirms
	loop.Interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for !strings.Contains(strings.Join(u.Events(), "|"), "confirm") {
		select {
		case err := <-done:
			t.Fatalf("Run returned %v before confirming", err)
		case <-deadline:
			t.Fatal("no confirm")
		case <-time.After(time.Millisecond):
		}
	}
	// the window elapses later: a confirmed update never ends Run
	clock.Add(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("Run = %v after a confirm", err)
	}
	confirms := 0
	for _, e := range u.Events() {
		if e == "confirm" {
			confirms++
		}
	}
	if confirms != 1 {
		t.Fatalf("events %q: want Confirm exactly once", u.Events())
	}
	if !strings.Contains(logs.String(), "v1.1.0 is up; update confirmed") {
		t.Fatalf("logs:\n%s", logs)
	}
}

// A window that elapses with every heartbeat timing out ends Run with
// ErrUnconfirmed naming the last error, which NoteConfirmError holds.
func TestSlowPlatformPastTheWindowIsUnconfirmed(t *testing.T) {
	p := newPriorityFake(t)
	loop, u, _, clock, _ := p.slowConfirmLoop(time.Minute)
	loop.Interval = time.Millisecond
	loop.Now = func() time.Time { // each read 10 fake seconds on
		clock.Add(10 * time.Second)
		return clock.Now()
	}
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background()) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end when the window elapsed")
	}
	var ue *UnconfirmedError
	if !errors.Is(err, ErrUnconfirmed) || !errors.As(err, &ue) {
		t.Fatalf("Run = %v, want ErrUnconfirmed", err)
	}
	if !strings.Contains(err.Error(), "v1.1.0 did not confirm within 1m0s: context deadline exceeded") {
		t.Fatalf("Run = %q", err)
	}
	errs := u.ConfirmErrs()
	if len(errs) == 0 || errs[len(errs)-1] != "context deadline exceeded" {
		t.Fatalf("NoteConfirmError %q", errs)
	}
	for _, e := range u.Events() {
		if e == "confirm" {
			t.Fatalf("confirmed without a heartbeat: %q", u.Events())
		}
	}
}

// With no update pending a failed heartbeat is only logged: no
// NoteConfirmError, no Confirm, and no window ends Run.
func TestFailedHeartbeatWithNoPendingUpdate(t *testing.T) {
	p := newPriorityFake(t)
	loop, u, _, clock, logs := p.slowConfirmLoop(time.Minute)
	loop.Confirming = ""
	u.pending = false
	loop.Tick(context.Background())
	clock.Add(time.Hour)
	loop.Tick(context.Background())
	if got := u.ConfirmErrs(); len(got) != 0 {
		t.Fatalf("NoteConfirmError %q", got)
	}
	if got := u.Events(); len(got) != 0 {
		t.Fatalf("events %q", got)
	}
	if err := loop.unconfirmedNow(); err != nil {
		t.Fatalf("unconfirmed with nothing pending: %v", err)
	}
	if !strings.Contains(logs.String(), "heartbeat acme: context deadline exceeded") {
		t.Fatalf("logs:\n%s", logs)
	}
}

// ReleaseUpdater.NoteConfirmError lands in update.json for a rollback to
// name.
func TestReleaseUpdaterNotesConfirmError(t *testing.T) {
	f, exe := updateState(t)
	if err := f.BeginUpdate("v1.0.0", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	u := &ReleaseUpdater{State: f, Exe: exe}
	if err := u.NoteConfirmError("context deadline exceeded"); err != nil {
		t.Fatal(err)
	}
	if s, err := f.Load(); err != nil || s.ConfirmError != "context deadline exceeded" {
		t.Fatalf("state %+v, %v", s, err)
	}
}

// StartLines names the version, the config path and, for a pending
// update, which start this is — the lines node.log gets before anything
// that can block on the platform.
func TestStartLines(t *testing.T) {
	check := StartCheck{From: "72ff910", To: "0a04088", Pending: true, Attempts: 1}
	lines := StartLines("v1.2.3", "/home/mini/.mesh/node.yaml", check)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"v1.2.3", "/home/mini/.mesh/node.yaml", "confirming update 72ff910 → 0a04088"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("StartLines = %q, want it to contain %q", lines, want)
		}
	}
}

// With no update pending, StartLines says so instead.
func TestStartLinesNoUpdatePending(t *testing.T) {
	lines := StartLines("v1.2.3", "/home/mini/.mesh/node.yaml", StartCheck{})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "no update pending") {
		t.Fatalf("StartLines = %q, want \"no update pending\"", lines)
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
