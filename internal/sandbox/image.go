package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

// RegistryCredential is a login to one image registry: the user and the
// secret podman sends it. A GitHub installation token's user is
// x-access-token (RegistryTokenUser).
type RegistryCredential struct {
	Username string
	Secret   string
}

// RegistryTokenUser is the user a GitHub token logs in to ghcr.io as.
const RegistryTokenUser = "x-access-token"

// RegistryBroker asks the broker — the platform — for a credential that
// reads images from registry (a host, `ghcr.io`). nil with no error is
// none to give: the pull goes as podman's own login has it, which for a
// public image is anonymous.
type RegistryBroker func(ctx context.Context, registry string) (*RegistryCredential, error)

// RegistryHost is the registry ref is pulled from: its first path
// component when that names a host (a dot, a port or localhost), else
// docker.io, as podman resolves a bare name.
func RegistryHost(ref string) string {
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || rest == "" {
		return "docker.io"
	}
	if strings.ContainsAny(host, ".:") || host == "localhost" {
		return host
	}
	return "docker.io"
}

// EnsureImage pulls ref and then removes every other
// ghcr.io/runsheet/mesh-node tag on this machine but ref and keep
// (the image the running node's sessions are still using). A failed pull
// is the error, and nothing is built in its place or removed.
//
// The pull is on the credential broker gives for ref's registry, as
// PushImage's push is on its token: an auth.json in a temporary directory
// of its own, passed by --authfile and removed once the pull returns,
// whatever it did — never in the image or a build context. A nil broker,
// or one with no credential for the registry, pulls without --authfile;
// a broker that errs is named in the pull's error should the pull then
// fail.
func EnsureImage(ctx context.Context, run Runner, ref string, broker RegistryBroker, keep ...string) (Image, error) {
	if err := pullImage(ctx, run, ref, broker); err != nil {
		return Image{Ref: ref}, err
	}
	img := Image{Ref: ref, Pulled: true}
	img.Removed, img.Kept = prune(ctx, run, append([]string{ref}, keep...))
	return img, nil
}

// pullImage is `podman pull ref`, with --authfile when broker has a
// credential for ref's registry.
func pullImage(ctx context.Context, run Runner, ref string, broker RegistryBroker) error {
	var cred *RegistryCredential
	var brokerErr error
	if broker != nil {
		cred, brokerErr = broker(ctx, RegistryHost(ref))
	}
	args := []string{"pull"}
	if cred != nil && cred.Secret != "" {
		dir, err := os.MkdirTemp("", "mesh-node-pull-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		auth, err := registryAuthAs(RegistryHost(ref), cred.Username, cred.Secret)
		if err != nil {
			return err
		}
		authFile := filepath.Join(dir, "auth.json")
		if err := os.WriteFile(authFile, auth, 0o600); err != nil {
			return err
		}
		args = append(args, "--authfile", authFile)
	}
	if out, err := run(ctx, imageBinary, append(args, ref)...); err != nil {
		reason := why(out, err)
		if brokerErr != nil {
			reason += " (registry credential: " + brokerErr.Error() + ")"
		}
		return fmt.Errorf("node image %s: pull failed: %s", ref, reason)
	}
	return nil
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
// §7) — pushes it, and answers the multi-arch index's digest. ref, when
// given, must be that same tag: a release names no other image. The
// build is of the Containerfile the binary embeds
// (images/node/Containerfile), written to a temporary file passed by
// --file and removed after, as BuildImage's. The push is on token, the
// App-minted GitHub token the release item is issued (`packages: write`,
// ADR 017 §4): written as an auth.json under a temporary directory of its
// own, never the build context, passed by --authfile, and removed with
// the digest file once the push returns.
func PushImage(ctx context.Context, run Runner, ref, version, token string) (string, error) {
	if !tagRE.MatchString(version) {
		return "", fmt.Errorf("node image: version %q is not an image tag", version)
	}
	tag := DefaultImage(version)
	if ref != "" && ref != tag {
		return "", fmt.Errorf("node image %s: a release pushes %s", ref, tag)
	}
	ref = tag
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("node image %s: no registry token to push with", ref)
	}
	dir, err := os.MkdirTemp("", "mesh-node-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, nodeimage.Containerfile, 0o644); err != nil {
		return "", err
	}
	if out, err := run(ctx, imageBinary, "build",
		"--platform", "linux/amd64,linux/arm64",
		"--manifest", ref, "--file", file,
		"--build-arg", "MESH_VERSION="+version, dir); err != nil {
		return "", fmt.Errorf("node image %s: multi-platform build failed: %s", ref, why(out, err))
	}
	return pushManifest(ctx, run, ref, token)
}

// pushManifest pushes the manifest list ref, every platform's image with
// it, on token, and answers the digest podman wrote. The auth file and
// the digest file lie in a temporary directory removed before it
// returns, whatever the push did.
func pushManifest(ctx context.Context, run Runner, ref, token string) (string, error) {
	dir, err := os.MkdirTemp("", "mesh-node-push-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	auth, err := registryAuth(ref, token)
	if err != nil {
		return "", err
	}
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, auth, 0o600); err != nil {
		return "", err
	}
	digestFile := filepath.Join(dir, "digest")
	if out, err := run(ctx, imageBinary, "manifest", "push", "--all",
		"--authfile", authFile, "--digestfile", digestFile,
		ref, "docker://"+ref); err != nil {
		return "", fmt.Errorf("node image %s: push failed: %s", ref, why(out, err))
	}
	b, err := os.ReadFile(digestFile)
	if err != nil {
		return "", fmt.Errorf("node image %s: pushed, but no digest: %w", ref, err)
	}
	d := strings.TrimSpace(string(b))
	if !strings.HasPrefix(d, "sha256:") {
		return "", fmt.Errorf("node image %s: pushed, but the digest %q is not sha256:…", ref, d)
	}
	return d, nil
}

// registryAuth is the containers auth.json logging in to ref's registry
// with a GitHub installation token, whose user is x-access-token.
func registryAuth(ref, token string) ([]byte, error) {
	host, _, _ := strings.Cut(ref, "/")
	return registryAuthAs(host, RegistryTokenUser, token)
}

// registryAuthAs is the containers auth.json logging in to host as user
// with secret; an empty user is RegistryTokenUser.
func registryAuthAs(host, user, secret string) ([]byte, error) {
	if user == "" {
		user = RegistryTokenUser
	}
	return json.Marshal(map[string]any{"auths": map[string]any{
		host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))},
	}})
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
