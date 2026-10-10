package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	nodeimage "projectmesh.io/mesh-cli/images/node"
)

// image.go keeps the node image — the toolchains and agent CLIs every
// sandboxed session runs in (mesh-specs ADR 007, "The node image") — at
// the version the node runs or is about to run. `mesh node update` and
// the self-update ensure the new version's image before they drain: a
// pull that fails is the update's error, never a silent local build.

// ImageBuild is node.yaml's `sandbox.image: build`: the node image is
// built on this machine from the Containerfile the binary embeds, tagged
// with the node's version, rather than pulled.
const ImageBuild = "build"

// Runner runs a command and returns what it wrote (stdout and stderr
// together); tests inject a fake. ExecRunner is the real one.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs name with args on this machine.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// imageBinary is the container runtime the image is kept in (ADR 007).
const imageBinary = "podman"

// tagRE is an OCI tag.
var tagRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// ImageRef is the image node.yaml's `sandbox.image` names for a node at
// version: empty is DefaultImage(version); `build` is that same ref,
// built here (build is true); anything else is used as given.
func ImageRef(setting, version string) (ref string, build bool, err error) {
	switch strings.TrimSpace(setting) {
	case "", ImageBuild:
		if version != "" && !tagRE.MatchString(version) {
			return "", false, fmt.Errorf("node image: version %q is not an image tag", version)
		}
		return DefaultImage(version), setting == ImageBuild, nil
	}
	return setting, false, nil
}

// Image is what EnsureImage or BuildImage did.
type Image struct {
	// Ref is the image now on this machine.
	Ref string
	// Pulled: Ref came from its registry. Built: it was built here from
	// the embedded Containerfile (`sandbox.image: build`).
	Pulled, Built bool
	// Removed are the older mesh-node tags pruned; Kept those whose
	// removal failed (an image a container still uses), with why.
	Removed []string
	Kept    []string
	// Skipped says why no image was ensured (`sandbox: off`, no
	// runtime); Ref is empty then.
	Skipped string
}

// Summary is the image and how it came, as one line.
func (img Image) Summary() string {
	if img.Skipped != "" {
		return "no node image: " + img.Skipped
	}
	var b strings.Builder
	b.WriteString(img.Ref)
	switch {
	case img.Pulled:
		b.WriteString(" (pulled)")
	case img.Built:
		b.WriteString(" (built from the embedded Containerfile)")
	}
	if len(img.Removed) > 0 {
		fmt.Fprintf(&b, "; removed %s", strings.Join(img.Removed, ", "))
	}
	if len(img.Kept) > 0 {
		fmt.Fprintf(&b, "; not removed: %s", strings.Join(img.Kept, "; "))
	}
	return b.String()
}

// EnsureImage pulls ref and then removes every other
// ghcr.io/runsheet/mesh-node tag on this machine but ref and keep
// (the image the running node's sessions are still using). A failed pull
// is the error, and nothing is built in its place or removed.
func EnsureImage(ctx context.Context, run Runner, ref string, keep ...string) (Image, error) {
	if out, err := run(ctx, imageBinary, "pull", ref); err != nil {
		return Image{Ref: ref}, fmt.Errorf("node image %s: pull failed: %s", ref, why(out, err))
	}
	img := Image{Ref: ref, Pulled: true}
	img.Removed, img.Kept = prune(ctx, run, append([]string{ref}, keep...))
	return img, nil
}

// BuildImage builds ref on this machine from the Containerfile the binary
// embeds (images/node/Containerfile), with MESH_VERSION version, and then
// prunes as EnsureImage does. A failed build is the error.
func BuildImage(ctx context.Context, run Runner, ref, version string, keep ...string) (Image, error) {
	dir, err := os.MkdirTemp("", "mesh-node-image-")
	if err != nil {
		return Image{Ref: ref}, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, nodeimage.Containerfile, 0o644); err != nil {
		return Image{Ref: ref}, err
	}
	if out, err := run(ctx, imageBinary, "build", "--tag", ref, "--file", file,
		"--build-arg", "MESH_VERSION="+version, dir); err != nil {
		return Image{Ref: ref}, fmt.Errorf("node image %s: build failed: %s", ref, why(out, err))
	}
	img := Image{Ref: ref, Built: true}
	img.Removed, img.Kept = prune(ctx, run, append([]string{ref}, keep...))
	return img, nil
}

// PushImage builds the release's node image for linux/amd64 and
// linux/arm64 into one manifest list tagged
// ghcr.io/runsheet/mesh-node:<version> (DefaultImage), a release unit's
// step 4 — the public ghcr every node pulls from (mesh-specs ADR 017 §4,
// §7), with MESH_VERSION version as BuildImage's build does. ref, when
// given, must be that same tag: a release names no other image. It is to
// answer the multi-arch index's digest; until the push lands it answers
// "" and the build's error.
func PushImage(ctx context.Context, run Runner, ref, version string) (string, error) {
	if !tagRE.MatchString(version) {
		return "", fmt.Errorf("node image: version %q is not an image tag", version)
	}
	tag := DefaultImage(version)
	if ref != "" && ref != tag {
		return "", fmt.Errorf("node image %s: a release pushes %s", ref, tag)
	}
	ref = tag
	if out, err := run(ctx, imageBinary, "build",
		"--platform", "linux/amd64,linux/arm64",
		"--manifest", ref,
		"--build-arg", "MESH_VERSION="+version); err != nil {
		return "", fmt.Errorf("node image %s: multi-platform build failed: %s", ref, why(out, err))
	}
	return "", nil
}

// ImageDigest is ref's digest as the runtime holds it (`podman image
// inspect --format {{.Digest}}`): the runner image a sandboxed
// validation's attestation names.
func ImageDigest(ctx context.Context, run Runner, ref string) (string, error) {
	out, err := run(ctx, imageBinary, "image", "inspect", "--format", "{{.Digest}}", ref)
	if err != nil {
		return "", fmt.Errorf("node image %s: inspect failed: %s", ref, why(out, err))
	}
	d := strings.TrimSpace(string(out))
	if d == "" || d == "<none>" {
		return "", fmt.Errorf("node image %s: no digest", ref)
	}
	return d, nil
}

// DefaultImageUID is the uid (and gid) ImageUser falls back to: the
// image's declared uid:gid when it has one, else the mesh image's own
// (images/node/Containerfile's USER mesh, uid 1000) — a name such as
// "mesh" cannot be resolved to a uid without running the image.
const DefaultImageUID = 1000

// ImageUser is ref's declared user (`podman image inspect --format
// {{.Config.User}}`), as uid:gid: "1000" is uid 1000, gid defaulting to
// the uid; "1001:1002" is uid 1001, gid 1002; empty, or a name such as
// "mesh" that is not a plain uid[:gid], falls back to DefaultImageUID
// for both. A failed inspect is returned wrapped, naming ref.
func ImageUser(ctx context.Context, run Runner, ref string) (uid, gid int, err error) {
	out, err := run(ctx, imageBinary, "image", "inspect", "--format", "{{.Config.User}}", ref)
	if err != nil {
		return 0, 0, fmt.Errorf("node image %s: inspect failed: %s", ref, why(out, err))
	}
	user := strings.TrimSpace(string(out))
	if user == "" {
		return DefaultImageUID, DefaultImageUID, nil
	}
	uidPart, gidPart, hasGid := strings.Cut(user, ":")
	u, err := strconv.Atoi(uidPart)
	if err != nil {
		return DefaultImageUID, DefaultImageUID, nil
	}
	if !hasGid {
		return u, u, nil
	}
	g, err := strconv.Atoi(gidPart)
	if err != nil {
		return DefaultImageUID, DefaultImageUID, nil
	}
	return u, g, nil
}

// prune removes every mesh-node tag not in keep.
func prune(ctx context.Context, run Runner, keep []string) (removed, kept []string) {
	out, err := run(ctx, imageBinary, "images", "--filter", "reference="+DefaultImageRepo, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, []string{"list " + DefaultImageRepo + ": " + why(out, err)}
	}
	for _, line := range strings.Split(string(out), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || !strings.HasPrefix(ref, DefaultImageRepo+":") || strings.HasSuffix(ref, ":<none>") || slices.Contains(keep, ref) {
			continue
		}
		if out, err := run(ctx, imageBinary, "rmi", ref); err != nil {
			kept = append(kept, ref+": "+why(out, err))
			continue
		}
		removed = append(removed, ref)
	}
	return removed, kept
}

// why is a failed command's reason: its last line of output, else err.
func why(out []byte, err error) string {
	lines := strings.Split(string(bytes.TrimSpace(out)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return err.Error()
}
