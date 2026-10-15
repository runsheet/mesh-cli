//go:build !windows

package node

import (
	"os"
	"syscall"
)

// ownedByNodeUID is an entry the node's own user owns: ScratchCheck
// never touches another uid's entry in a shared /tmp.
func ownedByNodeUID(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Getuid()
}
