// Package sessionunit runs an agent session outside the node process
// (mesh-cli ADR 008, mesh-specs ADR 008 Sessions outlive the node): in a
// unit of its own, its stdout written to a file beside the work dir and
// its state in a small file next to it, so stopping, restarting or
// upgrading the node stops only the node.
//
//	<workdir>.session/output.jsonl   the session's stream-json
//	<workdir>.session/stderr.log     its stderr
//	<workdir>.session/state.json     who it is for and where it is (State)
//
// The directory is the work dir's sibling, never inside it, so nothing
// lands in the git checkout.
//
// Two units: SystemdScope, a transient `systemd-run --user --scope` on
// Linux, outside the node service's cgroup; and Detached, a process in a
// session and process group of its own, for macOS or a machine without a
// user systemd. The sandboxed-sessions goal adds a third behind the same
// Unit interface: the session's container is its unit (ADR 007). It is
// not built here.
package sessionunit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Spec is one session to start.
type Spec struct {
	// Kind is the session's kind (build, evaluate, plan, research, rebase).
	Kind string
	// Subject names what the session is for — the item, goal or direction
	// id — in the unit's name.
	Subject string
	// Binary and Args are the command.
	Binary string
	Args   []string
	// Dir is the working directory (the work dir).
	Dir string
	// Env is the whole environment of the process.
	Env []string
	// Stdin is what the process reads (the prompt); nil is /dev/null. An
	// *os.File is handed to the process as it is.
	Stdin *os.File
	// Stdout and Stderr are the files the process writes.
	Stdout, Stderr *os.File
}

// Handle is a started session.
type Handle struct {
	// PID is the session's process (the command itself: systemd-run
	// --scope execs it in place).
	PID int
	// UnitName is the transient scope's name (without ".scope"); empty
	// for a detached process.
	UnitName string

	// proc is set on the node process that started the session: it
	// waits for it (so its exit is known and no zombie is left).
	proc *started
}

// started is a child this process waits on.
type started struct {
	done chan struct{}
	err  error
}

// Wait blocks until a session this process started ends and returns its
// exit as exec.Cmd.Wait does (*exec.ExitError on a non-zero exit). An
// adopted handle — one this process did not start — waits for the pid
// to go and returns nil.
func (h Handle) Wait(ctx context.Context) error {
	if h.proc != nil {
		select {
		case <-h.proc.done:
			return h.proc.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for pidAlive(h.PID) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

// Exited is whether a session this process started has ended; for an
// adopted handle, whether its pid is gone.
func (h Handle) Exited() bool {
	if h.proc != nil {
		select {
		case <-h.proc.done:
			return true
		default:
			return false
		}
	}
	return !pidAlive(h.PID)
}

// Unit starts sessions outside the node process and stops them.
type Unit interface {
	// Name is the unit's kind in state.json ("systemd-scope", "detached").
	Name() string
	// Start starts the session; it keeps running when the node exits.
	Start(spec Spec) (Handle, error)
	// Alive is whether the session's process is still running.
	Alive(h Handle) bool
	// Stop is Kill without its report.
	Stop(h Handle) error
	// Kill ends the session's whole unit — SIGTERM, SIGKILL after
	// StopGrace — waits for it to be gone and returns every process it
	// found there besides the session's own pid: what the agent left
	// running. A unit already gone is no error and no processes.
	Kill(h Handle) (killed []Process, err error)
}

// StopGrace is how long Kill waits after SIGTERM before SIGKILL.
var StopGrace = 5 * time.Second

// goneSlack is how much longer than StopGrace a scope's Kill waits for
// the unit to be gone after its stop.
var goneSlack = 3 * time.Second

// pollEvery is how often Kill looks again.
var pollEvery = 100 * time.Millisecond

// Detached runs the session as a process in a session (setsid) and
// process group of its own, so a signal to the node's group or the
// node's exit does not reach it. A service manager that kills the
// whole cgroup (systemd's default KillMode=control-group) would kill it
// on a `systemctl stop`: the node's unit says KillMode=process, and on
// such a machine SystemdScope is used anyway. The LaunchAgent's
// AbandonProcessGroup keeps it on macOS.
type Detached struct{}

// Name implements Unit.
func (Detached) Name() string { return "detached" }

// Start implements Unit.
func (Detached) Start(spec Spec) (Handle, error) {
	return start(exec.Command(spec.Binary, spec.Args...), spec, "")
}

// Alive implements Unit.
func (Detached) Alive(h Handle) bool { return !h.Exited() }

// Stop implements Unit.
func (d Detached) Stop(h Handle) error {
	_, err := d.Kill(h)
	return err
}

// Kill implements Unit: the process group, its members besides the
// session's pid listed first (from /proc, so on Linux only). The report
// is returned only when the group is confirmed gone; a stop that fails
// or leaves it standing is the error and no report.
func (Detached) Kill(h Handle) ([]Process, error) {
	if h.PID <= 0 {
		return nil, errors.New("sessionunit: no pid")
	}
	killed := others(groupMembers(h.PID), h.PID)
	if err := stopGroup(h); err != nil {
		return nil, err
	}
	return killed, nil
}

// SystemdScope runs the session in a transient systemd user scope,
// `systemd-run --user --scope --unit=mesh-session-<kind>-<subject>-<rand>
// --collect -- <command>`: its own cgroup under the user's manager, not
// the node service's, so `systemctl --user stop|restart` of the node
// leaves it running. It is also a process group of its own, as Detached.
type SystemdScope struct {
	// SystemdRun is the systemd-run executable ("systemd-run" by default).
	SystemdRun string
	// Systemctl is how the scope is looked at and stopped; nil is
	// HostSystemctl.
	Systemctl Systemctl
}

func (s SystemdScope) systemctl() Systemctl {
	if s.Systemctl == nil {
		return HostSystemctl{}
	}
	return s.Systemctl
}

// Name implements Unit.
func (SystemdScope) Name() string { return "systemd-scope" }

// Argv is the command line Start runs for spec under the unit name.
func (s SystemdScope) Argv(spec Spec, unit string) []string {
	bin := s.SystemdRun
	if bin == "" {
		bin = "systemd-run"
	}
	argv := []string{bin, "--user", "--scope", "--unit=" + unit, "--collect", "--quiet", "--", spec.Binary}
	return append(argv, spec.Args...)
}

// Start implements Unit.
func (s SystemdScope) Start(spec Spec) (Handle, error) {
	unit := UnitName(spec.Kind, spec.Subject)
	argv := s.Argv(spec, unit)
	return start(exec.Command(argv[0], argv[1:]...), spec, unit)
}

// Alive implements Unit.
func (SystemdScope) Alive(h Handle) bool { return !h.Exited() }

// Stop implements Unit.
func (s SystemdScope) Stop(h Handle) error {
	_, err := s.Kill(h)
	return err
}

// Kill implements Unit: the scope as a unit, not its leader. Its
// cgroup (Show ControlGroup) and the processes in it are read first,
// the cgroup gets SIGTERM, SIGKILL once StopGrace has passed with any
// left, then `systemctl --user stop` (KillMode=control-group: every
// process in it), and Kill waits for the unit to be gone (--collect
// unloads it) for StopGrace and goneSlack. Every error on the way is
// returned, and the report only once the unit is confirmed gone: a
// manager that cannot be asked is an error, never proof of a stop. A
// handle with no unit is stopped as Detached's.
func (s SystemdScope) Kill(h Handle) ([]Process, error) {
	if h.UnitName == "" {
		return Detached{}.Kill(h)
	}
	sc, unit := s.systemctl(), h.UnitName+".scope"
	gone, err := unitGone(sc, unit)
	if err != nil {
		return nil, err
	}
	if gone {
		if h.PID > 0 && pidAlive(h.PID) {
			return nil, stopGroup(h)
		}
		return nil, nil
	}
	props, err := sc.Show(unit, "ControlGroup")
	if err != nil {
		return nil, fmt.Errorf("sessionunit: %s: %w", unit, err)
	}
	cg := props["ControlGroup"]
	procs, err := sc.Processes(cg)
	if err != nil {
		return nil, fmt.Errorf("sessionunit: %s: processes: %w", unit, err)
	}
	found := others(procs, h.PID)

	var errs []error
	// fail keeps every error of a signal or the stop; the manager saying
	// the unit is not loaded is an answer (it was collected), not one.
	fail := func(what string, err error) {
		if err != nil && !errors.Is(err, ErrNotLoaded) {
			errs = append(errs, fmt.Errorf("sessionunit: %s: %s: %w", unit, what, err))
		}
	}
	left := func() bool {
		p, err := sc.Processes(cg)
		if err != nil {
			errs = append(errs, fmt.Errorf("sessionunit: %s: processes: %w", unit, err))
			return true // unknown: SIGKILL anyway
		}
		return len(p) > 0
	}
	fail("SIGTERM", sc.Kill(unit, "SIGTERM"))
	deadline := time.Now().Add(StopGrace)
	more := left()
	for more && time.Now().Before(deadline) && len(errs) == 0 {
		time.Sleep(pollEvery)
		more = left()
	}
	if more {
		fail("SIGKILL", sc.Kill(unit, "SIGKILL"))
	}
	fail("stop", sc.Stop(unit))

	deadline = time.Now().Add(StopGrace + goneSlack)
	for {
		gone, err := unitGone(sc, unit)
		if err != nil {
			return nil, errors.Join(append(errs, err)...)
		}
		if gone {
			break
		}
		if !time.Now().Before(deadline) {
			return nil, errors.Join(append(errs, fmt.Errorf("sessionunit: %s still loaded after stop", unit))...)
		}
		time.Sleep(pollEvery)
	}
	return found, errors.Join(errs...)
}

// unitGone is whether the manager no longer runs unit: it answers
// LoadState=not-found, or ActiveState inactive or failed. A show that
// fails is its error, not an answer — the unit may be running still.
func unitGone(sc Systemctl, unit string) (bool, error) {
	p, err := sc.Show(unit, "LoadState", "ActiveState")
	if err != nil {
		return false, fmt.Errorf("sessionunit: %s: %w", unit, err)
	}
	switch {
	case p["LoadState"] == "not-found":
		return true, nil
	case p["ActiveState"] == "inactive", p["ActiveState"] == "failed":
		return true, nil
	}
	return false, nil
}

var unitUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.\-]+`)

// UnitName is `mesh-session-<kind>-<subject>-<rand>`, the subject
// cleaned to the characters a unit name may hold.
func UnitName(kind, subject string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	part := func(s, def string) string {
		s = strings.Trim(unitUnsafe.ReplaceAllString(s, "_"), "_")
		if len(s) > 60 {
			s = s[:60]
		}
		if s == "" {
			return def
		}
		return s
	}
	return "mesh-session-" + part(kind, "session") + "-" + part(subject, "none") + "-" + hex.EncodeToString(b)
}

// Default is the unit for this machine: SystemdScope when systemd-run
// is on PATH and the user's manager answers `systemctl --user`, else
// Detached. Decided once per process.
func Default() Unit {
	defaultOnce.Do(func() {
		defaultUnit = Detached{}
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return
		}
		if exec.Command("systemctl", "--user", "show-environment").Run() == nil {
			defaultUnit = SystemdScope{}
		}
	})
	return defaultUnit
}

var (
	defaultOnce sync.Once
	defaultUnit Unit
)

// start starts cmd for spec in its own session and waits for it in the
// background.
func start(cmd *exec.Cmd, spec Spec, unit string) (Handle, error) {
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if spec.Stdin != nil {
		cmd.Stdin = spec.Stdin
	}
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return Handle{}, err
	}
	p := &started{done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return Handle{PID: cmd.Process.Pid, UnitName: unit, proc: p}, nil
}
