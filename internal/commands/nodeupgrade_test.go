//go:build !windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// fakeService is the restart: it records each call and what the
// executable held at that moment.
type fakeService struct {
	exe   string
	calls []string
	err   error
}

func (s *fakeService) Restart(_ context.Context, _ node.RunState, out io.Writer) error {
	b, _ := os.ReadFile(s.exe)
	head, _, _ := strings.Cut(string(b), "\n")
	s.calls = append(s.calls, "restart with "+head)
	return s.err
}

// noDrain fails t if anything signals a drain while it is in force.
func noDrain(t *testing.T) {
	t.Helper()
	was := signalDrain
	signalDrain = func(pid int) error {
		t.Errorf("drain signalled (pid %d): an upgrade must not drain", pid)
		return nil
	}
	t.Cleanup(func() { signalDrain = was })
}

// upgradeFixture is the update fixture's machine (mesh v1.1.0 at exe,
// v1.0.0 at .prev, a signed release of updateTo) under `mesh node
// upgrade`, with a fake restart.
func upgradeFixture(t *testing.T) (*nodeUpgrade, *updateFixture, *fakeService) {
	t.Helper()
	f := newUpdateFixture(t)
	svc := &fakeService{exe: f.exe}
	u := &nodeUpgrade{
		Updater: f.u.Updater,
		Latest:  f.u.Latest,
		Running: f.u.Running,
		Run:     f.u.Run,
		Version: binaryVersion,
		Image:   f.u.Image,
		Service: svc,
		Out:     f.out,
	}
	return u, f, svc
}

// a local build that answers `version` as mesh does
func fakeBinary(t *testing.T, say string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mesh-new")
	script := "#!/bin/sh\n# the new build\n[ \"$1\" = version ] && { echo \"" + say + "\"; exit 0; }\nexit 2\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// (a) `mesh node upgrade --binary <fake>` checks `<fake> version`, swaps
// the executable — the new file in place, .prev holding the old — marks
// the update pending for the rollback, and restarts the node; nothing
// drains.
func TestNodeUpgradeBinarySwapsAndRestartsWithoutDraining(t *testing.T) {
	noDrain(t)
	u, f, svc := upgradeFixture(t)
	bin := fakeBinary(t, "mesh v9.9.9")
	if err := u.run(context.Background(), "", bin, false); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, f.out)
	}
	want, _ := os.ReadFile(bin)
	if got, err := os.ReadFile(f.exe); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("executable = %q, %v; want the new build", got, err)
	}
	if got, _ := os.ReadFile(selfupdate.PrevPath(f.exe)); string(got) != "mesh v1.1.0" {
		t.Fatalf(".prev = %q, want the old binary", got)
	}
	if len(svc.calls) != 1 || svc.calls[0] != "restart with #!/bin/sh" {
		t.Fatalf("restarts %q: want one, after the swap", svc.calls)
	}
	s, err := f.state.Load()
	if err != nil || !s.Pending() || s.From != "v1.1.0" || s.To != "v9.9.9" {
		t.Fatalf("update state %+v, %v: want v1.1.0 → v9.9.9 pending", s, err)
	}
	if len(f.images) != 0 || f.drains != 0 {
		t.Fatalf("images %q, drains %d", f.images, f.drains)
	}
	if !strings.Contains(f.out.String(), "v9.9.9 is in place") {
		t.Fatalf("output:\n%s", f.out)
	}
}

// After the restart the output names the confirm window, where it is set
// and the log to watch, and what a rollback leaves in update.json.
func TestNodeUpgradePrintsTheConfirmWindowAndTheLog(t *testing.T) {
	noDrain(t)
	u, f, _ := upgradeFixture(t)
	u.ConfirmWithin = 7 * time.Minute
	if err := u.run(context.Background(), "", fakeBinary(t, "mesh v9.9.9"), false); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, f.out)
	}
	for _, want := range []string{
		"confirms the upgrade with its first heartbeat, within 7m0s (node.yaml update.confirm_within); watch ~/.mesh/node.log.",
		"cannot confirm inside the window on 3 starts, is rolled back to " + selfupdate.PrevPath(f.exe) + " and ~/.mesh/update.json says why",
	} {
		if !strings.Contains(f.out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, f.out)
		}
	}
}

// A file that does not answer `version` as mesh is refused: nothing on
// disk changes and nothing restarts.
func TestNodeUpgradeRefusesABinaryThatIsNotMesh(t *testing.T) {
	noDrain(t)
	for _, say := range []string{"hello", "mesh"} {
		u, f, svc := upgradeFixture(t)
		err := u.run(context.Background(), "", fakeBinary(t, say), false)
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("%q: err = %v", say, err)
		}
		f.unchanged(t)
		if len(svc.calls) != 0 {
			t.Fatalf("restarted after a refusal: %q", svc.calls)
		}
	}
	u, f, svc := upgradeFixture(t)
	if err := u.run(context.Background(), "", filepath.Join(t.TempDir(), "missing"), false); err == nil {
		t.Fatal("a missing binary was installed")
	}
	f.unchanged(t)
	if len(svc.calls) != 0 {
		t.Fatalf("restarted: %q", svc.calls)
	}
}

// `--version` fetches and verifies the release, ensures its image, swaps
// and restarts — no drain.
func TestNodeUpgradeReleaseSwapsAndRestartsWithoutDraining(t *testing.T) {
	noDrain(t)
	u, f, svc := upgradeFixture(t)
	if err := u.run(context.Background(), updateTo, "", false); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, f.out)
	}
	if got, _ := os.ReadFile(f.exe); !bytes.Equal(got, f.bin) {
		t.Fatalf("executable = %q", got)
	}
	if got, _ := os.ReadFile(selfupdate.PrevPath(f.exe)); string(got) != "mesh v1.1.0" {
		t.Fatalf(".prev = %q", got)
	}
	if len(svc.calls) != 1 || svc.calls[0] != "restart with mesh "+updateTo {
		t.Fatalf("restarts %q", svc.calls)
	}
	if f.drains != 0 || len(f.images) != 1 || f.images[0] != updateTo+" after 0 drains" {
		t.Fatalf("drains %d, images %q", f.drains, f.images)
	}
}

// A sandboxed node's sessions would end with it: refused unless --force.
func TestNodeUpgradeRefusesASandboxedNode(t *testing.T) {
	noDrain(t)
	u, f, svc := upgradeFixture(t)
	u.Sandboxed = func() bool { return true }
	bin := fakeBinary(t, "mesh v9.9.9")
	if err := u.run(context.Background(), "", bin, false); err == nil || !strings.Contains(err.Error(), "mesh node update") {
		t.Fatalf("err = %v", err)
	}
	f.unchanged(t)
	if len(svc.calls) != 0 {
		t.Fatalf("restarted: %q", svc.calls)
	}
	if err := u.run(context.Background(), "", bin, true); err != nil || len(svc.calls) != 1 {
		t.Fatalf("--force: %v, restarts %q", err, svc.calls)
	}
}

// Under systemd --user the node's unit is restarted, by its name.
func TestHostServiceRestartsTheSystemdUnit(t *testing.T) {
	noDrain(t)
	var ran []string
	s := hostService{GOOS: "linux", Command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	var out bytes.Buffer
	if err := s.Restart(context.Background(), node.RunState{Dir: t.TempDir()}, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl --user is-active --quiet io.projectmesh.mesh-node.service",
		"systemctl --user restart io.projectmesh.mesh-node.service",
	}
	if strings.Join(ran, "|") != strings.Join(want, "|") {
		t.Fatalf("ran %q, want %q", ran, want)
	}
}

// Under launchd the agent is kickstarted with -k.
func TestHostServiceKickstartsTheLaunchAgent(t *testing.T) {
	var ran []string
	s := hostService{GOOS: "darwin", Command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	if err := s.Restart(context.Background(), node.RunState{Dir: t.TempDir()}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 2 || !strings.HasPrefix(ran[1], "launchctl kickstart -k gui/") || !strings.HasSuffix(ran[1], "/"+node.ServiceLabel) {
		t.Fatalf("ran %q", ran)
	}
}

// With no service, the pidfile's process is sent SIGHUP: here the test's
// own, whose restart handler — the one `node start` installs — is called.
// No drain is signalled.
func TestHostServiceSignalsANodeNoServiceRuns(t *testing.T) {
	noDrain(t)
	state := node.RunState{Dir: t.TempDir()}
	if err := state.Started(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	restarted := make(chan struct{}, 1)
	stop := onRestartSignal(func() { restarted <- struct{}{} })
	defer stop()
	s := hostService{GOOS: "linux", Command: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("inactive")
	}}
	var out bytes.Buffer
	if err := s.Restart(context.Background(), state, &out); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP never reached the restart handler")
	}
	if !strings.Contains(out.String(), "restarting") {
		t.Fatalf("said %q", out.String())
	}
	state.Stopped(os.Getpid())
	out.Reset()
	if err := s.Restart(context.Background(), state, &out); err != nil || !strings.Contains(out.String(), "No node is running") {
		t.Fatalf("no node: %v, %q", err, out.String())
	}
}

func TestParseVersionLine(t *testing.T) {
	for in, want := range map[string]string{"mesh v1.2.3\n": "v1.2.3", "mesh dev": "dev"} {
		if got, err := parseVersionLine(in); err != nil || got != want {
			t.Fatalf("%q: %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "mesh", "git version 2", "mesh v1 extra"} {
		if _, err := parseVersionLine(in); err == nil {
			t.Fatalf("%q parsed", in)
		}
	}
}
