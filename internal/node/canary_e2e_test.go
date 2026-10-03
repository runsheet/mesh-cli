//go:build !windows

package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// The 2026-10-03 canary replayed end to end (goal
// an-update-that-serves-no-org-is-not-confirmed, (5)): the grapevine
// node, per org with node.yaml's orgs [grapevine], is asked for the new
// release against a platform that takes ownerKind and answers no scopes
// (the Fake's nil Answer); its person's orgs are foghornid, then
// grapevine. The swap records what it served; the new process registers
// on the owner contract and either keeps serving grapevine and confirms,
// or comes up serving none and is not confirmed — never confirmed
// serving none.

const (
	canaryOld = "v0.2.263"
	canaryNew = "v0.2.264"
	// canaryFoghorn is the person's first org, the one v0.2.264 sent
	canaryFoghorn = "cccccccc-3333-4333-8333-cccccccccccc"
)

// canaryUpdater is the real ReleaseUpdater on a state file and an
// executable, its download stood in for: Fetch hands the version back
// as the binary, or, when fetched is set, says which version it was
// asked for there and waits for the node to end — a download still
// under way.
type canaryUpdater struct {
	*ReleaseUpdater
	fetched chan string
}

func (u *canaryUpdater) Fetch(ctx context.Context, version string) ([]byte, error) {
	if u.fetched == nil {
		return []byte("mesh " + version), nil
	}
	u.fetched <- version
	<-ctx.Done()
	return nil, ctx.Err()
}

// canary is the platform, the node's Config and its update state, shared
// by the process before the swap and the one after it.
type canary struct {
	f     *gatewaytest.Fake
	gw    *gateway.Client
	cfg   *Config
	state *selfupdate.StateFile
	exe   string
	// perOrg is grapevine's per-org node id, the row v0.2.263 served on
	perOrg string
}

// canaryPoll is nodecmd.go's poll-mode line, which v0.2.264 logged as
// `for , capacity 4` serving none.
func canaryPoll(l *Loop) string {
	return fmt.Sprintf("poll mode: node %s (%s) for %s, capacity %s, every %s", l.Cfg.Name, l.Cfg.Slug, strings.Join(l.CurrentScopes().Names(), ", "), l.Cfg.Capacity, l.Interval)
}

// canarySwap is the process before the swap: v0.2.263 on the per-org
// contract serving grapevine on its per-org row, asked there for
// v0.2.264. It swaps without a drain (ErrRestartForUpdate) and
// update.json records the orgs it served: [grapevine]. It returns the
// old process's log.
func canarySwap(t *testing.T) (*canary, *syncBuf) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	f := seed(t)
	f.Orgs = []map[string]any{
		{"id": canaryFoghorn, "login": "foghornid", "accountId": "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"},
		{"id": orgID, "login": "", "tenantSlug": "grapevine", "accountId": acctID},
	}
	if f.Answer != nil || len(f.AnswerJSON) != 0 {
		t.Fatal("the canary's platform answers no scopes")
	}
	gw := gateway.New(f.URL(), "rs_test")
	if !gw.TakesOwner(context.Background()) {
		t.Fatal("the fake does not take ownerKind")
	}
	cfg := &Config{Slug: "grapevine", Name: "grapevine", OwnerID: f.UserID, Contract: ContractPerOrg, Orgs: []string{"grapevine"}, Capacity: Fixed(4)}
	state, exe := updateState(t)
	c := &canary{f: f, gw: gw, cfg: cfg, state: state, exe: exe}

	scopes, err := ResolveScopes(context.Background(), gw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0].ID != orgID {
		t.Fatalf("v0.2.263 serves %+v, want grapevine alone", scopes)
	}
	c.perOrg = scopes[0].NodeID
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: c.perOrg, OrgID: orgID, OwnerID: f.UserID, Name: cfg.Name, Slug: cfg.Slug,
		Harnesses: []string{"claude-code"}, Capacity: 4, Version: canaryOld}); err != nil {
		t.Fatal(err)
	}
	f.Nodes[c.perOrg]["desiredVersion"] = canaryNew

	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	old := &Loop{GW: gw, Cfg: cfg, Scopes: scopes, Version: canaryOld, Interval: time.Hour, Harness: "claude-code", Log: logger,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Update:   &canaryUpdater{ReleaseUpdater: &ReleaseUpdater{State: state, Exe: exe}}}
	logger.Print(StartLine(cfg, Registered{Scopes: scopes}))
	logger.Print(canaryPoll(old))
	done := make(chan error, 1)
	go func() { done <- old.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("Run = %v, want ErrRestartForUpdate\n%s", err, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("v0.2.263 did not swap:\n%s", logs.String())
	}
	old.updateWG.Wait()
	s, err := state.Load()
	if err != nil || !s.Pending() || s.From != canaryOld || s.To != canaryNew || strings.Join(s.Served, ",") != "grapevine" {
		t.Fatalf("update.json after the swap: %+v, %v; want %s → %s pending, served [grapevine]", s, err, canaryOld, canaryNew)
	}
	return c, logs
}

// newProcess is mesh node start after the swap, on the same Config and
// update.json at v0.2.264: the start check (start 1 of 3), the owner
// registration — the probe, ConfiguredOrg and Resolve as nodecmd.go
// wires them — saved to the Config, and the loop confirming v0.2.264 on
// clock's time. It returns the loop, what Register made, the
// registerNode call it sent and the new process's log.
func (c *canary) newProcess(t *testing.T, clock *confirmClock, u Updater) (*Loop, Registered, gatewaytest.Call, *syncBuf) {
	t.Helper()
	if s, err := CheckStart(c.state, c.exe, time.Minute); err != nil || !s.Pending || s.Attempts != 1 || s.RolledBack {
		t.Fatalf("start check: %+v, %v", s, err)
	}
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	before := len(c.f.CallsNamed("registerNode"))
	reg, err := Register(context.Background(), configuredOrgRegistrar(c.gw, c.cfg), c.cfg, c.cfg.Orgs)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	calls := c.f.CallsNamed("registerNode")[before:]
	if len(calls) != 1 {
		t.Fatalf("the new process's registerNode calls: %+v, want one", calls)
	}
	if reg.Owned == nil || reg.Contract() != ContractOwner {
		t.Fatalf("registered %+v, want the owner contract", reg)
	}
	if err := SaveRegistration(c.cfg, reg, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, line := range RegistrationLines(c.cfg, reg) {
		logger.Print(line)
	}
	loop := &Loop{GW: c.gw, Cfg: c.cfg, Scopes: reg.Scopes, Owned: reg.Owned, Version: canaryNew, Interval: time.Hour, Harness: "claude-code", Log: logger,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Update:   u, Confirming: canaryNew, ConfirmWithin: time.Minute, Now: clock.Now, Started: clock.Now()}
	loop.Reload = OwnedReloader(func() (*Config, error) { return c.cfg, nil })
	loop.TakeAnswer(reg.Answer)
	logger.Print(canaryPoll(loop))
	return loop, reg, calls[0], logs
}

func canaryClock() *confirmClock {
	return &confirmClock{t: time.Date(2026, 10, 3, 13, 51, 0, 0, time.UTC)}
}

// (1) The new process keeps serving grapevine and confirms: its
// registerNode carries grapevine's id, never foghornid's (item 4); the
// nil answer keeps grapevine (item 2); its first heartbeat goes through
// serving what the swap recorded and confirms (item 1). Neither process
// says it serves none.
func TestCanaryReplayKeepsServingAndConfirms(t *testing.T) {
	c, oldLogs := canarySwap(t)
	clock := canaryClock()
	loop, reg, call, logs := c.newProcess(t, clock, &ReleaseUpdater{State: c.state, Exe: c.exe})

	t.Logf("registerNode orgId %v (grapevine %s, foghornid %s)", call.Input["orgId"], orgID, canaryFoghorn)
	if call.Input["orgId"] != orgID {
		t.Fatalf("registerNode orgId %v, want grapevine's %s", call.Input["orgId"], orgID)
	}
	if call.Input["ownerKind"] != "person" {
		t.Fatalf("registerNode %+v, want the owner contract's ownerKind", call.Input)
	}
	start := StartLine(c.cfg, reg)
	t.Logf("start line: %s", start)
	if !strings.HasSuffix(start, "serving grapevine") {
		t.Fatalf("start line %q, want serving grapevine", start)
	}

	loop.Tick(loop.Context(context.Background()))
	loop.wg.Wait()
	loop.updateWG.Wait()

	beats := c.f.CallsNamed("heartbeatNode")
	if len(beats) == 0 || beats[len(beats)-1].Input["aggregateId"] != reg.Owned.NodeID {
		t.Fatalf("heartbeats %+v, want the last under the owned id %s", beats, reg.Owned.NodeID)
	}
	if got := strings.Join(loop.CurrentScopes().Names(), ","); got != "grapevine" {
		t.Fatalf("after the nil heartbeat answer the node serves %q, want grapevine\n%s", got, logs.String())
	}
	s, err := c.state.Load()
	if err != nil || s.Pending() || s.ConfirmedAt == nil {
		t.Fatalf("update.json after the first heartbeat: %+v, %v; want confirmed", s, err)
	}
	out := logs.String()
	t.Logf("new process log:\n%s", out)
	if !strings.Contains(out, canaryNew+" is up; update confirmed") {
		t.Fatalf("no confirm in the new process's log:\n%s", out)
	}
	for name, l := range map[string]string{"old": oldLogs.String(), "new": out} {
		for _, never := range []string{"serving none", "for , capacity"} {
			if strings.Contains(l, never) {
				t.Fatalf("the %s process logged %q:\n%s", name, never, l)
			}
		}
	}
	if _, ok := beats[len(beats)-1].Input["updateError"]; ok {
		t.Fatalf("a confirmed update reports an error: %+v", beats[len(beats)-1].Input)
	}
}

// (2) The new process's registration answers an empty scope list (not
// nil) under a restriction it matches none of, so it comes up serving
// none: the heartbeat goes through but confirms nothing — `before
// grapevine, after none` on update.json and on the next heartbeat's
// updateError — it still reads desiredVersion by its owned id (item 3)
// and hears the downgrade asked there, and past the window Run ends
// ErrUnconfirmed naming the orgs and the third start rolls back naming
// them.
func TestCanaryReplayServingNoneRollsBack(t *testing.T) {
	c, _ := canarySwap(t)
	owned := OwnedNodeID(c.cfg, c.cfg.OwnerKind(), c.f.UserID)
	c.f.Answer = &gateway.NodeAnswer{NodeID: owned, Owner: gateway.NodeOwner{Kind: "person", ID: c.f.UserID, Name: "Chris"}, Scopes: []gateway.ScopeAnswer{}}
	clock := canaryClock()
	u := &canaryUpdater{ReleaseUpdater: &ReleaseUpdater{State: c.state, Exe: c.exe}, fetched: make(chan string, 1)}
	loop, reg, _, logs := c.newProcess(t, clock, u)
	if reg.Answer == nil || reg.Owned.NodeID != owned || len(reg.Scopes) != 0 {
		t.Fatalf("registered %+v, want the owner contract on %s serving none", reg, owned)
	}
	// the heartbeats answer no scopes, as the canary's platform did
	c.f.Answer = nil
	c.f.Nodes[owned]["desiredVersion"] = canaryOld

	ctx := loop.Context(context.Background())
	loop.Tick(ctx)
	beats := c.f.CallsNamed("heartbeatNode")
	if len(beats) == 0 || beats[len(beats)-1].Input["aggregateId"] != owned {
		t.Fatalf("heartbeats %+v, want one through under %s", beats, owned)
	}
	s, err := c.state.Load()
	if err != nil || !s.Pending() || !strings.Contains(s.ConfirmError, fewerGrapevine) {
		t.Fatalf("update.json after the first heartbeat: %+v, %v; want pending with confirmError %q", s, err, fewerGrapevine)
	}
	var read bool
	for _, r := range c.f.ReadsNamed("nodeIndex") {
		read = read || r.Input["id"] == owned
	}
	if !read {
		t.Fatalf("no desiredVersion read on the owned id %s: %+v", owned, c.f.ReadsNamed("nodeIndex"))
	}
	select {
	case v := <-u.fetched:
		if v != canaryOld {
			t.Fatalf("fetching %s, want the downgrade to %s", v, canaryOld)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("a node serving none did not hear the downgrade asked on its own id:\n%s", logs.String())
	}

	loop.Tick(ctx)
	beats = c.f.CallsNamed("heartbeatNode")
	if got, _ := beats[len(beats)-1].Input["updateError"].(string); !strings.Contains(got, fewerGrapevine) {
		t.Fatalf("the last heartbeat's updateError %q, want %q", got, fewerGrapevine)
	}
	if strings.Contains(logs.String(), "update confirmed") {
		t.Fatalf("confirmed serving none:\n%s", logs.String())
	}

	clock.Add(2 * time.Minute)
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background()) }()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not end past the window:\n%s", logs.String())
	}
	loop.updateWG.Wait()
	if !errors.Is(err, ErrUnconfirmed) || !strings.Contains(err.Error(), fewerGrapevine) {
		t.Fatalf("Run = %v, want ErrUnconfirmed naming %q", err, fewerGrapevine)
	}
	t.Logf("Run: %v", err)

	var check StartCheck
	for range selfupdate.MaxStarts - 1 {
		if check, err = CheckStart(c.state, c.exe, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if !check.RolledBack || !strings.Contains(check.Reason, fewerGrapevine) {
		t.Fatalf("start check after %d starts: %+v, want rolled back naming %q", selfupdate.MaxStarts, check, fewerGrapevine)
	}
	t.Logf("rolled back: %s", check.Reason)
}
