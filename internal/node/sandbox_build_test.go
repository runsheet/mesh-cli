package node

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// node.yaml's sandbox.image: build asks EnsureImage to build the node
// image rather than pull it (adr-017-releases-ship-from-a-node §7: the
// workaround for a node with no credential for a private image, kept
// here for a machine that cannot reach ghcr even though the fleet
// pulls the public image instead).
func TestSandboxImageBuildRunsPodmanBuild(t *testing.T) {
	ctx := context.Background()

	cfg := sandboxOf(t, "sandbox:\n  image: build\n")
	if cfg.Image != sandbox.ImageBuild {
		t.Fatalf("Image = %q, want %q", cfg.Image, sandbox.ImageBuild)
	}

	r := &imageRunner{}
	if _, err := cfg.EnsureImage(ctx, r.run, "v1.1.0", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if b := r.sub("build"); len(b) != 1 {
		t.Fatalf("builds = %q; want exactly one podman build", b)
	}
	if p := r.sub("pull"); len(p) != 0 {
		t.Fatalf("pulls = %q; want no podman pull", p)
	}
}
