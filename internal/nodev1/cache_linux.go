//go:build linux

package nodev1

import (
	"os"
	"syscall"
	"time"
)

// lastUsed is info's access time (atime): the node runs on linux (the
// mini runs linux/amd64), where Stat_t.Atim is read reliably across
// architectures. A platform with no Stat_t falls back to ModTime.
func lastUsed(info os.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime()
	}
	return time.Unix(st.Atim.Sec, st.Atim.Nsec)
}
