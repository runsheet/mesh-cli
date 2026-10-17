package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/version"
)

// VersionWarnAfter is how long the platform's desired version may
// differ from the running one before the version check warns — the
// same length selfUpdate itself waits (updateRetry) before trying a
// failed version again. VersionCriticalAfter, its double, is "more
// than one update cycle" with no successful update.
const (
	VersionWarnAfter     = time.Hour
	VersionCriticalAfter = 2 * VersionWarnAfter
)

// versionCheck is the quartermaster's version check (goal
// the-quartermaster-keeps-the-nodes-fit): ok at the desired version (or
// with none known); warn when the desired version differs and the node
// has not updated within VersionWarnAfter of first seeing it differ, or
// a dev build runs where a release is desired; critical when the same
// desired version has failed to update for more than VersionCriticalAfter,
// the error named in the detail. Running, Commit, Desired and
// LastFailure are funcs rather than a *Loop so the thresholds can be
// tested without one; VersionCheck wires them to a real Loop.
type versionCheck struct {
	// Running and Commit are the running node's own version and commit.
	Running func() string
	Commit  func() string
	// Desired is the platform's desired version and whether the read
	// went through, as Loop.desiredVersion.
	Desired func(ctx context.Context) (version string, ok bool)
	// LastFailure is the version whose update last failed, when, and
	// why; an empty version is no failure recorded, as Loop.failed and
	// Loop.UpdateError carry it.
	LastFailure func() (version string, at time.Time, reason string)

	mu          sync.Mutex
	differsFrom string    // the desired version this is tracking, "" when none
	since       time.Time // when it was first seen differing from Running
}

func newVersionCheck(running, commit func() string, desired func(context.Context) (string, bool), lastFailure func() (string, time.Time, string)) *versionCheck {
	return &versionCheck{Running: running, Commit: commit, Desired: desired, LastFailure: lastFailure}
}

// VersionCheck is the quartermaster Check for version drift and a
// stuck self-update, wired to l: the running version and commit it
// reports itself as, l.desiredVersion (unchanged) and the last
// self-update failure (l.failed, l.UpdateError). Not yet wired into the
// Loop or the heartbeat — that is item 5 of the same goal.
func VersionCheck(l *Loop) Check {
	return newVersionCheck(
		func() string { return l.Version },
		func() string { return l.Commit },
		l.desiredVersion,
		func() (string, time.Time, string) {
			l.mu.Lock()
			defer l.mu.Unlock()
			return l.failed.version, l.failed.at, l.updateErr
		},
	).Check
}

// sinceDiffer is when desired was first seen differing from the running
// version: now, the first time desired is seen (or changes to a
// different one), kept across calls otherwise.
func (c *versionCheck) sinceDiffer(desired string, now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.differsFrom != desired {
		c.differsFrom = desired
		c.since = now
	}
	return c.since
}

// Check is the quartermaster Check for the running node's version
// against what the platform desires.
func (c *versionCheck) Check(ctx context.Context, now time.Time) []Finding {
	running, commit := c.Running(), c.Commit()
	desired, _ := c.Desired(ctx)

	if desired == "" || desired == running {
		return []Finding{{
			Kind:     "version",
			Severity: "ok",
			Subject:  running,
			Detail:   versionCurrentDetail(running, commit, desired == ""),
			At:       now,
		}}
	}

	elapsed := now.Sub(c.sinceDiffer(desired, now))
	devBuild := !version.Release(running) && version.Release(desired)
	failVersion, _, failReason := c.LastFailure()
	failingSame := failReason != "" && failVersion == desired

	severity := "ok"
	switch {
	case failingSame && elapsed >= VersionCriticalAfter:
		severity = "critical"
	case devBuild || elapsed >= VersionWarnAfter:
		severity = "warn"
	}

	return []Finding{{
		Kind:     "version",
		Severity: severity,
		Subject:  running,
		Detail:   versionDiffDetail(running, commit, desired, elapsed, devBuild, failingSame, failReason),
		At:       now,
	}}
}

func versionCurrentDetail(running, commit string, noDesired bool) string {
	if noDesired {
		return fmt.Sprintf("%s (%s), no desired version is known", running, commit)
	}
	return fmt.Sprintf("%s (%s), current", running, commit)
}

func versionDiffDetail(running, commit, desired string, elapsed time.Duration, devBuild, failingSame bool, failReason string) string {
	switch {
	case failingSame:
		return fmt.Sprintf("%s (%s): desired %s has failed to update for %s: %s", running, commit, desired, elapsed.Round(time.Second), failReason)
	case devBuild:
		return fmt.Sprintf("%s (%s) is a dev build; release %s is desired", running, commit, desired)
	default:
		return fmt.Sprintf("%s (%s): desired %s, differing for %s", running, commit, desired, elapsed.Round(time.Second))
	}
}
