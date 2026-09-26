package selfupdate

import (
	"errors"
	"fmt"
	"os"
)

// rename is os.Rename; tests replace it to fail a swap midway.
var rename = os.Rename

// PrevPath is where Swap keeps the executable it replaced.
func PrevPath(exePath string) string { return exePath + ".prev" }

// Swap replaces the executable at exePath with newBin. It writes
// <exePath>.new beside it (same directory, so the renames are atomic) with
// mode 0755, moves the current executable to <exePath>.prev, and renames
// the new one into place. If the last rename fails the previous executable
// is put back, so exePath always holds a whole binary. exePath should be
// the resolved executable (no symlink): the swap replaces what it names.
func Swap(exePath string, newBin []byte) error {
	newPath := exePath + ".new"
	prevPath := PrevPath(exePath)
	if err := writeExecutable(newPath, newBin); err != nil {
		return err
	}
	if err := rename(exePath, prevPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("swap: keep previous: %w", err)
	}
	if err := rename(newPath, exePath); err != nil {
		if rerr := rename(prevPath, exePath); rerr != nil {
			return fmt.Errorf("swap: install new: %w; restore previous: %v", err, rerr)
		}
		os.Remove(newPath)
		return fmt.Errorf("swap: install new: %w", err)
	}
	return nil
}

func writeExecutable(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("swap: write %s: %w", path, err)
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(path)
		return fmt.Errorf("swap: write %s: %w", path, err)
	}
	// The umask may have taken bits OpenFile asked for.
	if err := os.Chmod(path, 0o755); err != nil {
		os.Remove(path)
		return fmt.Errorf("swap: chmod %s: %w", path, err)
	}
	return nil
}
