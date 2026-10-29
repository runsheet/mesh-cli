package nodev1

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"projectmesh.io/mesh-cli/internal/mcprelay"
)

// leaseSocketName is a lease's sessionproxy socket, alone in its
// directory.
const leaseSocketName = "s"

// leaseDirLen is how many hex characters of the lease id's hash name its
// socket directory: short, since a unix socket's path is bounded
// (mcprelay.MaxSocketPath), and stable, so a node restarted on the same
// lease finds the directory again.
const leaseDirLen = 12

// socketRoots are where the lease socket directories may go, in order:
// the runtime's SocketRoot, the user's runtime directory, the temporary
// directory, /tmp. A lease's directory is never under its work root or
// TMPDIR's own depth alone — on a node validating inside a job's
// scratch TMPDIR is .mesh/jobs/<job>/.scratch/validate-…, past the
// bound by itself (Mesh v1 spec, "The mesh-cli node").
func (r *Runtime) socketRoots() []string {
	if r.SocketRoot != "" {
		return []string{r.SocketRoot}
	}
	name := "mesh"
	if uid := os.Getuid(); uid >= 0 {
		name += "-" + strconv.Itoa(uid)
	}
	var roots []string
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		roots = append(roots, filepath.Join(x, "mesh"))
	}
	return append(roots, filepath.Join(os.TempDir(), name), filepath.Join("/tmp", name))
}

// fitsSocket says whether a lease socket under root binds as it is.
func fitsSocket(root string) bool {
	return len(filepath.Join(root, "0123456789ab", leaseSocketName)) <= mcprelay.MaxSocketPath
}

// leaseSocketDir makes lease's socket directory, 0700, under the first
// socket root whose socket path fits and which is a directory of this
// user's own: one it can make 0700. A directory a crashed node left for
// the lease is made afresh. The caller removes it when the lease ends.
func (r *Runtime) leaseSocketDir(lease string) (string, error) {
	sum := sha256.Sum256([]byte(lease))
	leaf := hex.EncodeToString(sum[:])[:leaseDirLen]
	var errs []error
	for _, root := range r.socketRoots() {
		if !fitsSocket(root) {
			errs = append(errs, fmt.Errorf("%s: a socket under it is past the %d bytes a unix socket takes", root, mcprelay.MaxSocketPath))
			continue
		}
		if err := ownDir(root); err != nil {
			errs = append(errs, err)
			continue
		}
		dir := filepath.Join(root, leaf)
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			errs = append(errs, err)
			continue
		}
		return dir, nil
	}
	return "", fmt.Errorf("no directory for the lease's socket: %w", errors.Join(errs...))
}

// ownDir makes root when it is missing and holds it to a real directory
// (not a link) this user can set to 0700, which only its owner can: a
// shared /tmp's mesh-<uid> someone else made is refused.
func ownDir(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	return os.Chmod(root, 0o700)
}
