package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// SumsName is the file WriteSums writes, the asset the release signs
// (mesh-cli ADR-002's asset names; mesh-specs ADR 017 §4 step 2).
const SumsName = "SHA256SUMS"

// WriteSums sums each named artefact under dir and writes dir/SHA256SUMS in
// sha256sum's format — `<sha256 hex>  <name>\n`, two spaces, sorted by
// name, as `sha256sum mesh-*` writes it in release-tag.yml — returning the
// bytes written, which are what Sign signs. A missing artefact is an error
// and nothing is written.
func WriteSums(dir string, names []string) ([]byte, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("sums: no artefacts to sum")
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	var buf bytes.Buffer
	for _, name := range sorted {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("sums: %w", err)
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&buf, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	out := buf.Bytes()
	if err := os.WriteFile(filepath.Join(dir, SumsName), out, 0o644); err != nil {
		return nil, fmt.Errorf("sums: %w", err)
	}
	return out, nil
}
