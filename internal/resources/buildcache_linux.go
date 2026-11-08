//go:build linux

package resources

import (
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// lastUsed is info's access time (atime): the node runs on linux (the mini
// runs linux/amd64), where Stat_t.Atim is read reliably across
// architectures. A platform with no Stat_t falls back to ModTime
// (mirroring internal/nodev1's lastUsed).
func lastUsed(info os.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime()
	}
	return time.Unix(st.Atim.Sec, st.Atim.Nsec)
}

// ioprio_set/ioprio_get's which and class values (linux/ioprio.h); not
// defined by golang.org/x/sys/unix, which carries only the raw syscall
// numbers (unix.SYS_IOPRIO_SET, unix.SYS_IOPRIO_GET) per architecture.
const (
	ioprioWhoProcess = 1
	ioprioClassShift = 13
	ioprioClassIdle  = 3

	// ioprioFallback is the best-effort priority restored when reading
	// the thread's own priority fails: best-effort class, level 4.
	ioprioFallback = 2<<ioprioClassShift | 4
)

// lowerIOPriority locks the calling goroutine to its OS thread and sets
// that thread's I/O priority to the idle class — scheduled only when no
// other I/O is pending — so an eviction run's reads and unlinks never
// compete with a live build's disk I/O. The returned restore puts the
// thread's previous priority back and unlocks the goroutine; the caller
// must call it exactly once, however Evict returns.
//
// Both calls are best-effort: a kernel or container that refuses
// ioprio_set (an old kernel, a restrictive seccomp profile) leaves the
// thread at its ordinary priority rather than failing the evict.
func lowerIOPriority() (restore func()) {
	runtime.LockOSThread()
	prev, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, 0, 0)
	if errno != 0 {
		prev = uintptr(ioprioFallback)
	}
	unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, uintptr(ioprioClassIdle<<ioprioClassShift))
	return func() {
		unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, prev)
		runtime.UnlockOSThread()
	}
}
