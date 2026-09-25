package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/selfupdate"
	"projectmesh.io/mesh-cli/internal/selfupdate/selfupdatetest"
)

const updateTo = "v1.2.0"

// releaseSource is a fake release: its assets by name, for any version it
// is asked for, and the order they were asked for.
type releaseSource map[string][]byte

func (s releaseSource) Asset(_ context.Context, version, name string) ([]byte, error) {
	if b, ok := s[name]; ok {
		return b, nil
	}
	return nil, &selfupdate.Error{Kind: selfupdate.ErrMissingAsset, Reason: "release " + version + " has no " + name}
}

// updateFixture is a machine running mesh v1.1.0 at exe, an older binary
// at exe.prev, and a release of updateTo signed on a virtual Sigstore.
type updateFixture struct {
	u      *nodeUpdate
	exe    string
	state  *selfupdate.StateFile
	src    releaseSource
	vs     *ca.VirtualSigstore
	bin    []byte
	out    *bytes.Buffer
	drains int
	// images are the versions whose node image was ensured, each with
	// the drains asked before it
	images   []string
	imageErr error
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "mesh")
	if err := os.WriteFile(exe, []byte("mesh v1.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe+".prev", []byte("mesh v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	bin := []byte("mesh " + updateTo)
	sums := sumsOf(map[string][]byte{"mesh-linux-amd64": bin, "mesh-darwin-arm64": []byte("darwin")})
	f := &updateFixture{
		exe:   exe,
		state: &selfupdate.StateFile{Path: filepath.Join(dir, "update.json")},
		vs:    vs,
		bin:   bin,
		out:   &bytes.Buffer{},
		src: releaseSource{
			"mesh-linux-amd64":     bin,
			selfupdate.SumsAsset:   sums,
			selfupdate.BundleAsset: selfupdatetest.SignSums(t, vs, selfupdate.IdentityPrefix+updateTo, selfupdate.Issuer, sums),
		},
	}
	f.u = &nodeUpdate{
		Updater: &node.ReleaseUpdater{
			Source: f.src, State: f.state, Exe: exe, GOOS: "linux", GOARCH: "amd64",
			// the release policy, on the virtual Sigstore's roots
			Policy: func(v string) selfupdate.Policy {
				p := selfupdate.ReleasePolicy(v)
				p.TrustedMaterial = func() (root.TrustedMaterial, error) { return vs, nil }
				return p.WithoutSCTs()
			},
		},
		Latest:  func(context.Context) (string, error) { return updateTo, nil },
		Running: "v1.1.0",
		Run:     node.RunState{Dir: dir},
		Drain: func(node.RunState, io.Writer) (bool, error) {
			f.drains++
			return true, nil
		},
		Image: func(_ context.Context, v string) (sandbox.Image, error) {
			f.images = append(f.images, fmt.Sprintf("%s after %d drains", v, f.drains))
			if f.imageErr != nil {
				return sandbox.Image{}, f.imageErr
			}
			return sandbox.Image{Ref: sandbox.DefaultImage(v), Pulled: true,
				Removed: []string{sandbox.DefaultImage("v1.0.0")}}, nil
		},
		TempDir: t.TempDir(),
		Out:     f.out,
	}
	return f
}

func sumsOf(files map[string][]byte) []byte {
	var buf bytes.Buffer
	for _, name := range []string{"mesh-darwin-arm64", "mesh-linux-amd64"} {
		s := sha256.Sum256(files[name])
		fmt.Fprintf(&buf, "%s  %s\n", hex.EncodeToString(s[:]), name)
	}
	return buf.Bytes()
}

// unchanged fails t unless the executable and .prev are what the fixture
// started with.
func (f *updateFixture) unchanged(t *testing.T) {
	t.Helper()
	if b, err := os.ReadFile(f.exe); err != nil || string(b) != "mesh v1.1.0" {
		t.Errorf("executable = %q, %v; want it unchanged", b, err)
	}
	if b, err := os.ReadFile(f.exe + ".prev"); err != nil || string(b) != "mesh v1.0.0" {
		t.Errorf(".prev = %q, %v; want it unchanged", b, err)
	}
}

// `mesh node update <v>` verifies, drains the running node, and only then
// swaps: when the drain is asked the executable is still the old one, and
// afterwards the new one is in place with the old kept as .prev and the
// update pending, for the first heartbeat to confirm.
func TestNodeUpdateDrainsBeforeItSwaps(t *testing.T) {
	f := newUpdateFixture(t)
	f.u.Drain = func(node.RunState, io.Writer) (bool, error) {
		f.drains++
		f.unchanged(t)
		return true, nil
	}
	if err := f.u.run(context.Background(), updateTo, false); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out)
	}
	if f.drains != 1 {
		t.Fatalf("drained %d times, want once", f.drains)
	}
	if b, _ := os.ReadFile(f.exe); !bytes.Equal(b, f.bin) {
		t.Fatalf("executable = %q, want the release", b)
	}
	if b, _ := os.ReadFile(f.exe + ".prev"); string(b) != "mesh v1.1.0" {
		t.Fatalf(".prev = %q, want the binary it replaced", b)
	}
	s, err := f.state.Load()
	if err != nil || !s.Pending() || s.From != "v1.1.0" || s.To != updateTo {
		t.Fatalf("update state = %+v, %v; want v1.1.0 → %s pending", s, err, updateTo)
	}
	if out := f.out.String(); !strings.Contains(out, "its service (systemd --user or launchd) starts "+updateTo) {
		t.Fatalf("output does not say how the service restarts:\n%s", out)
	}
	// the new version's node image was pulled before the drain
	if want := updateTo + " after 0 drains"; strings.Join(f.images, ",") != want {
		t.Fatalf("images ensured = %q, want %q", f.images, want)
	}
	if out := f.out.String(); !strings.Contains(out, "Node image: ghcr.io/projectmesh-io/mesh-node:"+updateTo+" (pulled); removed ghcr.io/projectmesh-io/mesh-node:v1.0.0") {
		t.Fatalf("output does not name the node image:\n%s", out)
	}
}

// A node image that cannot be pulled is the command's error: nothing is
// drained or swapped, and the refusal is recorded.
func TestNodeUpdateFailedImagePullChangesNothing(t *testing.T) {
	f := newUpdateFixture(t)
	f.imageErr = errors.New("node image ghcr.io/projectmesh-io/mesh-node:" + updateTo + ": pull failed: Error: initializing source: unauthorized")
	err := f.u.run(context.Background(), updateTo, false)
	if err == nil || !strings.Contains(err.Error(), "pull failed") || !strings.Contains(err.Error(), "not updated") {
		t.Fatalf("update = %v; want the pull's failure", err)
	}
	if f.drains != 0 {
		t.Fatal("drained after a failed pull")
	}
	f.unchanged(t)
	s, lerr := f.state.Load()
	if lerr != nil || s.Pending() || !strings.Contains(s.LastError, "pull failed") {
		t.Fatalf("update state = %+v, %v; want the failure recorded, nothing pending", s, lerr)
	}
}

// With node.yaml's `sandbox: off` the command's image step pulls nothing
// (it would need podman to).
func TestNodeUpdateSandboxOffPullsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // no podman to call
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte("sandbox: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	img, err := ensureNodeImage(context.Background(), updateTo)
	if err != nil || img.Skipped != "sandbox: off" || img.Ref != "" {
		t.Fatalf("image = %+v, %v; want skipped for sandbox: off", img, err)
	}
}

// With no version it takes the latest release.
func TestNodeUpdateLatest(t *testing.T) {
	f := newUpdateFixture(t)
	if err := f.u.run(context.Background(), "", false); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out)
	}
	if b, _ := os.ReadFile(f.exe); !bytes.Equal(b, f.bin) {
		t.Fatalf("executable = %q, want the latest release", b)
	}
	if !strings.Contains(f.out.String(), "The latest release is "+updateTo) {
		t.Fatalf("output:\n%s", f.out)
	}
}

// A refusal exits non-zero with the reason, drains nothing and leaves the
// executable and .prev as they were.
func TestNodeUpdateRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(f *updateFixture, t *testing.T)
		reason string
	}{
		{
			name: "tampered checksum",
			tamper: func(f *updateFixture, t *testing.T) {
				f.src["mesh-linux-amd64"] = []byte("mesh " + updateTo + " with something added")
			},
			reason: "mesh-linux-amd64 sha256",
		},
		{
			name: "another workflow's signature",
			tamper: func(f *updateFixture, t *testing.T) {
				id := "https://github.com/projectmesh-io/mesh-cli/.github/workflows/ci.yml@refs/tags/" + updateTo
				f.src[selfupdate.BundleAsset] = selfupdatetest.SignSums(t, f.vs, id, selfupdate.Issuer, f.src[selfupdate.SumsAsset])
			},
			reason: "SHA256SUMS is not signed by release-tag.yml on " + updateTo,
		},
		{
			name: "another tag's signature",
			tamper: func(f *updateFixture, t *testing.T) {
				f.src[selfupdate.BundleAsset] = selfupdatetest.SignSums(t, f.vs, selfupdate.IdentityPrefix+"v1.1.9", selfupdate.Issuer, f.src[selfupdate.SumsAsset])
			},
			reason: "SHA256SUMS is not signed by release-tag.yml on " + updateTo,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			tc.tamper(f, t)
			err := f.u.run(context.Background(), updateTo, false)
			if err == nil || !strings.Contains(err.Error(), "refused "+updateTo+": "+tc.reason) {
				t.Fatalf("update = %v, want a refusal with %q", err, tc.reason)
			}
			if f.drains != 0 {
				t.Fatal("a refused release drained the node")
			}
			f.unchanged(t)
			if s, _ := f.state.Load(); s.To != "" {
				t.Fatalf("a refusal left update state %+v", s)
			}
		})
	}
}

// --verify-only verifies into a temporary directory: nothing is drained
// or swapped, even for the version already running.
func TestNodeUpdateVerifyOnly(t *testing.T) {
	f := newUpdateFixture(t)
	f.u.Running = updateTo
	if err := f.u.run(context.Background(), updateTo, true); err != nil {
		t.Fatalf("verify-only: %v\n%s", err, f.out)
	}
	if f.drains != 0 {
		t.Fatal("--verify-only drained the node")
	}
	f.unchanged(t)
	got, _ := filepath.Glob(filepath.Join(f.u.TempDir, "mesh-update-*", "mesh-linux-amd64"))
	if len(got) != 1 {
		t.Fatalf("verified binary not written to the temp dir: %v", got)
	}
	if b, _ := os.ReadFile(got[0]); !bytes.Equal(b, f.bin) {
		t.Fatalf("verified binary = %q", b)
	}
	if !strings.Contains(f.out.String(), "nothing swapped") {
		t.Fatalf("output:\n%s", f.out)
	}
	if len(f.images) != 0 {
		t.Fatalf("--verify-only ensured an image: %q", f.images)
	}

	// and a bad release is refused the same way
	f.src["mesh-linux-amd64"] = []byte("tampered")
	if err := f.u.run(context.Background(), updateTo, true); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("verify-only of a tampered release = %v", err)
	}
	f.unchanged(t)
}

// The version already running is left alone.
func TestNodeUpdateAlreadyRunning(t *testing.T) {
	f := newUpdateFixture(t)
	f.u.Running = updateTo
	if err := f.u.run(context.Background(), updateTo, false); err != nil {
		t.Fatal(err)
	}
	if f.drains != 0 {
		t.Fatal("drained for the running version")
	}
	f.unchanged(t)
	// the running version's image is ensured all the same
	if want := updateTo + " after 0 drains"; strings.Join(f.images, ",") != want {
		t.Fatalf("images = %q, want %q", f.images, want)
	}
}

// With no node running there is nothing to drain; the update goes ahead.
func TestDrainIfRunningWithNoNode(t *testing.T) {
	state := node.RunState{Dir: t.TempDir()}
	if running, err := drainIfRunning(state, io.Discard); running || err != nil {
		t.Fatalf("drainIfRunning with no pidfile = %v, %v", running, err)
	}
	if err := state.Started(1 << 22); err != nil {
		t.Fatal(err)
	}
	if running, err := drainIfRunning(state, io.Discard); running || err != nil {
		t.Fatalf("drainIfRunning with a dead pid = %v, %v", running, err)
	}
}

func TestNodeUpdateHelp(t *testing.T) {
	buf := &bytes.Buffer{}
	rootCmd.SetArgs([]string{"node", "update", "--help"})
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	defer func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	}()
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mesh node update [version]", "--verify-only", "node image", "ghcr.io/projectmesh-io/mesh-node:<version>", "sandbox.image", "build"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, buf)
		}
	}
}
