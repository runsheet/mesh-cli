//go:build !unix

package sandbox

import "os"

// ownedByNode is every file where the platform names no owner.
func ownedByNode(os.FileInfo) bool { return true }
