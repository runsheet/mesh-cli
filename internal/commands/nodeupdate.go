package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/selfupdate"
	"projectmesh.io/mesh-cli/internal/version"
)

var (
	nodeUpdateVerifyOnly bool
	nodeUpdateFromDir    string
)

var nodeUpdateCmd = &cobra.Command{
	Use:   "update [version]",
	Short: "Update this machine's mesh to a signed release by hand: verify, pull its node image, drain the node, swap the binary",
	Long: `Does by hand what a node does when the platform asks it for a version
(adr-002). With no version it takes the latest release that is not a
prerelease.

The release's binary for this OS/arch is downloaded from the public mirror
runsheet/mesh; the bundle must be signed by release-tag.yml on
refs/tags/<version> of runsheet/mesh-cli, issued by
https://token.actions.githubusercontent.com, and the binary's sha256 must
be its line in the sums. Anything else is refused with the reason and a
non-zero exit, and nothing on disk changes.

Once verified, the release's node image (mesh-specs ADR 007),
ghcr.io/runsheet/mesh-node:<version>, is pulled with podman before
anything is drained, and every older mesh-node tag but the running
node's is removed. node.yaml's sandbox.image chooses it: empty is that
reference, any other value is used as given, and ` + "`build`" + ` builds that tag
here from the Containerfile the binary embeds instead of pulling. A pull
or build that fails is the error: nothing is drained or swapped and the
node stays on its binary. With ` + "`sandbox: off`" + `, or no podman and no mode
set, no image is pulled. Asked for the version already running, the
command ensures its image and nothing else.

Then the node running on this machine (its pid is in
~/.mesh/node.pid) is drained as by ` + "`mesh node drain`" + `, and the executable
is swapped atomically, the previous one kept as <path>.prev. The node
exits when its sessions end and its service (systemd --user, launchd)
starts the new binary; one that fails to start twice is rolled back to
.prev.

To upgrade without waiting for the sessions — the node restarts at once
and the new process adopts them — use ` + "`mesh node upgrade`" + `.

--verify-only downloads and verifies into a temporary directory and swaps
nothing.

--from-dir reads the release's files from a directory instead of GitHub
(the binary for this OS/arch, SHA256SUMS, SHA256SUMS.sigstore.json) and
verifies them the same way, against the version's release identity: for
checking a downloaded release, or showing that an edited one is refused.`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		u, err := defaultNodeUpdate(cmd.OutOrStdout())
		if err != nil {
			return err
		}
		if nodeUpdateFromDir != "" {
			u.Updater.Source = selfupdate.Dir(nodeUpdateFromDir)
		}
		want := ""
		if len(args) == 1 {
			want = args[0]
		}
		return u.run(cmd.Context(), want, nodeUpdateVerifyOnly)
	},
}

// nodeUpdate is `mesh node update`, with what it touches injectable for
// tests: the release source and policy on Updater, the executable, the
// run state and the drain.
type nodeUpdate struct {
	// Updater fetches, verifies, records and swaps: the heartbeat path's own.
	Updater *node.ReleaseUpdater
	// Latest is the latest non-prerelease release's tag.
	Latest func(context.Context) (string, error)
	// Running is this binary's version.
	Running string
	// Run is where the running node's pidfile is.
	Run node.RunState
	// Drain drains the node Run names, reporting whether one was running;
	// nil is drainIfRunning.
	Drain func(node.RunState, io.Writer) (bool, error)
	// Image makes the node image of a version present on this machine
	// (SandboxConfig.EnsureImage); nil ensures none.
	Image func(context.Context, string) (sandbox.Image, error)
	// TempDir is where --verify-only writes the binary; empty is os.TempDir.
	TempDir string
	Out     io.Writer
}

func defaultNodeUpdate(out io.Writer) (*nodeUpdate, error) {
	exe, err := node.Executable()
	if err != nil {
		return nil, err
	}
	state, err := selfupdate.DefaultStateFile()
	if err != nil {
		return nil, err
	}
	run, err := node.DefaultRunState()
	if err != nil {
		return nil, err
	}
	gh := selfupdate.GitHub{}
	return &nodeUpdate{
		Updater: &node.ReleaseUpdater{Source: gh, State: state, Exe: exe},
		Latest:  gh.Latest,
		Running: version.String(),
		Run:     run,
		Image:   ensureNodeImage,
		Out:     out,
	}, nil
}

// run updates to want (the latest release when empty). The release is
// verified and its node image ensured before anything else happens, so a
// refusal or a failed pull changes nothing; then the node is drained and
// the binary swapped. The swap follows the
// drain signal at once: a node with no sessions exits on it, and its
// service waits 10 seconds (RestartSec, ThrottleInterval) before starting
// what is then the new binary.
func (u *nodeUpdate) run(ctx context.Context, want string, verifyOnly bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if want == "" {
		latest, err := u.Latest(ctx)
		if err != nil {
			return err
		}
		want = latest
		fmt.Fprintf(u.Out, "The latest release is %s.\n", want)
	}
	if !verifyOnly && want == u.Running {
		fmt.Fprintf(u.Out, "mesh %s is already running here; nothing to update.\n", want)
		return u.image(ctx, want)
	}
	asset := selfupdate.AssetName(u.goos(), u.goarch())
	fmt.Fprintf(u.Out, "Fetching %s of %s and verifying it…\n", asset, want)
	bin, err := u.Updater.Fetch(ctx, want)
	if err != nil {
		return fmt.Errorf("refused %s: %s", want, selfupdate.Reason(err))
	}
	sum := sha256.Sum256(bin)
	fmt.Fprintf(u.Out, "Verified: %s sha256 %s is in SHA256SUMS, signed by release-tag.yml on refs/tags/%s.\n",
		asset, hex.EncodeToString(sum[:]), want)
	if verifyOnly {
		return u.keep(asset, bin)
	}
	if err := u.image(ctx, want); err != nil {
		if rerr := u.Updater.Refuse(u.Running, want, err.Error()); rerr != nil {
			fmt.Fprintf(u.Out, "record the failed update: %v\n", rerr)
		}
		return fmt.Errorf("not updated: %w; nothing drained, %s stays in place", err, u.Running)
	}

	drain := u.Drain
	if drain == nil {
		drain = drainIfRunning
	}
	running, err := drain(u.Run, u.Out)
	if err != nil {
		return fmt.Errorf("not updated: %w", err)
	}
	err = u.Updater.Begin(u.Running, want)
	if err == nil {
		err = u.Updater.Swap(bin)
	}
	if err != nil {
		if rerr := u.Updater.Refuse(u.Running, want, selfupdate.Reason(err)); rerr != nil {
			fmt.Fprintf(u.Out, "record the failed update: %v\n", rerr)
		}
		if running {
			return fmt.Errorf("not updated: %w; the node is draining and its service will start %s again", err, u.Running)
		}
		return fmt.Errorf("not updated: %w", err)
	}
	exe := u.Updater.Exe
	fmt.Fprintf(u.Out, "%s is in place at %s; the previous binary is kept at %s.\n", want, exe, selfupdate.PrevPath(exe))
	if running {
		fmt.Fprintf(u.Out, "Once the node's sessions end it exits, and its service (systemd --user or launchd) starts %s.\n", want)
	} else {
		fmt.Fprintf(u.Out, "No node is running on this machine: the next `mesh node start`, or its service, runs %s.\n", want)
	}
	fmt.Fprintf(u.Out, "A %s that fails to start twice is rolled back to %s.\n", want, selfupdate.PrevPath(exe))
	return nil
}

// image ensures the node image of version and says which it is.
func (u *nodeUpdate) image(ctx context.Context, version string) error {
	if u.Image == nil {
		return nil
	}
	fmt.Fprintf(u.Out, "Ensuring the node image of %s…\n", version)
	img, err := u.Image(ctx, version)
	if err != nil {
		return err
	}
	fmt.Fprintf(u.Out, "Node image: %s\n", img.Summary())
	return nil
}

// ensureNodeImage is SandboxConfig.EnsureImage on this machine: node.yaml
// as it stands now, podman run as a command, the running version's image
// kept.
func ensureNodeImage(ctx context.Context, want string) (sandbox.Image, error) {
	cfg, err := node.LoadSettings()
	if err != nil {
		return sandbox.Image{}, err
	}
	return cfg.Sandbox.EnsureImage(ctx, sandbox.ExecRunner, want, version.String())
}

// keep writes a verified binary into a new temporary directory for
// --verify-only, and says where.
func (u *nodeUpdate) keep(asset string, bin []byte) error {
	dir, err := os.MkdirTemp(u.TempDir, "mesh-update-")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, asset)
	if err := os.WriteFile(path, bin, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(u.Out, "Verify only: written to %s; nothing swapped.\n", path)
	return nil
}

func (u *nodeUpdate) goos() string {
	if u.Updater.GOOS != "" {
		return u.Updater.GOOS
	}
	return runtime.GOOS
}

func (u *nodeUpdate) goarch() string {
	if u.Updater.GOARCH != "" {
		return u.Updater.GOARCH
	}
	return runtime.GOARCH
}

// drainIfRunning drains the node running on this machine, as `mesh node
// drain` does; with none running there is nothing to drain.
func drainIfRunning(state node.RunState, out io.Writer) (bool, error) {
	pid, err := state.PID()
	if errors.Is(err, node.ErrNoNode) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !processAlive(pid) {
		return false, nil
	}
	return true, drainNode(state, out)
}

func init() {
	nodeUpdateCmd.Flags().BoolVar(&nodeUpdateVerifyOnly, "verify-only", false, "download and verify into a temporary directory; swap nothing")
	nodeUpdateCmd.Flags().StringVar(&nodeUpdateFromDir, "from-dir", "", "read the release's files from this directory instead of GitHub; verified the same way")
	nodeCmd.AddCommand(nodeUpdateCmd)
}
