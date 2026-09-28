package sessionunit

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Process is one process a stop found in a session's unit.
type Process struct {
	PID int
	// Comm is /proc/<pid>/comm, the process's short name.
	Comm string
	// Cmdline is /proc/<pid>/cmdline with its NULs as spaces, cut to
	// cmdlineMax bytes.
	Cmdline string
}

func (p Process) String() string {
	if p.Cmdline != "" {
		return fmt.Sprintf("%s[%d] %s", p.Comm, p.PID, p.Cmdline)
	}
	return fmt.Sprintf("%s[%d]", p.Comm, p.PID)
}

// Scope is one mesh-session scope the user manager knows.
type Scope struct {
	// Unit is the scope's name without ".scope", as Handle.UnitName.
	Unit string
	// Active is its ActiveState (active, inactive, failed, …).
	Active string
}

// Systemctl is what SystemdScope asks of the user's systemd: a unit's
// properties, a signal to its cgroup, its stop, the mesh-session
// scopes, and the processes in a cgroup. Tests inject a fake.
type Systemctl interface {
	// Show is `systemctl --user show -p <props> <unit>`, as a map; a
	// unit the manager does not know answers LoadState=not-found.
	Show(unit string, props ...string) (map[string]string, error)
	// Kill is `systemctl --user kill --signal=<signal> <unit>`: every
	// process in the unit's cgroup.
	Kill(unit, signal string) error
	// Stop is `systemctl --user stop <unit>`.
	Stop(unit string) error
	// ListScopes is `systemctl --user list-units 'mesh-session-*'
	// --plain --no-legend --all`, the scopes among them.
	ListScopes() ([]Scope, error)
	// Processes lists the processes in a cgroup (a ControlGroup path),
	// named from /proc; a cgroup that is gone has none.
	Processes(cgroup string) ([]Process, error)
}

// HostSystemctl is the real Systemctl: the `systemctl` binary and the
// cgroup v2 hierarchy.
type HostSystemctl struct {
	// Binary is the systemctl executable ("systemctl" by default).
	Binary string
	// CgroupRoot is where the hierarchy is mounted ("/sys/fs/cgroup").
	CgroupRoot string
}

func (s HostSystemctl) run(args ...string) ([]byte, error) {
	bin := s.Binary
	if bin == "" {
		bin = "systemctl"
	}
	var stderr bytes.Buffer
	cmd := exec.Command(bin, append([]string{"--user"}, args...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Show implements Systemctl.
func (s HostSystemctl) Show(unit string, props ...string) (map[string]string, error) {
	out, err := s.run("show", "-p", strings.Join(props, ","), unit)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m, nil
}

// Kill implements Systemctl.
func (s HostSystemctl) Kill(unit, signal string) error {
	_, err := s.run("kill", "--signal="+signal, unit)
	return err
}

// Stop implements Systemctl.
func (s HostSystemctl) Stop(unit string) error {
	_, err := s.run("stop", unit)
	return err
}

// ListScopes implements Systemctl.
func (s HostSystemctl) ListScopes() ([]Scope, error) {
	out, err := s.run("list-units", "mesh-session-*", "--plain", "--no-legend", "--all")
	if err != nil {
		return nil, err
	}
	return parseListUnits(out), nil
}

// parseListUnits reads list-units' "UNIT LOAD ACTIVE SUB DESCRIPTION"
// rows, keeping the scopes.
func parseListUnits(out []byte) []Scope {
	var scopes []Scope
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) > 0 && (f[0] == "●" || f[0] == "*") {
			f = f[1:]
		}
		if len(f) < 3 || !strings.HasSuffix(f[0], ".scope") {
			continue
		}
		scopes = append(scopes, Scope{Unit: strings.TrimSuffix(f[0], ".scope"), Active: f[2]})
	}
	return scopes
}

// Processes implements Systemctl.
func (s HostSystemctl) Processes(cgroup string) ([]Process, error) {
	if cgroup == "" {
		return nil, nil
	}
	root := s.CgroupRoot
	if root == "" {
		root = "/sys/fs/cgroup"
	}
	b, err := os.ReadFile(filepath.Join(root, cgroup, "cgroup.procs"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var procs []Process
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			procs = append(procs, readProcess(pid))
		}
	}
	return procs, nil
}

// cmdlineMax is how much of a process's command line a Process keeps.
const cmdlineMax = 200

// readProcess names pid from /proc; where there is no /proc it is the
// pid alone.
func readProcess(pid int) Process {
	p := Process{PID: pid}
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	if b, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
		p.Comm = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		c := strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
		if len(c) > cmdlineMax {
			c = c[:cmdlineMax]
		}
		p.Cmdline = c
	}
	return p
}

// groupMembers lists the processes whose process group is pgid, from
// /proc; none where there is no /proc (macOS).
func groupMembers(pgid int) []Process {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var procs []Process
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		st, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// "pid (comm) state ppid pgrp ...": the fields after the last ')'
		i := bytes.LastIndexByte(st, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(st[i+1:]))
		if len(f) < 3 || f[0] == "Z" {
			continue
		}
		if g, err := strconv.Atoi(f[2]); err == nil && g == pgid {
			procs = append(procs, readProcess(pid))
		}
	}
	return procs
}

// others is procs less the session's own pid.
func others(procs []Process, pid int) []Process {
	var out []Process
	for _, p := range procs {
		if p.PID != pid {
			out = append(out, p)
		}
	}
	return out
}

var unitNameRE = regexp.MustCompile(`^mesh-session-([^-]+)-(.+)-[0-9a-f]{8}$`)

// ParseUnitName is UnitName's inverse: the kind and the subject of a
// `mesh-session-<kind>-<subject>-<8 hex>` name (".scope" allowed), the
// subject as it is in the name — its `:` and `/` already `_`.
func ParseUnitName(unit string) (kind, subject string, ok bool) {
	m := unitNameRE.FindStringSubmatch(strings.TrimSuffix(unit, ".scope"))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}
