//go:build darwin

package nodev1

import (
	"os"
	"syscall"
	"time"
)

// lastUsed is info's access time (atime) on a Mac, as on linux
// (cache_linux.go): the node does not run on one, but its tests do, and
// Trim's least-recently-used order is by access there too.
func lastUsed(info os.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime()
	}
	return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
}
