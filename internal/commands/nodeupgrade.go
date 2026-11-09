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
)

var nodeUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Install a new mesh and restart the node at once, its leases left running for the new process to adopt — no drain",
	Long: `Installs a new binary in place of this one and restarts the Mesh v1 node
running on this machine (` + "`mesh node run`" + `) at once (adr-002, adr-009,
adr-044). Nothing is drained: every lease it holds is left running
untouched, its brief, sidecar, push heads and turn ledger written to
leases/<id>/handoff.json for the new process to adopt. A lease the
hand-off cannot adopt — one with no relay sidecar, or one whose tunnel
intercepts with no egress process of its own still alive — refuses the
upgrade, naming it, unless --force, ending it.

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

The restart is SIGHUP to the node's own pidfile (~/.mesh/node-v1/node.pid):
every lease it holds is left running, its hand-off written for the new
process to adopt; once restarted, the command reports how many of the
leases left running the new process's own status says it adopted, and
exits non-zero if fewer than that arrived. Drain (` + "`mesh node drain`" + `) stays
for taking a machine out of service.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		v1, err := v1RunState()
		if err != nil {
			return err
		}
		u, err := defaultNodeUpgrade(cmd.OutOrStdout(), v1)
		if err != nil {
			return err
		}
		return u.run(cmd.Context(), nodeUpgradeVersion, nodeUpgradeBinary, nodeUpgradeForce)
	},
}

// v1Service restarts a Mesh v1 node (`mesh node run`) by its pidfile
// alone: it tries no systemd or launchd unit — drainV1Node
// (nodev1state.go) is the drain's own precedent for the same
// directness. Its restart leaves every lease it holds running, their
// hand-off written for the new process to adopt (adr-044). It signals
// SIGHUP — the same signal `mesh node set` sends the running node for
// its own, lease-untouched live reload (adr-044's amendment), so SIGHUP
// alone no longer says which is meant: Restart writes node.upgrade-ask
// (writeV1UpgradeAsk, nodev1state.go) right before it signals, and the
// node's restart handler reads and clears that marker (takeV1UpgradeAsk)
// to tell its own SIGHUP apart from `mesh node set`'s.
type v1Service struct{}

// Restart implements nodeRestarter. The binary is already swapped by
// the time Restart runs, so a legacy node (`mesh node start`) left
// running is told apart from no node at all — "the next `mesh node
// run` runs the new binary" would otherwise read as nothing is running
// here, when in fact a legacy node, untouched by this restart, is
// (adr-044's amendment, the --api v0 path is gone); either way the
// swap already succeeded and nothing more is done here.
func (v1Service) Restart(ctx context.Context, state node.RunState, out io.Writer) error {
	pid, err := state.PID()
	if errors.Is(err, node.ErrNoNode) {
		if lpid, lok := legacyNode(); lok {
			fmt.Fprintf(out, "No Mesh v1 node is running on this machine; a legacy node (mesh node start, pid %d) is, untouched by this restart — it already runs the new binary and picks it up at its own next restart or drain.\n", lpid)
			return nil
		}
		fmt.Fprintf(out, "No Mesh v1 node is running on this machine: the next `mesh node run` runs the new binary.\n")
		return nil
	}
	if err != nil {
		return err
	}
	if !processAlive(pid) {
		if lpid, lok := legacyNode(); lok {
			fmt.Fprintf(out, "No Mesh v1 node is running on this machine (pid %d is gone); a legacy node (mesh node start, pid %d) is, untouched by this restart — it already runs the new binary and picks it up at its own next restart or drain.\n", pid, lpid)
			return nil
		}
		fmt.Fprintf(out, "No Mesh v1 node is running on this machine (pid %d is gone): the next `mesh node run` runs the new binary.\n", pid)
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
	// Service restarts the node.
	Service nodeRestarter
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

// defaultNodeUpgrade builds a nodeUpgrade for the Mesh v1 node running
// on this machine: run is where its pidfile and status are
// (v1RunState, nodev1state.go), restarted by v1Service against it.
func defaultNodeUpgrade(out io.Writer, run node.RunState) (*nodeUpgrade, error) {
	exe, err := node.Executable()
	if err != nil {
		return nil, err
	}
	state, err := selfupdate.DefaultStateFile()
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
		Service:       v1Service{},
		V1Status:      func() (nodev1.Status, bool) { return readV1Status(run) },
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
	nodeUpgradeCmd.Flags().BoolVar(&nodeUpgradeForce, "force", false, "restart the node even if its hand-off cannot adopt every lease")
	rootCmd.AddCommand(versionCmd)
}
