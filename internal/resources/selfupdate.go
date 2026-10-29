package resources

// Self-update (ADR-044, Mesh v1 PR 3.4): a resource actor that keeps a
// v1 node on the newest release. It checks every Every; when a newer
// version is due it drains the node (no more takes), waits up to
// DrainWait for the leases running to end on their own, hands back what
// is still running with cause node_update — which spends no attempt and
// keeps each job's place in the queue — waits for those actors to
// release, installs the release, and asks the node to restart into it.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Self-update defaults.
const (
	DefaultUpdateEvery = 10 * time.Minute
	DefaultDrainWait   = 30 * time.Minute
	// handBackWait bounds the wait for the actors told to hand back.
	handBackWait = 2 * time.Minute
)

// Updater finds and installs releases.
type Updater interface {
	// Due answers the version to move to, or "" when the node is on the
	// newest.
	Due(ctx context.Context) (string, error)
	// Install puts version's binary in place of the running one.
	Install(ctx context.Context, version string) error
}

// Node is what the self-update moves.
type Node interface {
	// Drain stops the node taking leases.
	Drain(why string)
	// Live is how many leases the node still runs.
	Live() int
	// HandBackAll stops every live lease, each handed back with cause
	// node_update.
	HandBackAll(why string)
}

// SelfUpdate is the node's self-update. Updater and Node are required.
type SelfUpdate struct {
	Updater Updater
	Node    Node
	// Every is how often a release is looked for; DrainWait how long
	// the leases running may take to end on their own.
	Every     time.Duration
	DrainWait time.Duration
	// Poll is how often the drain looks at Live; a second when zero.
	Poll time.Duration
	Logf func(format string, args ...any)
}

func (u *SelfUpdate) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
	}
}

// Run looks for a release every Every until ctx ends or one is
// installed; it answers the version installed, for the node to restart
// into, or "" when ctx ended first.
func (u *SelfUpdate) Run(ctx context.Context) string {
	every := u.Every
	if every <= 0 {
		every = DefaultUpdateEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		v, err := u.Updater.Due(ctx)
		if err != nil && ctx.Err() == nil {
			u.logf("self-update: looking for a release: %v", err)
		}
		if v != "" {
			if err := u.Update(ctx, v); err != nil {
				u.logf("self-update: %v", err)
			} else {
				return v
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-t.C:
		}
	}
}

// Update drains the node, hands back what will not end in time, and
// installs version. The node stays drained when the install fails: it
// is restarted by hand or by the next start.
func (u *SelfUpdate) Update(ctx context.Context, version string) error {
	why := fmt.Sprintf("the node is updating to %s", version)
	u.logf("self-update: %s; draining", why)
	u.Node.Drain(why)
	wait := u.DrainWait
	if wait <= 0 {
		wait = DefaultDrainWait
	}
	if !u.waitLive(ctx, wait) {
		n := u.Node.Live()
		u.logf("self-update: %d leases still running after %s; handing them back node_update", n, wait)
		u.Node.HandBackAll(why)
		if !u.waitLive(ctx, handBackWait) {
			return fmt.Errorf("%d leases still running after their hand-back; not installing %s", u.Node.Live(), version)
		}
	}
	if err := u.Updater.Install(ctx, version); err != nil {
		return fmt.Errorf("installing %s: %w", version, err)
	}
	u.logf("self-update: installed %s; the node restarts into it", version)
	return nil
}

// waitLive waits up to d for the node to run nothing.
func (u *SelfUpdate) waitLive(ctx context.Context, d time.Duration) bool {
	poll := u.Poll
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(d)
	for u.Node.Live() > 0 {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}
	return true
}

// Newer reports whether release version a is newer than b, both
// vMAJOR.MINOR.PATCH; a version that does not parse is never newer, so
// a development build ("dev", a pseudo-version) is not updated.
func Newer(a, b string) bool {
	pa, ok := parseVersion(a)
	if !ok {
		return false
	}
	pb, ok := parseVersion(b)
	if !ok {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
