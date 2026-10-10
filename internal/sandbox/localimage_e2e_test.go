//go:build e2e

package sandbox

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
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

// TestLocalOnlyImageE2E starts a session from an image the node user's
// store holds only under an unresolvable registry name: the agent's
// container runs and podman's stderr says no "Trying to pull". The
// session's env carries HOME=<its temporary home>, so a podman that
// took the container's HOME for its own would look in an empty store
// and pull (the dl360, 2026-10-10); podmanEnv keeps the node's. It
// needs podman and the node image (MESH_SANDBOX_IMAGE, else
// mesh-node:dev), so it runs only with MESH_SANDBOX_E2E=1.
func TestLocalOnlyImageE2E(t *testing.T) {
	if os.Getenv("MESH_SANDBOX_E2E") != "1" {
		t.Skip("MESH_SANDBOX_E2E=1 runs a real pod")
	}
	base := os.Getenv("MESH_SANDBOX_IMAGE")
	if base == "" {
		base = DefaultImage("dev")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rt := &Podman{}
	if _, err := rt.Version(ctx); err != nil {
		t.Fatalf("podman: %v", err)
	}
	image := localOnlyImage(t, ctx, base)

	sb := &Sandbox{Runtime: rt, Config: Config{Image: image, Limits: Limits{CPUs: 1, Memory: "1g", Pids: 512}, Network: DefaultNetwork()}}
	sess, err := sb.Open("e2e/local-only", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var out, errb bytes.Buffer
	err = sess.Run(ctx, RunSpec{
		Workspace: t.TempDir(),
		Env:       []string{"HOME=/tmp/x"},
		Command:   []string{"sh", "-c", "echo local-only-ok"},
		Stdout:    &out, Stderr: &errb,
	})
	if strings.Contains(errb.String(), "Trying to pull") {
		t.Errorf("podman pulled %s; stderr:\n%s", image, errb.String())
	}
	if err != nil {
		t.Fatalf("run %s: %v\n%s", image, err, errb.String())
	}
	if !strings.Contains(out.String(), "local-only-ok") {
		t.Errorf("agent stdout = %q, want local-only-ok", out.String())
	}
	t.Logf("session ran from %s with no pull", image)
}

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
