//go:build windows

package auth

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on the entire file via LockFileEx.
// Blocks until acquired (no LOCKFILE_FAIL_IMMEDIATELY flag).
func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		^uint32(0),
		^uint32(0),
		ol,
	)
}

func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		^uint32(0),
		^uint32(0),
		ol,
	)
}
