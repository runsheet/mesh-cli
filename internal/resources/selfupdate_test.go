package resources

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeUpdater struct {
	due       string
	installed []string
	err       error
}

func (u *fakeUpdater) Due(context.Context) (string, error) { return u.due, nil }
func (u *fakeUpdater) Install(_ context.Context, v string) error {
	u.installed = append(u.installed, v)
	return u.err
}

type fakeNode struct {
	mu        sync.Mutex
	live      int
	drained   string
	handedOut string
	// endsAfterHandBack: the actors release once handed back
	endsAfterHandBack bool
}

func (n *fakeNode) Drain(why string) { n.mu.Lock(); n.drained = why; n.mu.Unlock() }
func (n *fakeNode) Live() int        { n.mu.Lock(); defer n.mu.Unlock(); return n.live }
func (n *fakeNode) HandBackAll(why string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handedOut = why
	if n.endsAfterHandBack {
		n.live = 0
	}
}

// A node with nothing running drains and installs at once.
func TestSelfUpdateDrainsThenInstalls(t *testing.T) {
	u := &fakeUpdater{due: "v0.3.0"}
	n := &fakeNode{}
	su := &SelfUpdate{Updater: u, Node: n, Every: time.Hour}
	if v := su.Run(context.Background()); v != "v0.3.0" {
		t.Fatalf("Run = %q", v)
	}
	if n.drained == "" || n.handedOut != "" || len(u.installed) != 1 {
		t.Fatalf("drained %q handed back %q installed %v", n.drained, n.handedOut, u.installed)
	}
}

// Leases that end on their own within DrainWait are not handed back.
func TestSelfUpdateWaitsForLeasesToEnd(t *testing.T) {
	n := &fakeNode{live: 2}
	go func() {
		time.Sleep(50 * time.Millisecond)
		n.mu.Lock()
		n.live = 0
		n.mu.Unlock()
	}()
	u := &fakeUpdater{}
	su := &SelfUpdate{Updater: u, Node: n, DrainWait: 5 * time.Second, Poll: 5 * time.Millisecond}
	if err := su.Update(context.Background(), "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	if n.handedOut != "" || len(u.installed) != 1 {
		t.Fatalf("handed back %q, installed %v", n.handedOut, u.installed)
	}
}

// Leases still running after DrainWait are handed back node_update, and
// the install waits for them to release.
func TestSelfUpdateHandsBackWhatStillRuns(t *testing.T) {
	n := &fakeNode{live: 1, endsAfterHandBack: true}
	u := &fakeUpdater{}
	su := &SelfUpdate{Updater: u, Node: n, DrainWait: 30 * time.Millisecond, Poll: 5 * time.Millisecond}
	if err := su.Update(context.Background(), "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	if n.handedOut == "" || len(u.installed) != 1 {
		t.Fatalf("handed back %q, installed %v", n.handedOut, u.installed)
	}
}

// A failed install is reported, and Run goes on looking.
func TestSelfUpdateInstallFails(t *testing.T) {
	u := &fakeUpdater{due: "v0.3.0", err: errors.New("the sums do not verify")}
	su := &SelfUpdate{Updater: u, Node: &fakeNode{}, Every: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if v := su.Run(ctx); v != "" {
		t.Fatalf("Run = %q after a failed install", v)
	}
	if len(u.installed) < 2 {
		t.Fatalf("installs tried %v", u.installed)
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v0.2.634", "v0.2.633", true},
		{"v0.3.0", "v0.2.999", true},
		{"v0.2.633", "v0.2.633", false},
		{"v0.2.632", "v0.2.633", false},
		{"v0.2.634", "dev", false},
		{"v0.2.634", "v0.2.633-0.20261030-abc", false},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
