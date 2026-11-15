package nodetools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"projectmesh.io/mesh-cli/internal/mcptool"
)

// Signing a release (mesh-cli ADR 002 amendment 2026-11-15): a runsheet
// deploy job of runsheet/mesh-cli builds SHA256SUMS in its workspace, and
// the node — never the session, which holds no signing token — signs it
// keyless through our own Sigstore as the lease's session (its ID token
// for audience sigstore), writing the bundle beside it as
// SHA256SUMS.sigstore.json. The certificate names the job, its repository
// and its kind; a node accepts the release only when the repository is
// runsheet/mesh-cli, the kind is deploy and the sums name the version.

// SignReleaseTool is the sign_release node tool's name, as a kind's
// node_tools names it.
const SignReleaseTool = "sign_release"

// maxSums bounds SHA256SUMS: a handful of lines.
const maxSums = 64 << 10

// sumsLine is one sha256sum line, `<hex>  <name>` or `<hex> *<name>`.
var sumsLine = regexp.MustCompile(`^[0-9a-f]{64} [ *][A-Za-z0-9._-]+$`)

// SignRelease is one lease's sign_release tool.
type SignRelease struct {
	// Workspace is the lease's checkout; Path is resolved inside it.
	Workspace string
	// Sign signs sums keyless and answers the Sigstore bundle's JSON; nil
	// when the node has no Fulcio and Rekor or the lease no signing token.
	Sign func(ctx context.Context, sums []byte) ([]byte, error)
}

// Tool is sign_release {path}.
func (s *SignRelease) Tool() mcptool.Tool {
	return mcptool.Tool{
		Name: SignReleaseTool,
		Description: "Have the node sign a release's SHA256SUMS through our Sigstore as this job, " +
			"writing SHA256SUMS.sigstore.json beside it. Run after the release build, before uploading.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "SHA256SUMS's path in the workspace, e.g. dist/SHA256SUMS"},
			},
			"required": []string{"path"},
		},
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			rel, _ := args["path"].(string)
			full, err := insideWorkspace(s.Workspace, rel)
			if err != nil {
				return "", err
			}
			if filepath.Base(full) != "SHA256SUMS" {
				return "", fmt.Errorf("refused: sign_release signs a file named SHA256SUMS, not %q", filepath.Base(full))
			}
			sums, err := os.ReadFile(full)
			if err != nil {
				return "", fmt.Errorf("refused: %v", err)
			}
			if err := checkSums(sums); err != nil {
				return "", err
			}
			if s.Sign == nil {
				return "", fmt.Errorf("this node cannot sign a release: it has no Fulcio and Rekor, or the lease no signing token")
			}
			bundle, err := s.Sign(ctx, sums)
			if err != nil {
				return "", fmt.Errorf("signing failed: %v", err)
			}
			out := filepath.Join(filepath.Dir(full), "SHA256SUMS.sigstore.json")
			if err := os.WriteFile(out, bundle, 0o644); err != nil {
				return "", err
			}
			sum := sha256.Sum256(sums)
			b, _ := json.Marshal(map[string]string{
				"signed": rel, "bundle": strings.TrimPrefix(strings.TrimPrefix(out, s.Workspace), "/"),
				"sha256": hex.EncodeToString(sum[:]),
			})
			return string(b), nil
		},
	}
}

// insideWorkspace is rel under ws, refused when it is absolute or climbs
// out.
func insideWorkspace(ws, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if ws == "" {
		return "", fmt.Errorf("this lease has no workspace")
	}
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("refused: path is relative to the workspace")
	}
	full := filepath.Join(ws, filepath.Clean(rel))
	if full != ws && !strings.HasPrefix(full, filepath.Clean(ws)+string(filepath.Separator)) {
		return "", fmt.Errorf("refused: %q is outside the workspace", rel)
	}
	return full, nil
}

// checkSums refuses anything but a short sha256sum file naming
// mesh-version: what the node signs is a release's sums and nothing else.
func checkSums(b []byte) error {
	if len(b) == 0 || len(b) > maxSums {
		return fmt.Errorf("refused: SHA256SUMS is %d bytes", len(b))
	}
	version := false
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if !sumsLine.MatchString(line) {
			return fmt.Errorf("refused: %q is not a sha256sum line", line)
		}
		if strings.HasSuffix(line, " mesh-version") || strings.HasSuffix(line, "*mesh-version") {
			version = true
		}
	}
	if !version {
		return fmt.Errorf("refused: SHA256SUMS names no mesh-version; build the release with mesh release build")
	}
	return nil
}
