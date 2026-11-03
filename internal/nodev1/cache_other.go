//go:build !linux

package nodev1

import (
	"os"
	"time"
)

// lastUsed is ModTime everywhere but linux, which the node does not run
// on; this exists so the module still cross-compiles there.
func lastUsed(info os.FileInfo) time.Time { return info.ModTime() }
