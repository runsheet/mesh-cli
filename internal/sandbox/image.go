// Package sandbox is what a node needs to run sessions in containers
// (mesh-specs ADR 007). image.go keeps the node image — the toolchains
// and agent CLIs every sandboxed session runs in — at the node's own
// version.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	nodeimage "projectmesh.io/mesh-cli/images/node"
)

// Repository is where release-tag.yml pushes the node image; the tag is
// the node's version (internal/version).
const Repository = "ghcr.io/projectmesh-io/mesh-node"

// ErrNoRuntime is EnsureImage on a machine without the container runtime:
// a node there runs with `sandbox: off` and needs no image.
var ErrNoRuntime = errors.New("no container runtime")

// Runner runs the container runtime's binary with args and returns what
// it wrote (stdout and stderr together); tests inject a fake.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Runtime is the container runtime the node image is kept in.
type Runtime struct {
	// Binary is the runtime's command, "podman" (ADR 007).
	Binary string
	// Run runs it; nil is exec.
	Run Runner
	// Containerfile is what a local build builds; nil is the one the
	// binary embeds (images/node/Containerfile).
	Containerfile []byte
	// TempDir is where a local build's context is written; empty is
	// os.TempDir.
	TempDir string
}

// DefaultRuntime is podman from PATH, or ErrNoRuntime.
func DefaultRuntime() (Runtime, error) {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return Runtime{}, fmt.Errorf("%w: podman is not on PATH", ErrNoRuntime)
	}
	return Runtime{Binary: bin}, nil
}

// Image is what EnsureImage did.
type Image struct {
	// Ref is the image in use, Repository:<version>.
	Ref string
	// Pulled: Ref came from the registry. Built: the pull failed and Ref
	// was built from the Containerfile; PullError is why the pull failed.
	Pulled, Built bool
	PullError     string
	// Removed are the older mesh-node tags pruned; Kept those whose
	// removal failed (an image a container still uses), with why.
	Removed []string
	Kept    []string
}

// Summary is the image in use and how it came, as one line.
func (img Image) Summary() string {
	var b strings.Builder
	b.WriteString(img.Ref)
	switch {
	case img.Pulled:
		b.WriteString(" (pulled)")
	case img.Built:
		fmt.Fprintf(&b, " (built from the Containerfile; the pull failed: %s)", img.PullError)
	}
	if len(img.Removed) > 0 {
		fmt.Fprintf(&b, "; removed %s", strings.Join(img.Removed, ", "))
	}
	if len(img.Kept) > 0 {
		fmt.Fprintf(&b, "; not removed: %s", strings.Join(img.Kept, "; "))
	}
	return b.String()
}

// tagRE is an OCI tag.
var tagRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// Ref is the node image for version: Repository:<version>, the version
// as it is (v1.2.0 is mesh-node:v1.2.0, a dev build mesh-node:dev).
func Ref(version string) (string, error) {
	if !tagRE.MatchString(version) {
		return "", fmt.Errorf("node image: version %q is not an image tag", version)
	}
	return Repository + ":" + version, nil
}

// EnsureImage makes the node image for version the one on this machine:
// it pulls Repository:<version>, and when the pull fails builds that tag
// from the Containerfile; then it removes every other mesh-node tag. A
// failed build is the error; a tag that cannot be removed is kept and
// said on Image.Kept.
func EnsureImage(ctx context.Context, rt Runtime, version string) (Image, error) {
	ref, err := Ref(version)
	if err != nil {
		return Image{}, err
	}
	img := Image{Ref: ref}
	if out, err := rt.run(ctx, "pull", ref); err == nil {
		img.Pulled = true
	} else {
		img.PullError = why(out, err)
		if err := rt.build(ctx, ref, version); err != nil {
			return img, fmt.Errorf("node image %s: pull failed (%s); build failed: %w", ref, img.PullError, err)
		}
		img.Built = true
	}
	img.Removed, img.Kept = rt.prune(ctx, ref)
	return img, nil
}

// build builds ref from the Containerfile in a temporary context.
func (rt Runtime) build(ctx context.Context, ref, version string) error {
	dir, err := os.MkdirTemp(rt.TempDir, "mesh-node-image-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	cf := rt.Containerfile
	if cf == nil {
		cf = nodeimage.Containerfile
	}
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, cf, 0o644); err != nil {
		return err
	}
	out, err := rt.run(ctx, "build", "--tag", ref, "--file", file,
		"--build-arg", "MESH_VERSION="+version, dir)
	if err != nil {
		return errors.New(why(out, err))
	}
	return nil
}

// prune removes every mesh-node tag but keep.
func (rt Runtime) prune(ctx context.Context, keep string) (removed, kept []string) {
	out, err := rt.run(ctx, "images", "--filter", "reference="+Repository, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, []string{"list " + Repository + ": " + why(out, err)}
	}
	for _, line := range strings.Split(string(out), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || ref == keep || !strings.HasPrefix(ref, Repository+":") || strings.HasSuffix(ref, ":<none>") {
			continue
		}
		if out, err := rt.run(ctx, "rmi", ref); err != nil {
			kept = append(kept, ref+": "+why(out, err))
			continue
		}
		removed = append(removed, ref)
	}
	return removed, kept
}

func (rt Runtime) run(ctx context.Context, args ...string) ([]byte, error) {
	bin := rt.Binary
	if bin == "" {
		bin = "podman"
	}
	if rt.Run != nil {
		return rt.Run(ctx, bin, args...)
	}
	return exec.CommandContext(ctx, bin, args...).CombinedOutput()
}

// why is a failed command's reason: its last line of output, else err.
func why(out []byte, err error) string {
	lines := strings.Split(string(bytes.TrimSpace(out)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return err.Error()
}
