//go:build !linux

package resources

import (
	"os"
	"time"
)

// lastUsed is ModTime everywhere but linux, which the node runs
// (mirroring internal/nodev1's lastUsed).
func lastUsed(info os.FileInfo) time.Time { return info.ModTime() }

// lowerIOPriority is a no-op off linux: ioprio_set is linux-only, so a
// platform that lacks it evicts at the process's ordinary I/O priority.
func lowerIOPriority() (restore func()) { return func() {} }
