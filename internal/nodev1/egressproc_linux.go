//go:build linux

package nodev1

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/supervisor"
)

// egressRuntime implements supervisor.EgressRuntime by reading /proc:
// the node runs on linux (the mini runs linux/amd64), and a lease's
// egress process has no other registry of its own once its actor is
// gone (ADR-051 decision 3) -- its argv is all that is left to find it
// by, the same argv startEgressProcess (relaypod.go) gave it.
type egressRuntime struct{}

var _ supervisor.EgressRuntime = egressRuntime{}

// List finds every running `mesh lease-egress --control ...` process on
// this host, named by pid and when it started: the reconcile
// cross-checks each against the live leases' own EgressPID, never this
// process's argv further, and spares one too young to judge.
func (egressRuntime) List() ([]supervisor.EgressProcess, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []supervisor.EgressProcess
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if isLeaseEgress(pid) {
			started, _ := procStartedAt(pid)
			out = append(out, supervisor.EgressProcess{PID: pid, StartedAt: started})
		}
	}
	return out, nil
}

// Stop ends a lease-egress process the reconcile found no live lease
// naming, the same bare-pid stop releaseLeftovers uses for one an
// actor.json leftover names (decision 5).
func (egressRuntime) Stop(pid int) error {
	return stopLeftoverEgress(pid)
}

// isLeaseEgress reads pid's cmdline and says whether it is a
// `mesh lease-egress --control <path>` process (relaypod.go's
// startEgressProcess); a pid already gone, or one whose cmdline this
// process cannot read (another user's, a kernel thread), is not one.
func isLeaseEgress(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return false
	}
	args := bytes.Split(bytes.TrimRight(data, "\x00"), []byte{0})
	foundSub, foundControl := false, false
	for _, a := range args {
		switch string(a) {
		case "lease-egress":
			foundSub = true
		case "--control":
			foundControl = true
		}
	}
	return foundSub && foundControl
}

// userHZ is the kernel's USER_HZ, the unit of /proc/<pid>/stat's
// starttime: 100 on every architecture Linux builds this node for.
const userHZ = 100

// procStartedAt is when pid started, from its /proc/<pid>/stat
// starttime (clock ticks after boot) and /proc/stat's btime; zero and
// an error when either cannot be read.
func procStartedAt(pid int) (time.Time, error) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return time.Time{}, err
	}
	ticks, err := statStartTicks(string(stat))
	if err != nil {
		return time.Time{}, err
	}
	boot, err := bootTime()
	if err != nil {
		return time.Time{}, err
	}
	return boot.Add(time.Duration(ticks) * time.Second / userHZ), nil
}

// statStartTicks reads field 22, starttime, of a /proc/<pid>/stat line.
// The command (field 2) is parenthesised and may hold spaces or
// parentheses itself, so the fields are counted after its last ')'.
func statStartTicks(stat string) (uint64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, errors.New("no command field in /proc/<pid>/stat")
	}
	f := strings.Fields(stat[i+1:])
	// f[0] is field 3 (state), so field 22 is f[19]
	if len(f) < 20 {
		return 0, errors.New("/proc/<pid>/stat is short")
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// bootTime is /proc/stat's btime.
func bootTime() (time.Time, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(secs, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}
