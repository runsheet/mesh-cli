//go:build unix

package sandbox

import (
	"os"
	"syscall"
)

// ownedByNode is a file the node's own user owns: another node user's
// HOME in a shared /tmp is its node's to sweep, against its own store.
func ownedByNode(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Getuid()
}
