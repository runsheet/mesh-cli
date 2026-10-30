//go:build unix

package relaysidecar

import (
	"fmt"
	"os"
	"syscall"
)

// ownedBySelf says whether fi is this process's user's own.
func ownedBySelf(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("its owner cannot be read")
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("owned by uid %d, not this user (%d)", st.Uid, os.Getuid())
	}
	return nil
}
