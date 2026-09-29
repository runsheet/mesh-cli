package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/selfupdate"
	"projectmesh.io/mesh-cli/internal/version"
)

// ErrRestartForUpdate is what Run returns once a self-update has put the
// new binary in place: `mesh node start` exits 0 on it and the service
// manager (systemd Restart=always, launchd KeepAlive) starts the new one.
var ErrRestartForUpdate = errors.New("restart for update")

// updateRetry is how long a version whose update failed is left before
// it is tried again, while the platform still asks for it.
var updateRetry = time.Hour

// Updater is what the loop needs to update the node's own binary (adr-002);
// ReleaseUpdater is the real one, tests inject fakes.
type Updater interface {
	// Fetch downloads version's binary for this OS/arch and verifies it;
	// an error is a refusal whose selfupdate.Reason the heartbeat carries.
	Fetch(ctx context.Context, version string) ([]byte, error)
	// Begin records the update from → to, just before the swap.
	Begin(from, to string) error
	// Swap puts bin in place of the running executable, keeping it as .prev.
	Swap(bin []byte) error
	// Refuse records an update that did not happen and why.
	Refuse(from, to, reason string) error
	// Confirm records that this binary is up and heartbeating, ending a
	// pending update; it reports whether one was pending.
	Confirm() (bool, error)
	// NoteConfirmError records the last error a heartbeat hit while a
	// pending update is unconfirmed, for a rollback to name.
	NoteConfirmError(reason string) error
}

// ErrUnconfirmed is what Run returns when a pending update's confirm
// window (Loop.ConfirmWithin) elapses with no heartbeat through: the
// platform could not be reached, which is not the binary failing to
// run. `mesh node start` logs it and exits 1 for the service to start
// the process again; the third such start rolls back, naming the last
// error (selfupdate.StateFile.Rollback). The error is an
// *UnconfirmedError.
var ErrUnconfirmed = errors.New("update not confirmed")

// UnconfirmedError is ErrUnconfirmed with what it carries: the version
// that could not confirm, the window it had and the last error.
type UnconfirmedError struct {
	Version string
	Window  time.Duration
	Last    error
}

func (e *UnconfirmedError) Error() string {
	return fmt.Sprintf("%s did not confirm within %s: %v", e.Version, e.Window, e.Last)
}

func (e *UnconfirmedError) Is(target error) bool { return target == ErrUnconfirmed }

func (e *UnconfirmedError) Unwrap() error { return e.Last }

// PlatformSlow is how a confirm error is said: a timeout is the platform
// being slow, never the binary failing to start.
func PlatformSlow(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return err.Error() + " (the platform is slow)"
	}
	return err.Error()
}

// confirmPending reports whether this process is still confirming a
// pending update: one is pending (Loop.Confirming) and no heartbeat has
// gone through yet.
func (l *Loop) confirmPending() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Update != nil && l.Confirming != "" && !l.confirmed
}

// confirmDeadline is when the confirm window ends: ConfirmWithin
// (selfupdate.DefaultConfirmWithin when unset) after Started, which the
// first call sets to now when the caller left it empty.
func (l *Loop) confirmDeadline() time.Time {
	within := l.ConfirmWithin
	if within <= 0 {
		within = selfupdate.DefaultConfirmWithin
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Started.IsZero() {
		l.Started = l.now()
	}
	return l.Started.Add(within)
}

// confirmFailed is a heartbeat that did not go through while a pending
// update is unconfirmed: logged with its error and how long the window
// has left, and recorded for a rollback to name.
func (l *Loop) confirmFailed(login string, err error) {
	left := l.confirmDeadline().Sub(l.now()).Round(time.Second)
	if left < 0 {
		left = 0
	}
	l.logf("self-update: confirming %s: heartbeat %s: %s; retrying for %s more", l.Confirming, login, PlatformSlow(err), left)
	l.mu.Lock()
	l.confirmErr = err
	l.mu.Unlock()
	if nerr := l.Update.NoteConfirmError(err.Error()); nerr != nil {
		l.logf("self-update: record confirm error: %v", nerr)
	}
}

// unconfirmedNow is the error Run returns once the confirm window has
// elapsed with no heartbeat through; nil while there is time left, or
// with nothing to confirm.
func (l *Loop) unconfirmedNow() error {
	if !l.confirmPending() || l.now().Before(l.confirmDeadline()) {
		return nil
	}
	within := l.ConfirmWithin
	if within <= 0 {
		within = selfupdate.DefaultConfirmWithin
	}
	l.mu.Lock()
	last := l.confirmErr
	l.mu.Unlock()
	if last == nil {
		last = errors.New("no heartbeat went through")
	}
	return &UnconfirmedError{Version: l.Confirming, Window: within, Last: last}
}

// updateFailure is the last version whose update failed. final is a
// rollback: that version is not tried again until the platform asks for
// another.
type updateFailure struct {
	version string
	at      time.Time
	final   bool
}

// NoteUpdateFailure seeds the loop with an update that failed before this
// process started (StartCheck.Failed): the heartbeat carries reason, and
// version is not tried again for updateRetry — or, final, until the
// platform asks for another.
func (l *Loop) NoteUpdateFailure(version, reason string, final bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed = updateFailure{version: version, at: l.now(), final: final}
	l.updateErr = reason
}

// UpdateError is the reason the last self-update failed, as the
// heartbeat reports it; empty when there is none to report.
func (l *Loop) UpdateError() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.updateErr
}

// confirmUpdate is called after a heartbeat went through: the first one
// of the process confirms a pending update.
func (l *Loop) confirmUpdate() {
	l.mu.Lock()
	done := l.Update == nil || l.confirmed
	l.confirmed = true
	l.mu.Unlock()
	if done {
		return
	}
	confirmed, err := l.Update.Confirm()
	switch {
	case err != nil:
		l.logf("self-update: confirm %s: %v", l.Version, err)
	case confirmed:
		l.mu.Lock()
		l.updateErr = ""
		l.mu.Unlock()
		l.logf("self-update: %s is up; update confirmed", l.Version)
	}
}

// selfUpdate follows the version the platform asks for (after the
// heartbeat, every tick). An empty one, the running one, a dev build, a
// draining node or an update already under way do nothing. A dev build is
// any not reporting a release tag — "dev" or the commit it was built from
// (version.Resolved) — and keeps today's behaviour: it was put there by
// hand, from a checkout, and a release the platform asks for would
// replace it unasked; a node follows desiredVersion only from a release.
// A version that failed is left for updateRetry (for good after a
// rollback) unless the platform asks for another. The update itself runs beside the loop.
func (l *Loop) selfUpdate(ctx context.Context) {
	if l.Update == nil || !version.Release(l.Version) || l.Draining() {
		return
	}
	l.mu.Lock()
	busy := l.updating || l.restart
	l.mu.Unlock()
	if busy {
		return
	}
	desired, ok := l.desiredVersion(ctx)
	if !ok {
		return
	}
	l.mu.Lock()
	if desired == "" || desired == l.Version {
		// nothing asked: an old failure is no longer news
		l.updateErr = ""
		l.mu.Unlock()
		return
	}
	f := l.failed
	if f.version == desired && (f.final || l.now().Sub(f.at) < updateRetry) {
		l.mu.Unlock()
		return
	}
	l.updating = true
	l.updateWG.Add(1)
	l.mu.Unlock()
	go l.update(ctx, l.Version, desired)
}

// desiredVersion is the version the platform asks this node to run: the
// first non-empty one over its node in each served org. ok is false when
// no read went through. A platform without the field is asked nothing,
// logged once.
func (l *Loop) desiredVersion(ctx context.Context) (string, bool) {
	ok := false
	for _, sc := range l.Scopes {
		v, err := l.GW.GetDesiredVersion(ctx, sc.NodeID)
		if gateway.IsUnsupported(err) {
			l.mu.Lock()
			logged := l.desiredUnsupported
			l.desiredUnsupported = true
			l.mu.Unlock()
			if !logged {
				l.logf("self-update: the platform has no desired version yet; not updating")
			}
			return "", true
		}
		if err != nil {
			l.logf("desired version of %s: %v", sc.Login, err)
			continue
		}
		ok = true
		if v != "" {
			return v, true
		}
	}
	return "", ok
}

// update fetches and verifies to and ensures its node image — a refused
// release or an image that cannot be had costs the node no work and
// leaves it on its binary — then swaps the binary in and restarts at
// once: every session that outlives the node runs on in its own unit,
// and the new process adopts it (adr-009, adr-002). Run then returns
// ErrRestartForUpdate. A sandboxed node's sessions end with it (their
// pod is not yet their unit), so it still drains first and swaps once
// they have ended. A failure resumes a drain the update began and is
// reported on the heartbeat.
func (l *Loop) update(ctx context.Context, from, to string) {
	defer l.updateWG.Done()
	l.logf("self-update: %s → %s: fetching", from, to)
	bin, err := l.Update.Fetch(ctx, to)
	if err != nil {
		l.updateFailed(ctx, from, to, err, false)
		return
	}
	if l.Image != nil {
		img, err := l.Image(ctx, to)
		if err != nil {
			l.updateFailed(ctx, from, to, err, false)
			return
		}
		l.logf("self-update: %s", img.Summary())
	}
	ours := false
	if l.sandboxed() {
		ours = !l.Draining()
		l.logf("self-update: %s verified; draining (sandboxed sessions end with the node)", to)
		if err := l.DrainAndWait(ctx); err != nil {
			l.mu.Lock()
			l.updating = false
			l.mu.Unlock()
			l.logf("self-update to %s: %v; not updated", to, err)
			l.notify()
			return
		}
	} else {
		l.logf("self-update: %s verified; swapping and restarting, sessions left running", to)
	}
	err = l.Update.Begin(from, to)
	if err == nil {
		err = l.Update.Swap(bin)
	}
	if err != nil {
		l.updateFailed(ctx, from, to, err, ours)
		return
	}
	l.mu.Lock()
	l.updating = false
	l.restart = true
	n := l.sessions()
	l.mu.Unlock()
	l.logf("self-update: %s is in place; exiting for the service to start it, %d session(s) left for it to adopt", to, n)
	l.detach()
	l.notify()
}

// sandboxed reports whether the node runs its sessions in pods, which
// end with the node: a self-update then drains before it swaps.
func (l *Loop) sandboxed() bool {
	return l.Sandbox != nil && l.Sandbox.Mode == SandboxOn
}

// updateFailed records a failed update: the state file and the heartbeat
// say why, and to waits updateRetry. resume undoes the update's own drain.
// A stopping node records nothing.
func (l *Loop) updateFailed(ctx context.Context, from, to string, err error, resume bool) {
	if ctx.Err() != nil {
		l.mu.Lock()
		l.updating = false
		l.mu.Unlock()
		l.notify()
		return
	}
	reason := selfupdate.Reason(err)
	l.logf("self-update to %s refused: %v", to, err)
	if rerr := l.Update.Refuse(from, to, reason); rerr != nil {
		l.logf("self-update: record refusal: %v", rerr)
	}
	l.mu.Lock()
	l.failed = updateFailure{version: to, at: l.now()}
	l.updateErr = reason
	l.updating = false
	l.mu.Unlock()
	if resume {
		l.Resume()
	}
	l.notify()
}

// updatingNow reports whether an update is between its decision and its
// end: Run does not end on a drain the update is waiting out.
func (l *Loop) updatingNow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.updating
}

// restartRequested reports whether a new binary is in place.
func (l *Loop) restartRequested() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.restart
}

// ReleaseUpdater updates from signed tag releases of runsheet/mesh-cli,
// downloaded from the public mirror runsheet/mesh (adr-002), recording each
// step in the update state file.
type ReleaseUpdater struct {
	Source selfupdate.Source
	State  *selfupdate.StateFile
	// Exe is the running executable, symlinks resolved (Executable).
	Exe string
	// GOOS and GOARCH pick the release asset; empty is this build's.
	GOOS, GOARCH string
	// Policy is what the signature must satisfy; nil is
	// selfupdate.ReleasePolicy.
	Policy func(version string) selfupdate.Policy
}

func (u *ReleaseUpdater) Fetch(ctx context.Context, version string) ([]byte, error) {
	goos, goarch := u.GOOS, u.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	rel, err := selfupdate.Fetch(ctx, u.Source, version, goos, goarch)
	if err != nil {
		return nil, err
	}
	policy := u.Policy
	if policy == nil {
		policy = selfupdate.ReleasePolicy
	}
	if err := selfupdate.Verify(rel, policy(version)); err != nil {
		return nil, err
	}
	return rel.Binary, nil
}

func (u *ReleaseUpdater) Begin(from, to string) error { return u.State.BeginUpdate(from, to) }

func (u *ReleaseUpdater) Swap(bin []byte) error { return selfupdate.Swap(u.Exe, bin) }

func (u *ReleaseUpdater) Refuse(from, to, reason string) error {
	return u.State.Refuse(from, to, reason)
}

func (u *ReleaseUpdater) NoteConfirmError(reason string) error {
	return u.State.NoteConfirmError(reason)
}

func (u *ReleaseUpdater) Confirm() (bool, error) {
	s, err := u.State.Load()
	if err != nil || !s.Pending() {
		return false, err
	}
	return true, u.State.Confirm()
}

// Executable is the running executable with symlinks resolved: the file
// a swap replaces.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// StartCheck is what the update state says as `mesh node start` begins.
type StartCheck struct {
	// RolledBack: the new binary had failed to start twice, and .prev is
	// back in place. The process reports Reason as From and exits 0 so
	// the service manager starts the previous binary.
	RolledBack bool
	From, To   string
	// Failed is a version whose update failed, with Reason; Final when it
	// was rolled back (it is not tried again until another is asked for).
	Failed string
	Reason string
	Final  bool
	// Pending is whether an update is still awaiting confirmation after
	// this start (never true alongside RolledBack).
	Pending bool
	// Attempts is this start's count against the pending update (1 on
	// the first, up to selfupdate.MaxStarts).
	Attempts int
}

// CheckStart counts this start of a pending update and rolls back to
// <exe>.prev when the new binary has failed to start twice
// (selfupdate.MaxStarts); otherwise it hands on the last failure, if any.
// window is the confirm window a rollback names (Config.ConfirmWithin).
func CheckStart(f *selfupdate.StateFile, exe string, window time.Duration) (StartCheck, error) {
	attempts, err := f.NoteStart()
	if err != nil {
		return StartCheck{}, err
	}
	roll, err := f.ShouldRollback()
	if err != nil {
		return StartCheck{}, err
	}
	if roll {
		if err := f.Rollback(exe, "", window); err != nil {
			return StartCheck{}, err
		}
	}
	s, err := f.Load()
	if err != nil {
		return StartCheck{}, err
	}
	c := StartCheck{RolledBack: roll, From: s.From, To: s.To, Pending: s.Pending(), Attempts: attempts}
	if s.LastError != "" && s.To != "" {
		c.Failed, c.Reason, c.Final = s.To, s.LastError, s.RolledBackAt != nil
	}
	return c, nil
}

// StartLines are node.log's first lines on every start, logged before
// anything that can block on the platform: the version, the pid and the
// config path so a start that exits before it can log anything else is
// never silent, and whether an update is pending so a slow confirm is
// never mistaken for a crash.
func StartLines(version, cfgPath string, check StartCheck) []string {
	lines := []string{
		fmt.Sprintf("mesh node %s, pid %d, config %s", version, os.Getpid(), cfgPath),
	}
	if check.Pending {
		lines = append(lines, fmt.Sprintf("confirming update %s → %s: start %d of %d, rolls back on the third", check.From, check.To, check.Attempts, selfupdate.MaxStarts))
	} else {
		lines = append(lines, "no update pending")
	}
	return lines
}

// String is the start check as a log line.
func (c StartCheck) String() string {
	switch {
	case c.RolledBack:
		return fmt.Sprintf("self-update: %s failed to start; rolled back to %s", c.To, c.From)
	case c.Failed != "":
		return fmt.Sprintf("self-update: the update to %s failed: %s", c.Failed, c.Reason)
	}
	return ""
}
