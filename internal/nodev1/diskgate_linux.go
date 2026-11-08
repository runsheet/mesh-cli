//go:build linux

package nodev1

import "syscall"

// FSDiskProbe reads a path's free space with statfs — the node runs on
// linux (the mini runs linux/amd64).
type FSDiskProbe struct{}

// FreeBytes is path's filesystem's free space available to an
// unprivileged process (Bavail, not Bfree, which also counts what is
// reserved for root).
func (FSDiskProbe) FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
