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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
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
	// Stop ends the session: SIGTERM to its process group, SIGKILL after
	// a grace period.
	Stop(h Handle) error
}

// StopGrace is how long Stop waits after SIGTERM before SIGKILL.
var StopGrace = 5 * time.Second

// Detached runs the session as a process in a session (setsid) and
// process group of its own, so a signal to the node's group or the
// node's exit does not reach it. On a service manager that kills the
// whole cgroup (systemd's default KillMode) it does not survive a
// `systemctl stop`; SystemdScope does. launchd's AbandonProcessGroup
// keeps it on macOS.
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
func (Detached) Stop(h Handle) error { return stopGroup(h) }

// SystemdScope runs the session in a transient systemd user scope,
// `systemd-run --user --scope --unit=mesh-session-<kind>-<subject>-<rand>
// --collect -- <command>`: its own cgroup under the user's manager, not
// the node service's, so `systemctl --user stop|restart` of the node
// leaves it running. It is also a process group of its own, as Detached.
type SystemdScope struct {
	// SystemdRun is the systemd-run executable ("systemd-run" by default).
	SystemdRun string
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

// Stop implements Unit: the process group as Detached, then whatever of
// the scope is left.
func (SystemdScope) Stop(h Handle) error {
	err := stopGroup(h)
	if h.UnitName != "" && pidAlive(h.PID) {
		_ = exec.Command("systemctl", "--user", "kill", "--signal=SIGKILL", h.UnitName+".scope").Run()
	}
	return err
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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

// stopGroup sends SIGTERM to the session's process group, and SIGKILL
// when it is still there after StopGrace.
func stopGroup(h Handle) error {
	if h.PID <= 0 {
		return errors.New("sessionunit: no pid")
	}
	if err := syscall.Kill(-h.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sessionunit: stop %d: %w", h.PID, err)
	}
	deadline := time.Now().Add(StopGrace)
	for time.Now().Before(deadline) {
		if h.Exited() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-h.PID, syscall.SIGKILL)
	return nil
}

// pidAlive is whether pid is a running process (not a zombie).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if st, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "stat")); err == nil {
		// "pid (comm) S ...": the state follows the last ')'
		if i := strings.LastIndexByte(string(st), ')'); i >= 0 && i+2 < len(st) && st[i+2] == 'Z' {
			return false
		}
	}
	return true
}
