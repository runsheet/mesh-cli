//go:build e2e

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nodeimage "projectmesh.io/mesh-cli/images/node"
)

// toolsImageRef is the tag TestImageToolsE2E builds the embedded
// Containerfile under: outside DefaultImageRepo, so neither the build
// nor a later prune touches a node's own mesh-node tags.
const toolsImageRef = "localhost/mesh-node-tools-e2e:test"

// TestImageToolsE2E runs scripts/image-tools-check.sh inside the node
// image as its non-root user: node and npm with `npm ci && npx tsc
// --version` in images/node/testdata/tools-fixture, a Postgres server
// started in a temp dir and `select 1`, and headless Chromium's
// `--headless --version` and a headless start (docs/e2e-sandbox.md,
// Tools check; ADR-007). The image is MESH_TOOLS_IMAGE when set, else
// built here from the embedded Containerfile as toolsImageRef. It needs
// podman and skips without it.
//
//	go test -tags e2e -run TestImageToolsE2E -v ./internal/sandbox/
func TestImageToolsE2E(t *testing.T) {
	if _, err := exec.LookPath(imageBinary); err != nil {
		t.Skip("podman not found; the image's tools are not checked")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	image := os.Getenv("MESH_TOOLS_IMAGE")
	if image == "" {
		image = toolsImageRef
		dir := t.TempDir()
		file := filepath.Join(dir, "Containerfile")
		if err := os.WriteFile(file, nodeimage.Containerfile, 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := ExecRunner(ctx, imageBinary, "build", "--tag", image, "--file", file, dir); err != nil {
			t.Fatalf("podman build %s: %v\n%s", image, err, buildTail(out))
		}
		t.Cleanup(func() { _, _ = ExecRunner(context.Background(), imageBinary, "rmi", image) })
	}

	out, err := ExecRunner(ctx, imageBinary, "run", "--rm",
		"--security-opt", "label=disable",
		"--volume", root+":/check:ro",
		image, "bash", "/check/scripts/image-tools-check.sh", "/check/images/node/testdata/tools-fixture")
	t.Logf("%s in %s:\n%s", "image-tools-check.sh", image, out)
	if err != nil {
		t.Fatalf("image-tools-check.sh in %s: %v", image, err)
	}
	for _, check := range []string{"node", "postgres", "chrome"} {
		if !strings.Contains(string(out), "PASS "+check+":") {
			t.Errorf("no PASS line for %s", check)
		}
	}
}

// buildTail is a build's last lines, enough to say why it failed.
func buildTail(out []byte) string {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}
