package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	nodeimage "projectmesh.io/mesh-cli/images/node"
)

// fakeRuntime is the injected Runner: it answers podman's subcommands
// from a script and records every call; a build records the
// Containerfile it was given.
type fakeRuntime struct {
	calls    []string
	pullErr  bool
	buildErr bool
	rmiFail  map[string]bool
	images   []string
	built    []byte
	// digest is what a manifest push writes to its --digestfile;
	// pushErr fails the push. pushedAuth is the --authfile's contents
	// and pushedAuthFile its path, read during the push.
	digest         string
	pushErr        bool
	pushedAuth     []byte
	pushedAuthFile string
	// pulledAuth is a pull's --authfile contents, read during the pull,
	// and pulledAuthFile its path.
	pulledAuth     []byte
	pulledAuthFile string
}

func (f *fakeRuntime) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch args[0] {
	case "pull":
		for i, a := range args {
			if a == "--authfile" {
				f.pulledAuthFile = args[i+1]
				b, err := os.ReadFile(args[i+1])
				if err != nil {
					return nil, err
				}
				f.pulledAuth = b
			}
		}
		if f.pullErr {
			return []byte("Trying to pull...\nError: initializing source: unauthorized"), errors.New("exit status 125")
		}
	case "build":
		for i, a := range args {
			if a == "--file" {
				b, err := os.ReadFile(args[i+1])
				if err != nil {
					return nil, err
				}
				f.built = b
			}
		}
		if f.buildErr {
			return []byte("Error: no space left on device"), errors.New("exit status 1")
		}
	case "manifest":
		if args[1] != "push" {
			break
		}
		for i, a := range args {
			switch a {
			case "--authfile":
				f.pushedAuthFile = args[i+1]
				b, err := os.ReadFile(args[i+1])
				if err != nil {
					return nil, err
				}
				f.pushedAuth = b
			case "--digestfile":
				if f.pushErr {
					continue
				}
				if err := os.WriteFile(args[i+1], []byte(f.digest), 0o644); err != nil {
					return nil, err
				}
			}
		}
		if f.pushErr {
			return []byte("Error: writing manifest: denied"), errors.New("exit status 125")
		}
	case "images":
		return []byte(strings.Join(f.images, "\n") + "\n"), nil
	case "rmi":
		if f.rmiFail[args[1]] {
			return []byte("Error: image used by container abc"), errors.New("exit status 2")
		}
	}
	return nil, nil
}

func (f *fakeRuntime) called(sub string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "podman "+sub+" ") {
			out = append(out, c)
		}
	}
	return out
}

// (a) The image is ghcr.io/runsheet/mesh-node:<node version>; the
// setting `build` is that same tag, built; any other is used as given.
func TestImageRefIsTheNodeVersion(t *testing.T) {
	for _, c := range []struct {
		setting, version, want string
		build                  bool
	}{
		{"", "v1.2.0", "ghcr.io/runsheet/mesh-node:v1.2.0", false},
		{"", "v0.0.0-selfupdate-test.1", "ghcr.io/runsheet/mesh-node:v0.0.0-selfupdate-test.1", false},
		{"", "dev", "ghcr.io/runsheet/mesh-node:dev", false},
		{"build", "v1.2.0", "ghcr.io/runsheet/mesh-node:v1.2.0", true},
		{"registry.example.com/team/node:7", "v1.2.0", "registry.example.com/team/node:7", false},
	} {
		ref, build, err := ImageRef(c.setting, c.version)
		if err != nil || ref != c.want || build != c.build {
			t.Errorf("ImageRef(%q, %q) = %q, %v, %v; want %q, %v", c.setting, c.version, ref, build, err, c.want, c.build)
		}
	}
	for _, v := range []string{"v1.2.0+meta", "../x", ".v1"} {
		if _, _, err := ImageRef("", v); err == nil {
			t.Errorf("ImageRef(%q): no error", v)
		}
	}
}

func TestEnsureImagePulls(t *testing.T) {
	f := &fakeRuntime{images: []string{"ghcr.io/runsheet/mesh-node:v1.2.0"}}
	img, err := EnsureImage(context.Background(), f.run, "ghcr.io/runsheet/mesh-node:v1.2.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if img.Ref != "ghcr.io/runsheet/mesh-node:v1.2.0" || !img.Pulled || img.Built {
		t.Errorf("image = %+v; want pulled v1.2.0", img)
	}
	if p := f.called("pull"); len(p) != 1 || p[0] != "podman pull ghcr.io/runsheet/mesh-node:v1.2.0" {
		t.Errorf("pulls = %q", p)
	}
	if len(f.called("build")) != 0 || len(f.called("rmi")) != 0 {
		t.Errorf("built or removed: %q", f.calls)
	}
	if got := img.Summary(); got != "ghcr.io/runsheet/mesh-node:v1.2.0 (pulled)" {
		t.Errorf("summary = %q", got)
	}
}

// (b) A failed pull is the error: nothing is built in its place and
// nothing is pruned.
func TestEnsureImageFailedPullIsTheError(t *testing.T) {
	f := &fakeRuntime{pullErr: true, images: []string{"ghcr.io/runsheet/mesh-node:v1.1.0"}}
	_, err := EnsureImage(context.Background(), f.run, "ghcr.io/runsheet/mesh-node:v1.2.0", nil)
	if err == nil || !strings.Contains(err.Error(), "pull failed: Error: initializing source: unauthorized") {
		t.Fatalf("err = %v; want the pull's reason", err)
	}
	if len(f.called("build")) != 0 {
		t.Errorf("built after a failed pull: %q", f.calls)
	}
	if len(f.called("images")) != 0 || len(f.called("rmi")) != 0 {
		t.Errorf("pruned after a failed pull: %q", f.calls)
	}
}

// (c) BuildImage builds from the Containerfile the binary embeds, which
// is images/node/Containerfile byte for byte, and pulls nothing.
func TestBuildImageFromTheContainerfile(t *testing.T) {
	f := &fakeRuntime{}
	img, err := BuildImage(context.Background(), f.run, "ghcr.io/runsheet/mesh-node:v1.2.0", "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !img.Built || img.Pulled || len(f.called("pull")) != 0 {
		t.Errorf("image = %+v, calls %q; want built, no pull", img, f.calls)
	}
	b := f.called("build")
	if len(b) != 1 || !regexp.MustCompile(`^podman build --tag ghcr.io/runsheet/mesh-node:v1.2.0 --file \S+/Containerfile --build-arg MESH_VERSION=v1.2.0 \S+$`).MatchString(b[0]) {
		t.Fatalf("builds = %q", b)
	}
	want, err := os.ReadFile("../../images/node/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.built, want) {
		t.Error("the build's Containerfile is not images/node/Containerfile")
	}

	f = &fakeRuntime{buildErr: true}
	if _, err := BuildImage(context.Background(), f.run, "ghcr.io/runsheet/mesh-node:v1.2.0", "v1.2.0"); err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("err = %v; want the build's reason", err)
	}
}

// PushImage builds the node image for both platforms a sandboxed node
// may run on, so the manifest list it pushes serves either.
func TestPushImageIsMultiPlatform(t *testing.T) {
	f := &fakeRuntime{digest: testDigest}
	if _, err := PushImage(context.Background(), f.run, "ghcr.io/runsheet/mesh-node:v1.2.3", "v1.2.3", "ghs_test"); err != nil {
		t.Fatal(err)
	}
	b := f.called("build")
	if len(b) != 1 {
		t.Fatalf("builds = %q; want one", b)
	}
	if !strings.Contains(b[0], "linux/amd64") || !strings.Contains(b[0], "linux/arm64") {
		t.Errorf("build args = %q; want both linux/amd64 and linux/arm64", b[0])
	}
}

// PushImage builds the Containerfile the binary embeds, passed by
// --file: what the build reads during the call is
// images/node/Containerfile byte for byte, and the file is gone after.
func TestPushImageBuildsTheEmbeddedContainerfile(t *testing.T) {
	f := &fakeRuntime{digest: testDigest}
	if _, err := PushImage(context.Background(), f.run, "", "v1.2.3", "ghs_test"); err != nil {
		t.Fatal(err)
	}
	b := f.called("build")
	if len(b) != 1 {
		t.Fatalf("builds = %q; want one", b)
	}
	args := strings.Fields(b[0])
	i := slices.Index(args, "--file")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("build args = %q; want --file <Containerfile>", b[0])
	}
	if !slices.Contains(args, "MESH_VERSION=v1.2.3") {
		t.Errorf("build args = %q; want --build-arg MESH_VERSION=v1.2.3", b[0])
	}
	want, err := os.ReadFile("../../images/node/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.built, want) {
		t.Error("the build's --file is not images/node/Containerfile")
	}
	if _, err := os.Stat(args[i+1]); !os.IsNotExist(err) {
		t.Errorf("the Containerfile %s is left behind (stat: %v)", args[i+1], err)
	}
}

// A failed build is PushImage's error, and it answers no digest.
func TestPushImageFailedBuildIsTheError(t *testing.T) {
	f := &fakeRuntime{buildErr: true}
	digest, err := PushImage(context.Background(), f.run, "", "v1.2.3", "ghs_test")
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("err = %v; want the build's reason", err)
	}
	if digest != "" {
		t.Errorf("digest = %q; want empty", digest)
	}
}

// PushImage's build is tagged ghcr.io/runsheet/mesh-node:<version>, the
// public ghcr the fleet pulls from (mesh-specs ADR 017 §7); a ref naming
// another image, or a version that is no tag, builds nothing.
func TestPushImageTagsTheVersion(t *testing.T) {
	f := &fakeRuntime{digest: testDigest}
	if _, err := PushImage(context.Background(), f.run, "", "v0.2.300", "ghs_test"); err != nil {
		t.Fatal(err)
	}
	b := f.called("build")
	if len(b) != 1 {
		t.Fatalf("builds = %q; want one", b)
	}
	if !slices.Contains(strings.Fields(b[0]), "ghcr.io/runsheet/mesh-node:v0.2.300") {
		t.Errorf("build args = %q; want the tag ghcr.io/runsheet/mesh-node:v0.2.300", b[0])
	}

	for _, c := range []struct{ ref, version string }{
		{"ghcr.io/someone/else:v0.2.300", "v0.2.300"},
		{"", "not a tag!"},
	} {
		f := &fakeRuntime{}
		if _, err := PushImage(context.Background(), f.run, c.ref, c.version, "ghs_test"); err == nil {
			t.Errorf("PushImage(%q, %q) = nil error; want a refusal", c.ref, c.version)
		}
		if len(f.calls) != 0 {
			t.Errorf("PushImage(%q, %q) ran %q; want nothing", c.ref, c.version, f.calls)
		}
	}
}

// (d) Older mesh-node tags are removed; the new one and the one the
// running node still uses are kept; one a container holds is said.
func TestEnsureImagePrunesOlderTags(t *testing.T) {
	f := &fakeRuntime{
		images: []string{
			"ghcr.io/runsheet/mesh-node:v1.0.0",
			"ghcr.io/runsheet/mesh-node:v1.1.0",
			"ghcr.io/runsheet/mesh-node:v1.2.0",
			"ghcr.io/runsheet/mesh-node:<none>",
			"ghcr.io/runsheet/mesh-node:dev",
		},
		rmiFail: map[string]bool{"ghcr.io/runsheet/mesh-node:dev": true},
	}
	img, err := EnsureImage(context.Background(), f.run, "ghcr.io/runsheet/mesh-node:v1.2.0", nil, "ghcr.io/runsheet/mesh-node:v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ghcr.io/runsheet/mesh-node:v1.0.0"}; strings.Join(img.Removed, ",") != strings.Join(want, ",") {
		t.Errorf("removed = %q; want %q", img.Removed, want)
	}
	if len(img.Kept) != 1 || !strings.HasPrefix(img.Kept[0], "ghcr.io/runsheet/mesh-node:dev: ") {
		t.Errorf("kept = %q; want dev, in use", img.Kept)
	}
	for _, c := range f.called("rmi") {
		if strings.HasSuffix(c, ":v1.2.0") || strings.HasSuffix(c, ":v1.1.0") || strings.HasSuffix(c, "<none>") {
			t.Errorf("removed %q", c)
		}
	}
	if l := f.called("images"); len(l) != 1 || !strings.Contains(l[0], "--filter reference=ghcr.io/runsheet/mesh-node") {
		t.Errorf("list = %q", l)
	}
}

// The binary embeds images/node/Containerfile as it is.
func TestContainerfileIsEmbedded(t *testing.T) {
	want, err := os.ReadFile("../../images/node/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(nodeimage.Containerfile, want) {
		t.Error("nodeimage.Containerfile is not images/node/Containerfile")
	}
}

// The Containerfile installs what ADR 007 names, on the go.mod toolchain.
func TestContainerfileToolchains(t *testing.T) {
	cf, err := os.ReadFile("../../images/node/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	s := string(cf)
	for _, want := range []string{"git", "go.dev/dl/go", "nodejs.org/dist", "npm install -g", "python3", "python3-pip", "astral-sh/uv",
		"corepack enable", "make", "gcc", " gh", "gh --version",
		"@anthropic-ai/claude-code@", "@openai/codex@", "DISABLE_AUTOUPDATER=1", "check_for_update_on_startup = false",
		"ca-certificates", "nftables", "dpkg-query -W nftables", "useradd", "USER 1000:1000"} {
		if !strings.Contains(s, want) {
			t.Errorf("Containerfile has no %q", want)
		}
	}
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^go (\S+)$`).FindSubmatch(mod)
	if m == nil {
		t.Fatal("go.mod has no go line")
	}
	if !strings.Contains(s, "ARG GO_VERSION="+string(m[1])+"\n") {
		t.Errorf("Containerfile's GO_VERSION is not go.mod's go %s", m[1])
	}
}

// No secret is baked into the image.
func TestContainerfileHasNoSecrets(t *testing.T) {
	cf, err := os.ReadFile("../../images/node/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"GH_TOKEN", "GITHUB_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "--mount=type=secret"} {
		if bytes.Contains(cf, []byte(bad)) {
			t.Errorf("Containerfile mentions %s", bad)
		}
	}
}

// ImageDigest asks podman for the image's digest; an empty answer or a
// failed inspect is an error, for the caller's fallback to the ref.
func TestImageDigest(t *testing.T) {
	var got string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = name + " " + strings.Join(args, " ")
		return []byte("sha256:abc123\n"), nil
	}
	d, err := ImageDigest(context.Background(), run, "ghcr.io/runsheet/mesh-node:v1")
	if err != nil || d != "sha256:abc123" {
		t.Fatalf("digest = %q, %v", d, err)
	}
	if got != "podman image inspect --format {{.Digest}} ghcr.io/runsheet/mesh-node:v1" {
		t.Fatalf("ran %q", got)
	}
	empty := func(context.Context, string, ...string) ([]byte, error) { return []byte("\n"), nil }
	if _, err := ImageDigest(context.Background(), empty, "x"); err == nil {
		t.Fatal("an empty digest must be an error")
	}
	failing := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("Error: no such image"), errors.New("exit status 125")
	}
	if _, err := ImageDigest(context.Background(), failing, "x"); err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("err = %v", err)
	}
}

// ImageUser asks podman for the image's declared user and parses it as
// uid:gid; an empty answer or a name it cannot resolve to a uid falls
// back to DefaultImageUID for both, and a failed inspect is an error.
func TestImageUser(t *testing.T) {
	for _, c := range []struct {
		name, user string
		wantUID    int
		wantGID    int
	}{
		{"uid only", "1000", 1000, 1000},
		{"uid:gid same", "1000:1000", 1000, 1000},
		{"uid:gid different", "1001:1002", 1001, 1002},
		{"empty", "", DefaultImageUID, DefaultImageUID},
		{"a name", "mesh", DefaultImageUID, DefaultImageUID},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				got = name + " " + strings.Join(args, " ")
				return []byte(c.user + "\n"), nil
			}
			uid, gid, err := ImageUser(context.Background(), run, "ghcr.io/runsheet/mesh-node:v1")
			if err != nil || uid != c.wantUID || gid != c.wantGID {
				t.Fatalf("ImageUser(%q) = %d, %d, %v; want %d, %d", c.user, uid, gid, err, c.wantUID, c.wantGID)
			}
			if got != "podman image inspect --format {{.Config.User}} ghcr.io/runsheet/mesh-node:v1" {
				t.Fatalf("ran %q", got)
			}
		})
	}

	failing := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("Error: no such image"), errors.New("exit status 125")
	}
	if _, _, err := ImageUser(context.Background(), failing, "x"); err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("err = %v", err)
	}
}

const testDigest = "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

// PushImage pushes the manifest list with every platform on an auth file
// holding the token, in a temporary directory gone once it returns, and
// answers exactly the digest the push reported.
func TestPushImagePushesUnderATempAuthFileAndAnswersTheDigest(t *testing.T) {
	f := &fakeRuntime{digest: testDigest + "\n"}
	digest, err := PushImage(context.Background(), f.run, "", "v0.2.300", "ghs_secret")
	if err != nil {
		t.Fatal(err)
	}
	if digest != testDigest {
		t.Errorf("digest = %q; want %q", digest, testDigest)
	}
	p := f.called("manifest")
	if len(p) != 1 {
		t.Fatalf("pushes = %q; want one", p)
	}
	args := strings.Fields(p[0])
	ref := "ghcr.io/runsheet/mesh-node:v0.2.300"
	if !slices.Contains(args, "--all") || args[len(args)-2] != ref || args[len(args)-1] != "docker://"+ref {
		t.Errorf("push = %q; want manifest push --all ... %s docker://%s", p[0], ref, ref)
	}
	var auth struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(f.pushedAuth, &auth); err != nil {
		t.Fatalf("auth file %q: %v", f.pushedAuth, err)
	}
	if got := auth.Auths["ghcr.io"].Auth; got != base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_secret")) {
		t.Errorf("ghcr.io auth = %q; want x-access-token:ghs_secret", got)
	}
	dir := filepath.Dir(f.pushedAuthFile)
	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Errorf("auth file %s is not under the temp dir %s", f.pushedAuthFile, os.TempDir())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the auth file's dir %s is left behind (stat: %v)", dir, err)
	}
	b := f.called("build")
	if len(b) != 1 || strings.Contains(b[0], dir) {
		t.Errorf("build = %q; the auth file's dir %s is not to be its context", b, dir)
	}
}

// A refused push is PushImage's error, naming the push, answers no
// digest and leaves no temp auth dir behind; with no token nothing is
// built or pushed.
func TestPushImageFailedPushIsTheError(t *testing.T) {
	f := &fakeRuntime{pushErr: true}
	digest, err := PushImage(context.Background(), f.run, "", "v1.2.3", "ghs_secret")
	if err == nil || !strings.Contains(err.Error(), "push failed") || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v; want the push named, with its reason", err)
	}
	if digest != "" {
		t.Errorf("digest = %q; want empty", digest)
	}
	if f.pushedAuthFile == "" {
		t.Fatal("the push was given no --authfile")
	}
	if dir := filepath.Dir(f.pushedAuthFile); dir == os.TempDir() {
		t.Errorf("auth file %s is not in a temp dir of its own", f.pushedAuthFile)
	} else if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the auth file's dir %s is left behind after a failed push (stat: %v)", dir, err)
	}

	f = &fakeRuntime{digest: testDigest}
	if _, err := PushImage(context.Background(), f.run, "", "v1.2.3", " "); err == nil {
		t.Error("PushImage with no token = nil error; want a refusal")
	}
	if len(f.calls) != 0 {
		t.Errorf("PushImage with no token ran %q; want nothing", f.calls)
	}
}

// authOf decodes an auth.json's login to host as user:secret.
func authOf(t *testing.T, b []byte, host string) string {
	t.Helper()
	var doc struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("auth.json %s: %v", b, err)
	}
	raw, err := base64.StdEncoding.DecodeString(doc.Auths[host].Auth)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The node image is pulled on the broker's credential for its registry:
// the broker is asked for ghcr.io, the pull carries --authfile whose
// auth.json logs in to ghcr.io with it, and the file is gone once the
// pull returns — after a pull that succeeds and after one that fails
// (ADR-007's amendment 2026-10-13).
func TestEnsureImagePullsOnTheBrokersCredential(t *testing.T) {
	const ref = "ghcr.io/runsheet/mesh-node:dev"
	for _, fail := range []bool{false, true} {
		var asked []string
		broker := func(_ context.Context, registry string) (*RegistryCredential, error) {
			asked = append(asked, registry)
			return &RegistryCredential{Secret: "ghs_packages_read"}, nil
		}
		f := &fakeRuntime{pullErr: fail}
		_, err := EnsureImage(context.Background(), f.run, ref, broker)
		if fail != (err != nil) {
			t.Fatalf("fail=%v: err = %v", fail, err)
		}
		if !slices.Equal(asked, []string{"ghcr.io"}) {
			t.Errorf("fail=%v: broker asked %q; want ghcr.io once", fail, asked)
		}
		p := f.called("pull")
		if len(p) != 1 || p[0] != "podman pull --authfile "+f.pulledAuthFile+" "+ref {
			t.Fatalf("fail=%v: pulls = %q", fail, p)
		}
		if got := authOf(t, f.pulledAuth, "ghcr.io"); got != "x-access-token:ghs_packages_read" {
			t.Errorf("fail=%v: auth.json logs in as %q", fail, got)
		}
		if _, err := os.Stat(f.pulledAuthFile); !os.IsNotExist(err) {
			t.Errorf("fail=%v: %s left after the pull (%v)", fail, f.pulledAuthFile, err)
		}
		if _, err := os.Stat(filepath.Dir(f.pulledAuthFile)); !os.IsNotExist(err) {
			t.Errorf("fail=%v: the auth dir is left after the pull (%v)", fail, err)
		}
	}
}

// A credential with its own user logs in as it; a broker with none, or
// no broker, pulls with no --authfile, as a public image needs; a
// broker's error is named should the pull then fail.
func TestEnsureImagePullWithoutACredential(t *testing.T) {
	f := &fakeRuntime{}
	broker := func(context.Context, string) (*RegistryCredential, error) {
		return &RegistryCredential{Username: "robot", Secret: "s3cret"}, nil
	}
	if _, err := EnsureImage(context.Background(), f.run, "registry.example.com:5000/node:7", broker); err != nil {
		t.Fatal(err)
	}
	if got := authOf(t, f.pulledAuth, "registry.example.com:5000"); got != "robot:s3cret" {
		t.Errorf("auth = %q", got)
	}

	for _, b := range []RegistryBroker{nil, func(context.Context, string) (*RegistryCredential, error) { return nil, nil }} {
		f := &fakeRuntime{}
		if _, err := EnsureImage(context.Background(), f.run, "docker.io/library/alpine:3", b); err != nil {
			t.Fatal(err)
		}
		if p := f.called("pull"); len(p) != 1 || p[0] != "podman pull docker.io/library/alpine:3" {
			t.Errorf("pulls = %q; want no --authfile", p)
		}
	}

	f = &fakeRuntime{pullErr: true}
	_, err := EnsureImage(context.Background(), f.run, "ghcr.io/runsheet/mesh-node:dev",
		func(context.Context, string) (*RegistryCredential, error) { return nil, errors.New("gateway down") })
	if err == nil || !strings.Contains(err.Error(), "unauthorized (registry credential: gateway down)") {
		t.Errorf("err = %v", err)
	}
}

func TestRegistryHost(t *testing.T) {
	for ref, want := range map[string]string{
		"ghcr.io/runsheet/mesh-node:dev":   "ghcr.io",
		"localhost/node:1":                 "localhost",
		"registry.example.com:5000/node:7": "registry.example.com:5000",
		"library/alpine:3":                 "docker.io",
		"alpine":                           "docker.io",
	} {
		if got := RegistryHost(ref); got != want {
			t.Errorf("RegistryHost(%q) = %q; want %q", ref, got, want)
		}
	}
}
