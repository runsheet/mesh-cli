//go:build e2e

package sandbox

import (
	"context"
	"strings"
	"testing"
)

// unresolvableRef names image under a registry host that cannot
// resolve, so a podman pull of it fails and only the local store —
// holding image under its real ref — can serve a podman run against
// it.
func unresolvableRef(image string) string {
	path := image
	if i := strings.Index(image, "/"); i >= 0 {
		host := image[:i]
		if host == "localhost" || strings.ContainsAny(host, ".:") {
			path = image[i+1:]
		}
	}
	return "mesh-local.invalid/" + path
}

// localOnlyImage tags image under unresolvableRef(image) in the node
// user's store and returns the new ref, so a podman run of it can only
// be served locally. Cleanup removes the new tag alone, never image.
// Only MESH_SANDBOX_E2E=1 tests call it.
func localOnlyImage(t *testing.T, ctx context.Context, image string) string {
	t.Helper()
	ref := unresolvableRef(image)
	if out, err := ExecRunner(ctx, imageBinary, "tag", image, ref); err != nil {
		t.Fatalf("podman tag %s %s: %v: %s", image, ref, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		if out, err := ExecRunner(context.Background(), imageBinary, "rmi", ref); err != nil {
			t.Logf("podman rmi %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
		}
	})
	return ref
}

// localOnlyImage is called only by MESH_SANDBOX_E2E=1 tests yet to land.
var _ = localOnlyImage

// TestUnresolvableRef proves unresolvableRef keeps an image ref's tag
// and image name while substituting a registry host that can never
// resolve. It needs no podman and runs with MESH_SANDBOX_E2E unset.
func TestUnresolvableRef(t *testing.T) {
	cases := []struct {
		image string
		tag   string
	}{
		{"ghcr.io/runsheet/mesh-node:v0.2.335", "v0.2.335"},
		{"mesh-node:dev", "dev"},
	}
	for _, c := range cases {
		t.Run(c.image, func(t *testing.T) {
			got := unresolvableRef(c.image)

			i := strings.Index(got, "/")
			if i < 0 {
				t.Fatalf("unresolvableRef(%q) = %q, no registry host", c.image, got)
			}
			host := got[:i]

			j := strings.LastIndex(got, "/")
			rest := got[j+1:]

			if !strings.HasSuffix(host, ".invalid") {
				t.Errorf("unresolvableRef(%q) host = %q, want a .invalid suffix", c.image, host)
			}
			if host == "ghcr.io" || host == "docker.io" {
				t.Errorf("unresolvableRef(%q) host = %q, want neither ghcr.io nor docker.io", c.image, host)
			}

			nameAndTag := strings.SplitN(rest, ":", 2)
			if len(nameAndTag) != 2 {
				t.Fatalf("unresolvableRef(%q) = %q, no tag", c.image, got)
			}
			if nameAndTag[0] != "mesh-node" {
				t.Errorf("unresolvableRef(%q) image name = %q, want mesh-node", c.image, nameAndTag[0])
			}
			if nameAndTag[1] != c.tag {
				t.Errorf("unresolvableRef(%q) tag = %q, want %q", c.image, nameAndTag[1], c.tag)
			}
		})
	}
}
