//go:build !windows

package sessionunit

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// A group the stop cannot end — here one this user may not signal, a
// root process's — is Kill's error and no report, though its members
// were read.
func TestDetachedKillStopError(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	if os.Getuid() == 0 {
		t.Skip("root may signal any group")
	}
	fastStop(t)
	pid := rootGroupLeader()
	if pid == 0 {
		t.Skip("no root process group leader")
	}
	killed, err := Detached{}.Kill(Handle{PID: pid})
	if err == nil || len(killed) != 0 {
		t.Fatalf("killed %v, err %v", killed, err)
	}
}

// rootGroupLeader is a live process owned by root that leads its own
// process group, besides init; 0 when there is none.
func rootGroupLeader() int {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		fi, err := os.Stat(filepath.Join("/proc", e.Name()))
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 {
			continue
		}
		for _, p := range groupMembers(pid) {
			if p.PID == pid {
				return pid
			}
		}
	}
	return 0
}
