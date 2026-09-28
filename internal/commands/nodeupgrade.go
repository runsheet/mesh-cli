package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/selfupdate"
	"projectmesh.io/mesh-cli/internal/version"
)

var (
	nodeUpgradeVersion string
	nodeUpgradeBinary  string
	nodeUpgradeForce   bool
)

var nodeUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Install a new mesh and restart the node at once, its sessions left running for the new process to adopt — no drain",
	Long: `Installs a new binary in place of this one and restarts the node running
on this machine at once (adr-002, adr-009). Nothing is drained: every
session runs in its own unit (a transient systemd user scope, else a
detached process group), so the node's exit does not end it, and the new
process adopts each one from its state file and output — it heartbeats
the item at once, follows the session from its saved offset and finishes
it as usual.

With --version vX.Y.Z, or the latest release that is not a prerelease
when neither flag is given, the release's binary for this OS/arch is
downloaded from the public mirror runsheet/mesh; the bundle must be signed
by release-tag.yml on refs/tags/<version> of runsheet/mesh-cli, verified
as ` + "`mesh node update`" + ` does, and its node image ensured. With --binary
<path> a local build is installed instead, once ` + "`<path> version`" + ` answers.
Either way the executable is swapped atomically, the previous one kept as
<path>.prev. The new process confirms the upgrade with its first
heartbeat, retried for node.yaml's update.confirm_within (5m by default)
while the platform is slow; one that cannot run, or cannot confirm inside
the window on three starts, is rolled back to it, and ~/.mesh/update.json
says why. Watch ~/.mesh/node.log.

The restart: under systemd --user, ` + "`systemctl --user restart " + node.ServiceLabel + "`" + `
(the unit's KillMode=process signals the node alone); under launchd,
` + "`launchctl kickstart -k`" + `; a node no service runs is sent SIGHUP (its pid is
in ~/.mesh/node.pid) and execs the new binary itself.

A sandboxed node's sessions end with it (their pod is not yet their
unit): upgrade refuses one unless --force; ` + "`mesh node update`" + ` drains it
first. Drain (` + "`mesh node drain`" + `) stays for taking a machine out of service.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		u, err := defaultNodeUpgrade(cmd.OutOrStdout())
		if err != nil {
			return err
		}
		return u.run(cmd.Context(), nodeUpgradeVersion, nodeUpgradeBinary, nodeUpgradeForce)
	},
}

// nodeRestarter restarts the node running on this machine; a node the
// command finds none of is not an error, only said.
type nodeRestarter interface {
	Restart(ctx context.Context, state node.RunState, out io.Writer) error
}

// nodeUpgrade is `mesh node upgrade`, with what it touches injectable
// for tests: the release source and policy on Updater, the executable,
// the version check of a local binary, the image and the restart.
type nodeUpgrade struct {
	// Updater fetches, verifies, records and swaps: the heartbeat path's own.
	Updater *node.ReleaseUpdater
	// Latest is the latest non-prerelease release's tag.
	Latest func(context.Context) (string, error)
	// Running is this binary's version.
	Running string
	// Run is where the running node's pidfile is.
	Run node.RunState
	// Version runs `<path> version` and returns the version it names.
	Version func(ctx context.Context, path string) (string, error)
	// Image makes the node image of a version present on this machine;
	// nil ensures none.
	Image func(context.Context, string) (sandbox.Image, error)
	// Sandboxed reports whether the node runs its sessions in pods;
	// nil is never.
	Sandboxed func() bool
	// Service restarts the node.
	Service nodeRestarter
	// ConfirmWithin is node.yaml's update.confirm_within, as the output
	// names it; zero is selfupdate.DefaultConfirmWithin.
	ConfirmWithin time.Duration
	Out           io.Writer
}

func defaultNodeUpgrade(out io.Writer) (*nodeUpgrade, error) {
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
	within := selfupdate.DefaultConfirmWithin
	if cfg, err := node.LoadSettings(); err == nil {
		within = cfg.ConfirmWithin()
	}
	return &nodeUpgrade{
		ConfirmWithin: within,
		Updater:       &node.ReleaseUpdater{Source: gh, State: state, Exe: exe},
		Latest:        gh.Latest,
		Running:       version.Version,
		Run:           run,
		Version:       binaryVersion,
		Image:         ensureNodeImage,
		Sandboxed:     nodeSandboxed,
		Service:       hostService{},
		Out:           out,
	}, nil
}

// run installs the new binary — a local build at binary, else the
// release want (the latest when empty) — and restarts the node at once.
func (u *nodeUpgrade) run(ctx context.Context, want, binary string, force bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if want != "" && binary != "" {
		return errors.New("--version and --binary are either-or")
	}
	if u.Sandboxed != nil && u.Sandboxed() && !force {
		return errors.New("this node runs its sessions in sandboxes, which end with it: `mesh node update` drains it first, or --force restarts it now and ends them")
	}
	var bin []byte
	if binary != "" {
		v, err := u.Version(ctx, binary)
		if err != nil {
			return fmt.Errorf("refused %s: %w", binary, err)
		}
		if bin, err = os.ReadFile(binary); err != nil {
			return err
		}
		want = v
		fmt.Fprintf(u.Out, "%s answers `version` with %s.\n", binary, want)
	} else {
		if want == "" {
			latest, err := u.Latest(ctx)
			if err != nil {
				return err
			}
			want = latest
			fmt.Fprintf(u.Out, "The latest release is %s.\n", want)
		}
		if want == u.Running {
			fmt.Fprintf(u.Out, "mesh %s is already running here; nothing to upgrade.\n", want)
			return nil
		}
		asset := selfupdate.AssetName(u.goos(), u.goarch())
		fmt.Fprintf(u.Out, "Fetching %s of %s and verifying it…\n", asset, want)
		var err error
		if bin, err = u.Updater.Fetch(ctx, want); err != nil {
			return fmt.Errorf("refused %s: %s", want, selfupdate.Reason(err))
		}
		sum := sha256.Sum256(bin)
		fmt.Fprintf(u.Out, "Verified: %s sha256 %s is in SHA256SUMS, signed by release-tag.yml on refs/tags/%s.\n",
			asset, hex.EncodeToString(sum[:]), want)
		if u.Image != nil {
			fmt.Fprintf(u.Out, "Ensuring the node image of %s…\n", want)
			img, err := u.Image(ctx, want)
			if err != nil {
				if rerr := u.Updater.Refuse(u.Running, want, err.Error()); rerr != nil {
					fmt.Fprintf(u.Out, "record the failed upgrade: %v\n", rerr)
				}
				return fmt.Errorf("not upgraded: %w; %s stays in place", err, u.Running)
			}
			fmt.Fprintf(u.Out, "Node image: %s\n", img.Summary())
		}
	}
	err := u.Updater.Begin(u.Running, want)
	if err == nil {
		err = u.Updater.Swap(bin)
	}
	if err != nil {
		if rerr := u.Updater.Refuse(u.Running, want, selfupdate.Reason(err)); rerr != nil {
			fmt.Fprintf(u.Out, "record the failed upgrade: %v\n", rerr)
		}
		return fmt.Errorf("not upgraded: %w", err)
	}
	exe := u.Updater.Exe
	fmt.Fprintf(u.Out, "%s is in place at %s; the previous binary is kept at %s.\n", want, exe, selfupdate.PrevPath(exe))
	if err := u.Service.Restart(ctx, u.Run, u.Out); err != nil {
		return fmt.Errorf("%s is in place, but the node was not restarted: %w", want, err)
	}
	within := u.ConfirmWithin
	if within <= 0 {
		within = selfupdate.DefaultConfirmWithin
	}
	fmt.Fprintf(u.Out, "The new process confirms the upgrade with its first heartbeat, within %s (node.yaml update.confirm_within); watch %s. "+
		"A binary that cannot run, or cannot confirm inside the window on %d starts, is rolled back to %s and ~/.mesh/update.json says why.\n",
		within, node.ServiceLog("~"), selfupdate.MaxStarts, selfupdate.PrevPath(exe))
	return nil
}

func (u *nodeUpgrade) goos() string {
	if u.Updater.GOOS != "" {
		return u.Updater.GOOS
	}
	return runtime.GOOS
}

func (u *nodeUpgrade) goarch() string {
	if u.Updater.GOARCH != "" {
		return u.Updater.GOARCH
	}
	return runtime.GOARCH
}

// binaryVersion runs `<path> version` and returns the version it prints
// (`mesh <version>`): a file that does not run as mesh is refused.
func binaryVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("`%s version`: %w", path, err)
	}
	return parseVersionLine(string(out))
}

// parseVersionLine reads `mesh <version>`.
func parseVersionLine(out string) (string, error) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 || f[0] != "mesh" || f[1] == "" {
		return "", fmt.Errorf("`version` said %q, not `mesh <version>`", strings.TrimSpace(out))
	}
	return f[1], nil
}

// nodeSandboxed is whether the node on this machine runs its sessions
// in pods, as OpenSandbox decides it: node.yaml's `sandbox`, or with
// the mode left out whether podman answers.
func nodeSandboxed() bool {
	cfg, err := node.LoadSettings()
	if err != nil {
		return false
	}
	switch strings.ToLower(cfg.Sandbox.Mode) {
	case node.SandboxOff:
		return false
	case node.SandboxOn:
		return true
	}
	return exec.Command("podman", "--version").Run() == nil
}

// hostService restarts the node the way this machine runs it: its
// systemd --user unit, its launchd agent, else the pidfile's process by
// SIGHUP. Command and GOOS are replaced in tests.
type hostService struct {
	// Command runs a command and returns its combined output; nil runs it.
	Command func(ctx context.Context, name string, args ...string) ([]byte, error)
	// GOOS is runtime.GOOS unless set.
	GOOS string
}

func (s hostService) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if s.Command != nil {
		return s.Command(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Restart implements nodeRestarter.
func (s hostService) Restart(ctx context.Context, state node.RunState, out io.Writer) error {
	goos := s.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	switch goos {
	case "linux":
		unit := node.ServiceLabel + ".service"
		if _, err := s.run(ctx, "systemctl", "--user", "is-active", "--quiet", unit); err == nil {
			if b, err := s.run(ctx, "systemctl", "--user", "restart", unit); err != nil {
				return fmt.Errorf("systemctl --user restart %s: %v: %s", unit, err, strings.TrimSpace(string(b)))
			}
			fmt.Fprintf(out, "Restarted %s: the new process adopts the sessions the old one left running (`systemctl --user list-units 'mesh-session-*'`).\n", unit)
			return nil
		}
	case "darwin":
		target := fmt.Sprintf("gui/%d/%s", os.Getuid(), node.ServiceLabel)
		if _, err := s.run(ctx, "launchctl", "print", target); err == nil {
			if b, err := s.run(ctx, "launchctl", "kickstart", "-k", target); err != nil {
				return fmt.Errorf("launchctl kickstart -k %s: %v: %s", target, err, strings.TrimSpace(string(b)))
			}
			fmt.Fprintf(out, "Restarted %s: the new process adopts the sessions the old one left running.\n", target)
			return nil
		}
	}
	pid, err := state.PID()
	if errors.Is(err, node.ErrNoNode) {
		fmt.Fprintf(out, "No node is running on this machine: the next `mesh node start` runs the new binary.\n")
		return nil
	}
	if err != nil {
		return err
	}
	if !processAlive(pid) {
		fmt.Fprintf(out, "No node is running on this machine (pid %d is gone): the next `mesh node start` runs the new binary.\n", pid)
		return nil
	}
	if err := signalRestart(pid); err != nil {
		return fmt.Errorf("signal node (pid %d): %w", pid, err)
	}
	fmt.Fprintf(out, "Node (pid %d) is restarting: it leaves its sessions running and execs the new binary, which adopts them.\n", pid)
	return nil
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print this binary's version: mesh <version>",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "mesh %s\n", version.Version)
	},
}

func init() {
	nodeUpgradeCmd.Flags().StringVar(&nodeUpgradeVersion, "version", "", "the release to install (vX.Y.Z); the latest when neither this nor --binary is given")
	nodeUpgradeCmd.Flags().StringVar(&nodeUpgradeBinary, "binary", "", "install this local build instead of a release, once `<path> version` answers")
	nodeUpgradeCmd.Flags().BoolVar(&nodeUpgradeForce, "force", false, "restart a sandboxed node too, ending its sessions")
	rootCmd.AddCommand(versionCmd)
}
