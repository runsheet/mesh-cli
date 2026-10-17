//go:build windows

package node

import "os"

// ownedByNodeUID is every entry on windows, which names no uid the way
// unix does (windows is not where the node runs; this exists so the
// module still cross-compiles there).
func ownedByNodeUID(os.FileInfo) bool { return true }
