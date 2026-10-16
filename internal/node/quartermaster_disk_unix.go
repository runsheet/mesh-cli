//go:build !windows

package node

import "golang.org/x/sys/unix"

// realStatfs is DiskCheck's default StatfsFunc: unix statfs(2), the
// filesystem's device taken from its Fsid so two paths on the same
// filesystem de-duplicate.
func realStatfs(path string) (DiskStat, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskStat{}, err
	}
	device := uint64(uint32(st.Fsid.Val[0]))<<32 | uint64(uint32(st.Fsid.Val[1]))
	return DiskStat{
		Free:   st.Bfree * uint64(st.Bsize),
		Total:  st.Blocks * uint64(st.Bsize),
		Device: device,
	}, nil
}
