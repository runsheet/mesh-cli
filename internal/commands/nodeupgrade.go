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
	"projectmesh.io/mesh-cli/internal/nodev1"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/selfupdate"
	"projectmesh.io/mesh-cli/internal/version"
)

var (
	nodeUpgradeVersion string
	nodeUpgradeBinary  string
	nodeUpgradeForce   bool
	nodeUpgradeAPI     string
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

A sandboxed node is no different, as its self-update is not: each
session's agent runs detached in its pod. The pods' conmon processes run
in the service's cgroup, so the unit must say KillMode=process (` + "`mesh node install`" + `
writes it); upgrade refuses a sandboxed node whose unit says otherwise,
unless --force, and names the fix. Drain (` + "`mesh node drain`" + `) stays for
taking a machine out of service.

A Mesh v1 node (` + "`mesh node run --api v1`" + `) is restarted the same way, by
SIGHUP to its own pidfile: every lease it holds is left running untouched,
its brief, sidecar, push heads and turn ledger written to
leases/<id>/handoff.json for the new process to adopt (adr-044); nothing
is drained. A lease the hand-off cannot adopt — one with no relay
sidecar, or one whose tunnel intercepts with no egress process of its
own still alive — refuses the upgrade, naming it, unless --force; once
restarted, the command reports how many of
the leases left running the new process's own status says it adopted,
and exits non-zero if fewer than that arrived. With both a legacy and
a v1 node running as this user, --api says which.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := node.DefaultRunState()
		if err != nil {
			return err
		}
		v1, err := v1RunState()
		if err != nil {
			return err
		}
		return upgradeEither(cmd.Context(), cmd.OutOrStdout(), state, v1, nodeUpgradeAPI, nodeUpgradeVersion, nodeUpgradeBinary, nodeUpgradeForce)
	},
}

// upgradeEither upgrades the legacy node (state) or the Mesh v1 node
// (v1): the one api names, else the one running, else the legacy node
// as before (drainEither's own rule, nodecmd.go).
func upgradeEither(ctx context.Context, out io.Writer, state, v1 node.RunState, api, want, binary string, force bool) error {
	switch api {
	case "v1":
		return upgradeV1(ctx, out, v1, want, binary, force)
	case "v0", "legacy":
	case "":
		_, legacy := liveNode(state)
		_, isV1 := liveNode(v1)
		switch {
		case legacy && isV1:
			return errors.New("a legacy node and a Mesh v1 node both run as this user: say which with --api v0 or --api v1")
		case isV1:
			return upgradeV1(ctx, out, v1, want, binary, force)
		}
	default:
		return fmt.Errorf("--api is v0 or v1, not %q", api)
	}
	u, err := defaultNodeUpgrade(out)
	if err != nil {
		return err
	}
	return u.run(ctx, want, binary, force)
}

// upgradeV1 upgrades the Mesh v1 node: the same binary-swap path as the
// legacy node's (nodeUpgrade.run, unchanged), restarted by v1Service
// instead of hostService -- every lease it holds is left running, its
// hand-off written for the new process to adopt (adr-044; adr-009's
// restart, carried to a v1 node with no drain).
func upgradeV1(ctx context.Context, out io.Writer, run node.RunState, want, binary string, force bool) error {
	u, err := defaultNodeUpgradeV1(out, run)
	if err != nil {
		return err
	}
	return u.run(ctx, want, binary, force)
}

// defaultNodeUpgradeV1 builds a nodeUpgrade for the Mesh v1 node: the
// same binary-swap mechanics as defaultNodeUpgrade (shared, since they
// are API-independent), restarted by v1Service against run's pidfile
// instead of hostService; v0's sandboxed-KillMode safety check does
// not apply, since a v1 node's restart never calls `systemctl restart`
// or `launchctl kickstart` -- only SIGHUP -- so the cgroup-wide-kill
// concern that check guards against is moot.
func defaultNodeUpgradeV1(out io.Writer, run node.RunState) (*nodeUpgrade, error) {
	u, err := defaultNodeUpgrade(out)
	if err != nil {
		return nil, err
	}
	u.Run = run
	u.Service = v1Service{}
	u.Sandboxed = nil
	u.KillMode = nil
	u.V1Status = func() (nodev1.Status, bool) { return readV1Status(run) }
	return u, nil
}

// v1Service restarts a Mesh v1 node (`mesh node run --api v1`) by its
// pidfile alone: unlike the legacy node's hostService, it tries no
// systemd or launchd unit -- drainV1Node (nodev1state.go) is the
// drain's own precedent for the same directness, and a v1 node has no
// unit of its own yet. Its restart leaves every lease it holds running,
// their hand-off written for the new process to adopt (adr-044). It
// signals SIGHUP -- the same signal `mesh node set` sends the running
// node for its own, lease-untouched live reload (adr-044's amendment),
// so SIGHUP alone no longer says which is meant: Restart writes
// node.upgrade-ask (writeV1UpgradeAsk, nodev1state.go) right before it
// signals, and the node's restart handler reads and clears that marker
// (takeV1UpgradeAsk) to tell its own SIGHUP apart from `mesh node
// set`'s.
type v1Service struct{}

// Restart implements nodeRestarter.
func (v1Service) Restart(ctx context.Context, state node.RunState, out io.Writer) error {
	pid, err := state.PID()
	if errors.Is(err, node.ErrNoNode) {
		fmt.Fprintf(out, "No Mesh v1 node is running on this machine: the next `mesh node run --api v1` runs the new binary.\n")
		return nil
	}
	if err != nil {
		return err
	}
	if !processAlive(pid) {
		fmt.Fprintf(out, "No Mesh v1 node is running on this machine (pid %d is gone): the next `mesh node run --api v1` runs the new binary.\n", pid)
		return nil
	}
	if err := writeV1UpgradeAsk(state); err != nil {
		return fmt.Errorf("mark the upgrade for node (pid %d): %w", pid, err)
	}
	if err := signalRestart(pid); err != nil {
		return fmt.Errorf("signal node (pid %d): %w", pid, err)
	}
	fmt.Fprintf(out, "Mesh v1 node (pid %d) is upgrading: every lease it holds is left running, its hand-off written for the new process to adopt (adr-044); nothing is drained.\n", pid)
	return nil
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
	// KillMode is the node's systemd unit's KillMode and whether such a
	// unit is loaded; nil is no unit (launchd, or a node no service runs).
	KillMode func(context.Context) (string, bool)
	// ConfirmWithin is node.yaml's update.confirm_within, as the output
	// names it; zero is selfupdate.DefaultConfirmWithin.
	ConfirmWithin time.Duration
	// V1Status is the Mesh v1 node's own status (nodev1state.go's
	// readV1Status); nil for a legacy-node upgrade, which has no leases
	// to ask about. Read once before the swap to refuse an unadoptable
	// lease (adr-044's hand-off never picks one up) and polled after the
	// restart to report how many of the adoptable leases the new
	// process actually adopted.
	V1Status func() (nodev1.Status, bool)
	// V1AdoptPoll is how often V1Status is polled after the restart;
	// zero is defaultV1AdoptPoll.
	V1AdoptPoll time.Duration
	// V1AdoptTimeout bounds that poll; zero is defaultV1AdoptTimeout.
	V1AdoptTimeout time.Duration
	Out            io.Writer
}

// defaultV1AdoptPoll and defaultV1AdoptTimeout are run's defaults for
// polling the new process's status after a v1 node's restart.
const (
	defaultV1AdoptPoll    = 200 * time.Millisecond
	defaultV1AdoptTimeout = 30 * time.Second
)

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
		Running:       version.Resolved(),
		Run:           run,
		Version:       binaryVersion,
		Image:         ensureNodeImage,
		Sandboxed:     nodeSandboxed,
		Service:       hostService{},
		KillMode:      hostKillMode,
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
	var beforePID int
	var adoptableBefore int
	if u.V1Status != nil {
		if before, ok := u.V1Status(); ok {
			beforePID = before.PID
			var unadoptable []string
			for _, l := range before.Leases {
				if l.Adoptable {
					adoptableBefore++
				} else if l.AdoptableReason != "" {
					unadoptable = append(unadoptable, fmt.Sprintf("%s (job %s): %s", l.LeaseID, l.JobID, l.AdoptableReason))
				} else {
					unadoptable = append(unadoptable, fmt.Sprintf("%s (job %s)", l.LeaseID, l.JobID))
				}
			}
			if len(unadoptable) > 0 && !force {
				return fmt.Errorf("the restart's hand-off cannot adopt %d lease(s) with no relay sidecar, whose tunnel intercepts with no egress process of its own, or whose egress process has died: %s; --force restarts anyway, ending them",
					len(unadoptable), strings.Join(unadoptable, ", "))
			}
		}
	}
	if u.Sandboxed != nil && u.Sandboxed() && !force {
		// a sandboxed session's pod outlives the node when its unit signals
		// the node alone (KillMode=process); under systemd's default every
		// pod's conmon is in the service's cgroup and dies with it
		if mode, managed := u.killMode(ctx); managed && mode != node.SafeKillMode {
			return fmt.Errorf("this node's unit has KillMode=%s, which ends its sandboxed sessions with it: add KillMode=%s under [Service] in %s and run `systemctl --user daemon-reload` (`mesh node install` writes it), or `mesh node update` drains it first, or --force restarts it now and ends them",
				mode, node.SafeKillMode, node.SystemdUnitPath("~"))
		}
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
	err := u.Updater.Begin(u.Running, want, nil)
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
	if u.V1Status != nil && adoptableBefore > 0 {
		if err := u.reportAdoption(ctx, beforePID, adoptableBefore); err != nil {
			return err
		}
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

// reportAdoption polls V1Status until the restarted process has
// published a status of its own (a different pid than beforePID) whose
// own start-up hand-off adoption has finished deciding every lease it
// found (!st.Adopting) or V1AdoptTimeout elapses, then prints how many
// of the adoptableBefore leases the new process's hand-off adoption
// actually picked up (adopt.go's adoptOne); fewer than expected, or a
// timeout with no new status ever seen complete, is reported as a
// failure, not silently. A changed pid alone is not enough: adoptOne
// runs its leases' adoption on goroutines that can still be deciding
// well after the new process's first status is published.
func (u *nodeUpgrade) reportAdoption(ctx context.Context, beforePID, adoptableBefore int) error {
	poll := u.V1AdoptPoll
	if poll <= 0 {
		poll = defaultV1AdoptPoll
	}
	timeout := u.V1AdoptTimeout
	if timeout <= 0 {
		timeout = defaultV1AdoptTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		if st, ok := u.V1Status(); ok && st.PID != beforePID && !st.Adopting {
			adopted := len(st.Leases)
			fmt.Fprintf(u.Out, "The new process (pid %d) adopted %d of %d lease(s) left running.\n", st.PID, adopted, adoptableBefore)
			if adopted < adoptableBefore {
				return fmt.Errorf("the new process adopted only %d of %d lease(s) left running", adopted, adoptableBefore)
			}
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the new process did not report adopting the %d lease(s) left running within %s", adoptableBefore, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// killMode is the node's unit's KillMode, when a systemd unit runs it.
func (u *nodeUpgrade) killMode(ctx context.Context) (string, bool) {
	if u.KillMode == nil {
		return "", false
	}
	return u.KillMode(ctx)
}

// hostKillMode is the KillMode of this machine's node unit under systemd
// --user; elsewhere there is none.
func hostKillMode(ctx context.Context) (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	return node.SystemdKillMode(ctx, nil)
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
// (`mesh <version>`, as version.Long says it): a file that does not run
// as mesh is refused.
func binaryVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("`%s version`: %w", path, err)
	}
	return parseVersionLine(string(out))
}

// parseVersionLine reads version.Long's line — `mesh <version>`, `mesh
// <tag> (commit <sha>)` or `mesh <sha> (dev build)` — and returns the
// version alone, as the new binary will report itself: the tag, else the
// sha. An older binary says the bare `mesh <version>`.
func parseVersionLine(out string) (string, error) {
	line := strings.TrimSpace(out)
	bad := fmt.Errorf("`version` said %q, not `mesh <version>`", line)
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "mesh" || f[1] == "" || strings.HasPrefix(f[1], "(") {
		return "", bad
	}
	switch rest := strings.Join(f[2:], " "); {
	case rest == "", rest == "(dev build)":
	case strings.HasPrefix(rest, "(commit ") && strings.HasSuffix(rest, ")") && len(f) == 4:
	default:
		return "", bad
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
	Short: "Print this binary's version and commit: mesh <tag> (commit <sha>), or mesh <sha> (dev build)",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), version.Long())
	},
}

func init() {
	nodeUpgradeCmd.Flags().StringVar(&nodeUpgradeVersion, "version", "", "the release to install (vX.Y.Z); the latest when neither this nor --binary is given")
	nodeUpgradeCmd.Flags().StringVar(&nodeUpgradeBinary, "binary", "", "install this local build instead of a release, once `<path> version` answers")
	nodeUpgradeCmd.Flags().BoolVar(&nodeUpgradeForce, "force", false, "restart a sandboxed node too, ending its sessions")
	nodeUpgradeCmd.Flags().StringVar(&nodeUpgradeAPI, "api", "", "the node to upgrade when a legacy and a v1 node both run as this user: v0 or v1 (default the one running)")
	rootCmd.AddCommand(versionCmd)
}
