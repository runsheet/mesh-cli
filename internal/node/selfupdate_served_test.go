package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// servedLoop is updateLoop's loop at v1.1.0 confirming the update
// v1.0.0 → v1.1.0 begun serving before, on the real ReleaseUpdater, its
// binary swapped so a rollback has a .prev to restore.
func (p *priorityFake) servedLoop(before []string) (*Loop, *selfupdate.StateFile, string, *confirmClock, *strings.Builder) {
	p.t.Helper()
	f, exe := updateState(p.t)
	if err := f.BeginUpdate("v1.0.0", "v1.1.0", before); err != nil {
		p.t.Fatal(err)
	}
	if err := selfupdate.Swap(exe, []byte("v1.1.0")); err != nil {
		p.t.Fatal(err)
	}
	if c, err := CheckStart(f, exe, time.Minute); err != nil || !c.Pending {
		p.t.Fatalf("start: %+v, %v", c, err)
	}
	loop, _, _, logs := p.updateLoop("")
	loop.Version = "v1.1.0"
	loop.Confirming = "v1.1.0"
	loop.ConfirmWithin = time.Minute
	loop.Update = &ReleaseUpdater{State: f, Exe: exe}
	clock := &confirmClock{t: time.Date(2026, 10, 3, 13, 51, 0, 0, time.UTC)}
	loop.Now = clock.Now
	loop.Started = clock.Now()
	return loop, f, exe, clock, logs
}

// servingNone puts the loop on the owner contract serving no org: its
// one heartbeat goes through and answers none (2026-10-03, the
// grapevine canary).
func (p *priorityFake) servingNone(loop *Loop) {
	loop.Owned = &Owned{NodeID: p.scopes[0].NodeID}
	loop.Scopes = nil
}

const fewerGrapevine = "before grapevine, after none"

// A heartbeat that goes through serving none where grapevine was served
// before the swap confirms nothing: the update stays pending, the
// reason is recorded as the confirm error and rides the next heartbeat's
// updateError.
func TestConfirmServingFewerOrgsIsNotConfirmed(t *testing.T) {
	p := newPriorityFake(t)
	loop, f, _, _, logs := p.servedLoop([]string{"grapevine"})
	p.servingNone(loop)
	loop.Tick(context.Background())
	if len(p.CallsNamed("heartbeatNode")) == 0 {
		t.Fatal("no heartbeat went out")
	}
	s, err := f.Load()
	if err != nil || !s.Pending() || !strings.Contains(s.ConfirmError, fewerGrapevine) {
		t.Fatalf("state after the heartbeat: %+v, %v", s, err)
	}
	loop.Tick(context.Background())
	if got, _ := p.lastHeartbeat()["updateError"].(string); !strings.Contains(got, fewerGrapevine) {
		t.Fatalf("updateError %q", got)
	}
	if strings.Contains(logs.String(), "update confirmed") {
		t.Fatalf("confirmed serving none:\n%s", logs)
	}
	if !strings.Contains(logs.String(), "self-update: confirming v1.1.0: serving fewer orgs than before the update: "+fewerGrapevine+"; retrying for") {
		t.Fatalf("logs:\n%s", logs)
	}
	if s, _ := f.Load(); !s.Pending() {
		t.Fatalf("state after the second heartbeat: %+v", s)
	}
}

// Past the window Run ends ErrUnconfirmed naming the orgs, and the
// third start rolls back naming them.
func TestConfirmServingFewerOrgsRollsBack(t *testing.T) {
	p := newPriorityFake(t)
	loop, f, exe, clock, _ := p.servedLoop([]string{"grapevine"})
	p.servingNone(loop)
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
	if !errors.Is(err, ErrUnconfirmed) || !strings.Contains(err.Error(), fewerGrapevine) {
		t.Fatalf("Run = %v", err)
	}
	var c StartCheck
	for range selfupdate.MaxStarts - 1 {
		if c, err = CheckStart(f, exe, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if !c.RolledBack || !strings.Contains(c.Reason, fewerGrapevine) {
		t.Fatalf("start check %+v", c)
	}
}

// The same orgs served before and after confirm.
func TestConfirmSameOrgsConfirms(t *testing.T) {
	p := newPriorityFake(t)
	loop, f, _, _, logs := p.servedLoop([]string{"grapevine"})
	sc := p.scopes[0]
	sc.Login = "grapevine"
	loop.Scopes = Scopes{sc}
	loop.Tick(context.Background())
	if s, err := f.Load(); err != nil || s.Pending() || s.ConfirmedAt == nil {
		t.Fatalf("state: %+v, %v", s, err)
	}
	if !strings.Contains(logs.String(), "v1.1.0 is up; update confirmed") {
		t.Fatalf("logs:\n%s", logs)
	}
	if _, ok := p.lastHeartbeat()["updateError"]; ok {
		t.Fatal("a confirmed update reports an error")
	}
}

// The swap records the orgs served then.
func TestSwapRecordsServedOrgs(t *testing.T) {
	p := newPriorityFake(t)
	loop, _, release, _ := p.updateLoop("v1.1.0")
	defer close(release)
	f, _ := updateState(t)
	u := &recordingUpdater{fakeUpdater: fakeUpdater{loop: loop}, state: f}
	loop.Update = u
	loop.Image = nil
	loop.Interval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := loop.Run(ctx); !errors.Is(err, ErrRestartForUpdate) {
		t.Fatalf("Run = %v", err)
	}
	s, err := f.Load()
	if err != nil || strings.Join(s.Served, ",") != strings.Join(p.scopes.Names(), ",") || len(s.Served) == 0 {
		t.Fatalf("served %q, want %q (%v)", s.Served, p.scopes.Names(), err)
	}
}

// recordingUpdater is fakeUpdater whose Begin writes the state file.
type recordingUpdater struct {
	fakeUpdater
	state *selfupdate.StateFile
}

func (u *recordingUpdater) Begin(from, to string, served []string) error {
	return u.state.BeginUpdate(from, to, served)
}
