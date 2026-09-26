package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Dir is a Source of one release's assets already on disk: <Dir>/<name>,
// whatever the version. It is for checking a downloaded (or deliberately
// edited) release by hand with `mesh node update --from-dir`; the files are
// verified exactly as GitHub's are, against the version's release identity,
// so a directory can never vouch for itself.
type Dir string

// Asset reads <d>/<name>; a missing file is ErrMissingAsset.
func (d Dir) Asset(_ context.Context, version, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(string(d), name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, refuse(ErrMissingAsset, nil, "release %s has no %s in %s", version, name, string(d))
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(b) > maxAssetSize {
		return nil, fmt.Errorf("read %s: larger than %d bytes", name, maxAssetSize)
	}
	return b, nil
}
