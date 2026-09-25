package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// fakeRuntime answers podman's subcommands from a script and records
// every call; a build records the Containerfile it was given.
type fakeRuntime struct {
	calls    []string
	pullErr  bool
	buildErr bool
	rmiFail  map[string]bool
	images   []string
	built    []byte
}

func (f *fakeRuntime) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch args[0] {
	case "pull":
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
	case "images":
		return []byte(strings.Join(f.images, "\n") + "\n"), nil
	case "rmi":
		if f.rmiFail[args[1]] {
			return []byte("Error: image used by container abc"), errors.New("exit status 2")
		}
	}
	return nil, nil
}

func (f *fakeRuntime) runtime(t *testing.T) ImageRuntime {
	return ImageRuntime{Binary: "podman", Run: f.run, TempDir: t.TempDir()}
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

func TestRefIsTheNodeVersion(t *testing.T) {
	for v, want := range map[string]string{
		"v1.2.0":                   "ghcr.io/projectmesh-io/mesh-node:v1.2.0",
		"v0.0.0-selfupdate-test.1": "ghcr.io/projectmesh-io/mesh-node:v0.0.0-selfupdate-test.1",
		"dev":                      "ghcr.io/projectmesh-io/mesh-node:dev",
	} {
		got, err := Ref(v)
		if err != nil || got != want {
			t.Errorf("Ref(%q) = %q, %v; want %q", v, got, err, want)
		}
	}
	for _, v := range []string{"", "v1.2.0+meta", "../x", ".v1"} {
		if _, err := Ref(v); err == nil {
			t.Errorf("Ref(%q): no error", v)
		}
	}
}

func TestEnsureImagePullSkipsBuild(t *testing.T) {
	f := &fakeRuntime{images: []string{"ghcr.io/projectmesh-io/mesh-node:v1.2.0"}}
	img, err := EnsureImage(context.Background(), f.runtime(t), "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if img.Ref != "ghcr.io/projectmesh-io/mesh-node:v1.2.0" || !img.Pulled || img.Built {
		t.Errorf("image = %+v; want pulled v1.2.0", img)
	}
	if p := f.called("pull"); len(p) != 1 || p[0] != "podman pull ghcr.io/projectmesh-io/mesh-node:v1.2.0" {
		t.Errorf("pulls = %q", p)
	}
	if b := f.called("build"); len(b) != 0 {
		t.Errorf("built after a successful pull: %q", b)
	}
	if len(img.Removed) != 0 || len(f.called("rmi")) != 0 {
		t.Errorf("removed the image in use: %q", f.calls)
	}
}

func TestEnsureImagePullFailsBuildsFromContainerfile(t *testing.T) {
	f := &fakeRuntime{pullErr: true}
	img, err := EnsureImage(context.Background(), f.runtime(t), "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if img.Pulled || !img.Built || !strings.Contains(img.PullError, "unauthorized") {
		t.Errorf("image = %+v; want built after a failed pull", img)
	}
	b := f.called("build")
	if len(b) != 1 {
		t.Fatalf("builds = %q", b)
	}
	if !regexp.MustCompile(`^podman build --tag ghcr.io/projectmesh-io/mesh-node:v1.2.0 --file \S+/Containerfile --build-arg MESH_VERSION=v1.2.0 \S+$`).MatchString(b[0]) {
		t.Errorf("build = %q", b[0])
	}
	// what was built is images/node/Containerfile, as the binary embeds it
	want, err := os.ReadFile("../../images/node/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.built, want) {
		t.Error("the build's Containerfile is not images/node/Containerfile")
	}
}

func TestEnsureImageBuildFailureIsTheError(t *testing.T) {
	f := &fakeRuntime{pullErr: true, buildErr: true}
	_, err := EnsureImage(context.Background(), f.runtime(t), "v1.2.0")
	if err == nil || !strings.Contains(err.Error(), "unauthorized") || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("err = %v; want the pull's and the build's reasons", err)
	}
	if len(f.called("images")) != 0 {
		t.Error("pruned without an image in use")
	}
}

func TestEnsureImagePrunesOlderTags(t *testing.T) {
	f := &fakeRuntime{
		images: []string{
			"ghcr.io/projectmesh-io/mesh-node:v1.0.0",
			"ghcr.io/projectmesh-io/mesh-node:v1.1.0",
			"ghcr.io/projectmesh-io/mesh-node:v1.2.0",
			"ghcr.io/projectmesh-io/mesh-node:<none>",
			"ghcr.io/projectmesh-io/mesh-node:dev",
		},
		rmiFail: map[string]bool{"ghcr.io/projectmesh-io/mesh-node:dev": true},
	}
	img, err := EnsureImage(context.Background(), f.runtime(t), "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	wantRemoved := []string{"ghcr.io/projectmesh-io/mesh-node:v1.0.0", "ghcr.io/projectmesh-io/mesh-node:v1.1.0"}
	if strings.Join(img.Removed, ",") != strings.Join(wantRemoved, ",") {
		t.Errorf("removed = %q; want %q", img.Removed, wantRemoved)
	}
	if len(img.Kept) != 1 || !strings.HasPrefix(img.Kept[0], "ghcr.io/projectmesh-io/mesh-node:dev: ") {
		t.Errorf("kept = %q; want dev, in use", img.Kept)
	}
	for _, c := range f.called("rmi") {
		if strings.HasSuffix(c, ":v1.2.0") || strings.HasSuffix(c, "<none>") {
			t.Errorf("removed %q", c)
		}
	}
	if l := f.called("images"); len(l) != 1 || !strings.Contains(l[0], "--filter reference=ghcr.io/projectmesh-io/mesh-node") {
		t.Errorf("list = %q", l)
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
		"@anthropic-ai/claude-code@", "@openai/codex@", "ca-certificates", "nftables", "nft --version", "useradd", "USER mesh"} {
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
