//go:build windows

package node

import (
	"strings"

	"golang.org/x/sys/windows"
)

// realStatfs is DiskCheck's default StatfsFunc on windows:
// GetDiskFreeSpaceEx; the path's drive letter stands in for the device
// (windows is not where the node runs; this exists so the module still
// cross-compiles there, mesh-cli's release build among its targets).
func realStatfs(path string) (DiskStat, error) {
	var free, total, totalFree uint64
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return DiskStat{}, err
	}
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return DiskStat{}, err
	}
	var device uint64
	if len(path) > 0 {
		device = uint64(strings.ToUpper(path)[0])
	}
	return DiskStat{Free: free, Total: total, Device: device}, nil
}
