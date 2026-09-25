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
	nodeUpdateImageOnly  bool
)

var nodeUpdateCmd = &cobra.Command{
	Use:   "update [version]",
	Short: "Update this machine's mesh to a signed release by hand: verify, drain the node, swap the binary, pull its node image",
	Long: `Does by hand what a node does when the platform asks it for a version
(adr-002). With no version it takes the latest release that is not a
prerelease.

The release's binary for this OS/arch is downloaded with SHA256SUMS and its
Sigstore bundle, and verified: the bundle must be signed by
release-tag.yml on refs/tags/<version> of projectmesh-io/mesh-cli, issued
by https://token.actions.githubusercontent.com, and the binary's sha256 must
be its line in the sums. Anything else is refused with the reason and a
non-zero exit, and nothing on disk changes.

Once verified, the node running on this machine (its pid is in
~/.mesh/node.pid) is drained as by ` + "`mesh node drain`" + `, and the executable
is swapped atomically, the previous one kept as <path>.prev. The node
exits when its sessions end and its service (systemd --user, launchd)
starts the new binary; one that fails to start twice is rolled back to
.prev.

The node image follows the binary (mesh-specs ADR 007): once the new
binary is in place, and when the version asked for is already running,
ghcr.io/projectmesh-io/mesh-node:<version> is pulled with podman — built
from the Containerfile this binary embeds when the pull fails — every
older mesh-node tag is removed, and the image in use is printed. Without
podman there is no image to keep (a node with sandbox: off).

--image does only that, for the running version: no release is fetched.

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
		if nodeUpdateImageOnly {
			return u.image(cmd.Context(), u.Running)
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
	// Image makes the node image of a version the one on this machine
	// (sandbox.EnsureImage); nil keeps none.
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
		Running: version.Version,
		Run:     run,
		Image:   ensureNodeImage,
		Out:     out,
	}, nil
}

// run updates to want (the latest release when empty). The release is
// verified before anything else happens, so a refusal changes nothing;
// then the node is drained and the binary swapped. The swap follows the
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
	return u.image(ctx, want)
}

// image makes the node image of version the one on this machine and says
// which it is. No container runtime is no image, not an error.
func (u *nodeUpdate) image(ctx context.Context, version string) error {
	if u.Image == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ref, err := sandbox.Ref(version)
	if err != nil {
		return err
	}
	fmt.Fprintf(u.Out, "Pulling the node image %s…\n", ref)
	img, err := u.Image(ctx, version)
	if errors.Is(err, sandbox.ErrNoRuntime) {
		fmt.Fprintf(u.Out, "No node image: %v (a node here runs with sandbox: off).\n", err)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(u.Out, "Node image in use: %s\n", img.Summary())
	return nil
}

// ensureNodeImage is sandbox.EnsureImage on podman from PATH.
func ensureNodeImage(ctx context.Context, version string) (sandbox.Image, error) {
	rt, err := sandbox.DefaultImageRuntime()
	if err != nil {
		return sandbox.Image{}, err
	}
	return sandbox.EnsureImage(ctx, rt, version)
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
	nodeUpdateCmd.Flags().BoolVar(&nodeUpdateImageOnly, "image", false, "only pull (or build) the node image of the running version and prune older ones; fetch no release")
	nodeUpdateCmd.Flags().StringVar(&nodeUpdateFromDir, "from-dir", "", "read the release's files from this directory instead of GitHub; verified the same way")
	nodeCmd.AddCommand(nodeUpdateCmd)
}
