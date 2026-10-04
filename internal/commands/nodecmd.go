package commands

import (
	"github.com/google/uuid"

	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/channel"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/latchkey"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/selfupdate"
	"projectmesh.io/mesh-cli/internal/sessionunit"
	"projectmesh.io/mesh-cli/internal/version"
)

var (
	nodeOrgsFlag           []string
	nodeNameFlag           string
	nodeSlugFlag           string
	nodeCapacityFlag       int
	nodeOwnerFlag          string
	nodeEnterpriseFlag     string
	nodeIntervalFlag       time.Duration
	nodeStallFlag          time.Duration
	nodeTurnTimeoutFlag    time.Duration
	nodeSessionTimeoutFlag time.Duration
	nodeEvalBudgetFlag     float64
	nodePlanBudgetFlag     float64
	nodeResearchBudgetFlag float64
	nodeRebaseBudgetFlag   float64
	nodeMaxBudgetFlag      float64
	nodeModeFlag           string
	nodeUnattended         bool
	nodeOnceFlag           bool
	nodeModelFlag          string
	nodeModelBuildFlag     string
	nodeModelEvaluateFlag  string
	nodeModelPlanFlag      string
	nodeModelChannelFlag   string
	nodeModelResearchFlag  string
	nodeModelRebaseFlag    string
	// nodeModels is what `node start` resolved for this run: the config
	// file's models with the flags applied, read by every session.
	nodeModels node.Models
)

// modelsFor is the node's models: the config file's, each overridden by
// a flag when one was given. A kind with none runs on the CLI's own
// default. (The platform's agent definitions will win over both once
// they land — the runsheet goal `agents-on-the-platform`.)
func modelsFor(cmd *cobra.Command, cfg *node.Config) node.Models {
	m := cfg.Models
	for _, o := range []struct {
		flag string
		val  string
		set  *string
	}{
		{"model", nodeModelFlag, &m.Default},
		{"model-build", nodeModelBuildFlag, &m.Build},
		{"model-evaluate", nodeModelEvaluateFlag, &m.Evaluate},
		{"model-plan", nodeModelPlanFlag, &m.Plan},
		{"model-channel", nodeModelChannelFlag, &m.Channel},
		{"model-research", nodeModelResearchFlag, &m.Research},
		{"model-rebase", nodeModelRebaseFlag, &m.Rebase},
	} {
		if cmd.Flags().Changed(o.flag) {
			*o.set = o.val
		}
	}
	return m
}

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Run this machine as a Mesh node on the runsheet platform",
	Long: `A node runs work items of goals: it heartbeats, claims ready items for
its org and runs each in a fresh claude session, on the credential in
~/.mesh/token — the Latchkey machine credential ` + "`mesh node login`" + ` registers
for this computer, or the operator's rs_* console token.`,
}

var nodeLoginCmd = &cobra.Command{
	Use:   "login [rs_token]",
	Short: "Sign this machine in to Latchkey (the device flow) and save its machine credential to ~/.mesh/token",
	Long: `With no argument this runs Latchkey's device flow: it prints a code,
opens the browser, and waits while you sign in and approve. Latchkey
registers a machine credential for this computer with you as its
operator; the node then runs on the machine's own tokens, refreshed
before they expire, and you can revoke it in the Latchkey console
without touching your own.

The public client the flow starts from is per estate: pass --client-id
(or MESH_LATCHKEY_CLIENT_ID); a registered node remembers it in
~/.mesh/node.yaml.

With an rs_* argument it saves a runsheet console token instead — the
fallback while the estate moves to machine credentials.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return saveToken(args[0])
		}
		return deviceLogin(cmd.Context())
	},
}

func saveToken(tok string) error {
	if err := auth.SaveToken(tok); err != nil {
		return err
	}
	gw := gateway.New(gatewayURL(), strings.TrimSpace(tok))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := gw.SignIn(ctx)
	if err != nil {
		return fmt.Errorf("token saved, but the gateway refused it: %w", err)
	}
	orgs, _ := gw.MyOrgs(ctx)
	logins := make([]string, 0, len(orgs))
	for _, o := range orgs {
		logins = append(logins, o.Name())
	}
	fmt.Printf("Token saved to ~/.mesh/token. You are %s; orgs: %s\n", id, strings.Join(logins, ", "))
	fmt.Println("`mesh node login` with no argument registers a Latchkey machine credential for this computer instead: its own principal, revocable without touching your token.")
	return nil
}

var nodeRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register this machine as a node of every org you belong to (or only --org ones), converging on its slug",
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(nodeNameFlag) == "" {
			return errors.New("--name <name> is required")
		}
		// the owner and the enterprise, --flag over a prior registration's
		// (`mesh node login` or an earlier `mesh node register`), validated
		// before any network call: a bad combination refuses here, not
		// after a wasted sign-in
		owner, enterprise := strings.TrimSpace(nodeOwnerFlag), strings.TrimSpace(nodeEnterpriseFlag)
		prev, err := node.LoadSettings()
		if err != nil {
			prev = &node.Config{}
		}
		if owner == "" {
			owner = prev.Owner
		}
		if enterprise == "" {
			enterprise = prev.Enterprise
		}
		if err := (&node.Config{Owner: owner, Enterprise: enterprise}).ValidateOwner(); err != nil {
			path, _ := node.ConfigPath()
			return fmt.Errorf("%s: %w", path, err)
		}
		gw, err := platformClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		me, err := gw.SignIn(ctx)
		if err != nil {
			return err
		}
		slug := strings.TrimSpace(nodeSlugFlag)
		if slug == "" {
			slug = node.Slugify(nodeNameFlag)
		}
		if slug == "" {
			return errors.New("--slug (or a --name that makes one) is required")
		}
		if nodeCapacityFlag <= 0 {
			nodeCapacityFlag = 1
		}
		cfg := &node.Config{Gateway: gw.BaseURL, Orgs: nodeOrgsFlag, OwnerID: me, Slug: slug, Name: strings.TrimSpace(nodeNameFlag), Capacity: node.Fixed(nodeCapacityFlag)}
		// registering again must not drop what `mesh node login` recorded
		// nor the node's one id a registration before it had
		cfg.Latchkey, cfg.LatchkeyClient = prev.Latchkey, prev.LatchkeyClient
		cfg.NodeID = prev.NodeID
		cfg.Owner, cfg.Enterprise = owner, enterprise
		// the contract it was on decides whether this registration retires
		// the legacy per-org rows (only on the move to the owner contract);
		// the cached enterprise id holds only for the same enterprise
		cfg.Contract = prev.Contract
		if enterprise == prev.Enterprise {
			cfg.EnterpriseID = prev.EnterpriseID
		}
		reg, err := node.Register(ctx, registrar(gw, cfg), cfg, cfg.Orgs)
		if err != nil {
			_ = node.SaveAdoption(cfg, reg, nil)
			return err
		}
		// a fresh cfg: the contract is always new to it, so this saves
		if err := node.SaveRegistration(cfg, reg, nil); err != nil {
			return err
		}
		path, _ := node.ConfigPath()
		for _, note := range reg.Notes {
			fmt.Println(note)
		}
		if reg.Owned != nil {
			fmt.Printf("Registered %s, capacity %s. Saved %s.\nStart it with: mesh node start\n", node.StartLine(cfg, reg), cfg.Capacity, path)
			return nil
		}
		fmt.Printf("Registered node %s (%s) in %s, capacity %s. Saved %s.\nStart it with: mesh node start\n", cfg.Name, slug, strings.Join(reg.Scopes.Names(), ", "), cfg.Capacity, path)
		return nil
	},
}

// registrar is node.Register's steps on the platform client: the
// owner probe and the field it found, the orgs of the token's person,
// the enterprise's id from its slug (myEnterprises), the one
// registration on the owner contract (the node's own id, the owner's id
// and kind), the legacy registration per org, and the retirement of the
// legacy per-org rows on the move to the owner contract.
func registrar(gw *gateway.Client, cfg *node.Config) node.Registrar {
	return node.Registrar{
		TakesOwner: gw.TakesOwner,
		OwnerField: gw.OwnerField,
		ResolveEnterprise: func(ctx context.Context, slugOrID string) (string, error) {
			if _, err := uuid.Parse(strings.TrimSpace(slugOrID)); err == nil {
				return strings.TrimSpace(slugOrID), nil
			}
			es, err := gw.MyEnterprises(ctx)
			if err != nil {
				return "", fmt.Errorf("enterprise %q: my enterprises: %w", slugOrID, err)
			}
			return node.FindEnterprise(slugOrID, es)
		},
		LegacyOrgs: func(ctx context.Context) ([]gateway.Org, error) { return gw.MyOrgs(ctx) },
		Retire:     gw.RetireNode,
		Resolve:    func(ctx context.Context) (node.Scopes, error) { return node.ResolveScopes(ctx, gw, cfg) },
		ConfiguredOrg: func(ctx context.Context) (string, error) {
			orgs, err := gw.MyOrgs(ctx)
			if err != nil {
				return "", fmt.Errorf("my orgs: %w", err)
			}
			if len(cfg.Orgs) == 0 {
				// no restriction: the first org, unrestricted, as before
				if len(orgs) == 0 {
					return "", nil
				}
				return orgs[0].ID, nil
			}
			id, _ := node.ConfiguredOrgID(orgs, cfg.Orgs)
			return id, nil
		},
		RegisterOwner: func(ctx context.Context, nodeID, orgID, ownerID string) (*gateway.NodeAnswer, error) {
			return gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: nodeID, OrgID: orgID, OwnerID: ownerID, Name: cfg.Name, Slug: cfg.Slug,
				Harnesses: []string{"claude-code"}, Capacity: cfg.Capacity.Max, Version: version.Resolved(), Owner: cfg.OwnerKind()})
		},
		RegisterIn: func(ctx context.Context, scopes node.Scopes) error { return registerIn(ctx, gw, cfg, scopes) },
	}
}

// registerIn registers the node in every scope (a Node per org, id from
// (org, slug); registering again is a heartbeat).
func registerIn(ctx context.Context, gw *gateway.Client, cfg *node.Config, scopes node.Scopes) error {
	for _, sc := range scopes {
		if _, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: sc.NodeID, OrgID: sc.ID, OwnerID: cfg.OwnerID, Name: cfg.Name, Slug: cfg.Slug,
			Harnesses: []string{"claude-code"}, Capacity: cfg.Capacity.Max, Version: version.Resolved()}); err != nil {
			return fmt.Errorf("register in %s: %w", sc.Name(), err)
		}
	}
	return nil
}

var nodeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Heartbeat once, list the org's nodes and this machine's session scopes",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := node.LoadConfig()
		if err != nil {
			return err
		}
		src, err := tokenSource()
		if err != nil {
			return err
		}
		gw := gateway.NewWithSource(gatewayURL(), src)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// an owned node — by the contract the last registration saved —
		// heartbeats once under its one id, a nil answer included; a
		// per-org node per org as before (node.HeartbeatOutside)
		out, err := node.HeartbeatOutside(ctx, gw, cfg, version.Resolved(), gateway.HeartbeatExtras{Commit: version.Commit()},
			func(sc node.Scope, err error) error { return fmt.Errorf("heartbeat in %s: %w", sc.Name(), err) })
		if err != nil {
			return err
		}
		if line := ownedStatusLine(cfg, out); line != "" {
			fmt.Println(line)
		}
		if line := personalNodeStatusLine(); line != "" {
			fmt.Println(line)
		}
		scopes := out.Scopes
		fmt.Println(credentialLine(src.Credential()))
		state, err := node.DefaultRunState()
		if err != nil {
			return err
		}
		local, draining := localNodeState(state)
		fmt.Println(local)
		fmt.Println(takesStatusLine(cfg))
		fmt.Println(hostConfigLine(cfg.Host))
		scaling, scales := localScaling(state)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ORG\tNAME\tSLUG\tSTATUS\tRUNNING\tCAPACITY\tVERSION\tLAST_SEEN")
		for _, sc := range scopes {
			nodes, err := gw.Nodes(ctx, sc.ID)
			if err != nil {
				return err
			}
			for _, n := range nodes {
				mark, status, capacity := "", n.Status, fmt.Sprint(n.Capacity)
				if n.ID == sc.NodeID {
					mark = " *"
					if draining {
						status += ",draining"
					}
					if scales {
						capacity = scaling.String()
					}
				}
				fmt.Fprintf(w, "%s\t%s%s\t%s\t%s\t%d\t%s\t%s\t%s\n", sc.Name(), n.Name, mark, n.Slug, status, n.Running, capacity, n.Version, n.LastSeenAt)
			}
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if rows, ok := localStanding(state); ok {
			for _, line := range standingLines(rows) {
				fmt.Println(line)
			}
		}
		if held, ok := localHeld(state); ok {
			heldGoalsSection(os.Stdout, held)
		}
		if pid, err := state.PID(); err == nil && processAlive(pid) {
			rows, _ := state.Shares()
			fmt.Println()
			for _, line := range sharesLines(rows) {
				fmt.Println(line)
			}
		}
		if _, ok := sessionunit.Default().(sessionunit.SystemdScope); ok {
			printScopes(ctx, gw, sessionunit.HostSystemctl{})
		}
		return nil
	},
}

// printScopes is `mesh node status`'s sessions listing (ADR 008, The
// start-up sweep): this machine's mesh-session scopes (sc.ListScopes,
// judged by node.ScopeReport), each with its kind, subject and the
// platform's answer, and a `sessions:` line where only scopes of live
// sessions count as live.
func printScopes(ctx context.Context, gw node.Gateway, sc sessionunit.Systemctl) {
	rows, n, err := node.ScopeReport(ctx, gw, sc)
	if err != nil {
		fmt.Printf("sessions: %v\n", err)
		return
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCOPE\tKIND\tSUBJECT\tPLATFORM")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Unit, dash(r.Kind), dash(r.Subject), scopeAnswer(r))
	}
	_ = w.Flush()
	fmt.Printf("sessions: %d live scope(s), %d leaked\n", n.Live, n.Leaked)
}

// scopeAnswer is a scope row's PLATFORM column.
func scopeAnswer(r node.ScopeRow) string {
	switch r.Verdict {
	case node.ScopeLive:
		return fmt.Sprintf("live (%s %s)", r.What, r.Status)
	case node.ScopeTerminal:
		return fmt.Sprintf("terminal (%s %s)", r.What, r.Status)
	case node.ScopeUnknown:
		return "unknown (" + r.What + " not on the platform)"
	case node.ScopeUnread:
		return fmt.Sprintf("unread: %v", r.Err)
	case node.ScopeInactive:
		return r.Active
	case node.ScopeStrange:
		return "not a kind the node runs"
	}
	return string(r.Verdict)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ownedStatusLine is what `mesh node status` says of the owner contract:
// the owner and the orgs it serves, an orgs restriction the answer did
// not meet, or — on a nil answer — that the platform named none this
// time (naming node.yaml's own `enterprise:` when the node is one, since
// the answer carries none to fall back on) — followed, either way, by
// the node id line (nodeIDStatusLine), since node.yaml's node_id does
// not depend on this heartbeat's answer. A per-org node says nothing
// here.
func ownedStatusLine(cfg *node.Config, out node.Outside) string {
	if !out.Owned {
		return ""
	}
	var line string
	switch {
	case out.Answer == nil && cfg != nil && cfg.Enterprise != "":
		line = fmt.Sprintf("owner: %s (enterprise), the platform answered no orgs on this heartbeat", cfg.Enterprise)
	case out.Answer == nil:
		line = "owner contract: the platform answered no orgs on this heartbeat"
	default:
		o := node.OwnerFrom(out.Answer)
		line = fmt.Sprintf("%s (%s); serving %s", o.String(), o.Kind, strings.Join(out.Scopes.Names(), ", "))
	}
	if id := nodeIDStatusLine(cfg); id != "" {
		line += "\n" + id
	}
	if out.Answer != nil && out.Restrict != nil {
		line = fmt.Sprintf("orgs: %v\n%s", out.Restrict, line)
	}
	return line
}

// personalNodeStatusLine is `mesh node status`'s line for `mesh mcp`'s
// personal node (ADR-023, mesh mcp): empty until one has registered.
func personalNodeStatusLine() string {
	pn, err := node.LoadPersonalNode()
	if err != nil || pn.ID == "" {
		return ""
	}
	return fmt.Sprintf("personal node: %s (%s)", pn.Name, pn.ID)
}

// nodeIDStatusLine is mesh node status's line naming node.yaml's node_id
// and how it was arrived at (node_id_source, item 4 of
// a-node-takes-the-id-the-platform-names): "derived" when OwnedNodeID
// computed it, "adopted from the platform" after a refusal named it
// (node.IDAdopted), or "recorded" for a node_id saved before
// node_id_source existed (empty on a node registered before this
// goal). Empty when node.yaml has no node_id to show.
func nodeIDStatusLine(cfg *node.Config) string {
	if cfg == nil || cfg.NodeID == "" {
		return ""
	}
	switch cfg.NodeIDSource {
	case node.IDDerived:
		return fmt.Sprintf("node id: %s (derived)", cfg.NodeID)
	case node.IDAdopted:
		return fmt.Sprintf("node id: %s (adopted from the platform)", cfg.NodeID)
	default:
		return fmt.Sprintf("node id: %s (recorded)", cfg.NodeID)
	}
}

// statusHeartbeat is `mesh node status`'s one heartbeat: the version
// this binary reports (the tag, else the sha) and, beside it, the commit
// — sent like the loop's, only when known and only when the gateway's
// HeartbeatNodeInput takes it (mesh-specs ADR 013, trd §1 Node).
func statusHeartbeat(ctx context.Context, gw *gateway.Client, nodeID string) error {
	_, err := gw.HeartbeatNode(ctx, nodeID, version.Resolved(), 0, gateway.HeartbeatExtras{Commit: version.Commit()})
	return err
}

// localNodeState is the line `mesh node status` prints for the node
// process on this machine, from its pidfile and draining marker, and
// whether it is draining. The platform's node record carries no drain
// state (the heartbeat reports only the version and the running count),
// so this machine is the only place it is known.
func localNodeState(state node.RunState) (string, bool) {
	pid, err := state.PID()
	switch {
	case errors.Is(err, node.ErrNoNode):
		return "This machine: no node running.", false
	case err != nil:
		return "This machine: " + err.Error(), false
	case !processAlive(pid):
		return fmt.Sprintf("This machine: no node running (pid %d in %s is gone).", pid, state.PIDPath()), false
	case state.Draining():
		return fmt.Sprintf("This machine: node running (pid %d), draining — it claims nothing new and exits when its sessions end.", pid), true
	default:
		return fmt.Sprintf("This machine: node running (pid %d).", pid), false
	}
}

// takesStatusLine is `mesh node status`'s line naming the kinds of
// session node.yaml's `takes:` claims on this machine, printed after the
// "This machine:" line (ADR-025, item 6): "Takes: every kind" when
// node.yaml names none, "Takes: evaluate (at most 2)" for a cap.
func takesStatusLine(cfg *node.Config) string {
	return "Takes: " + cfg.Takes.String()
}

// hostConfigLine is `mesh node status`'s `host:` line: the thresholds
// node.yaml's `host:` block is in force with, defaults applied
// (HostConfig.Effective), so what judges the host hot is visible beside
// its reading on the node row (adr-004's amendment 2026-10-03).
func hostConfigLine(h node.HostConfig) string {
	h = h.Effective()
	return fmt.Sprintf("host: load_per_core_max %s, package_temp_max_c %s", hostStatusNum(h.LoadPerCoreMax), hostStatusNum(h.PackageTempMaxC))
}

// hostStatusNum is a host: threshold as hostConfigLine shows it: "1.5",
// "90" — never a trailing ".0".
func hostStatusNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// localScaling is the capacity the node on this machine has in force
// when node.yaml's capacity is a range, as it left it at its last tick,
// and true; false when it is fixed or no node runs here. The platform's
// node record carries the registered capacity, and beside it the
// heartbeat's scaling.capacity — the capacity in force, fixed or ranged
// (ADR-018).
func localScaling(state node.RunState) (node.Scaling, bool) {
	pid, err := state.PID()
	if err != nil || !processAlive(pid) {
		return node.Scaling{}, false
	}
	return state.Scaling()
}

// localHeld is the goals the node on this machine holds off claiming
// for, as its last tick left them (state.Held), and true when a node
// runs here — the same pid rule as localScaling — whether or not it
// holds any; false (and nil) when no node runs here.
func localHeld(state node.RunState) ([]node.HeldGoal, bool) {
	pid, err := state.PID()
	if err != nil || !processAlive(pid) {
		return nil, false
	}
	held, _ := state.Held()
	return held, true
}

// localStanding is each scheduled account's clock standing as the last
// tick left it (state.Standing), and true when a node runs here — the
// same pid rule as localScaling and localHeld; false (and nil) when no
// node runs here (goal every-subscription-lands-full-at-its-reset,
// item 6).
func localStanding(state node.RunState) ([]node.AccountStanding, bool) {
	pid, err := state.PID()
	if err != nil || !processAlive(pid) {
		return nil, false
	}
	rows, _ := state.Standing()
	return rows, true
}

// standingLines is `mesh node status`'s account standing table, one
// line per account in label order: each window's own use against its
// target line and how far behind or ahead it stands, the seven-day
// window's reset, push and the account's pace mode when either applies
// (goal every-subscription-lands-full-at-its-reset, item 6). Nothing
// with no rows.
func standingLines(rows []node.AccountStanding) []string {
	out := make([]string, 0, len(rows))
	for _, a := range rows {
		line := fmt.Sprintf("account %s: week %s used, target %s (%s), resets %s; five-hour %s/%s (%s)",
			a.Label, standingPct(a.SevenDay.Utilization), standingPct(a.SevenDay.Target), standingDistance(a.SevenDay.Distance),
			formatHeldTime(a.SevenDay.ResetsAt),
			standingPct(a.FiveHour.Utilization), standingPct(a.FiveHour.Target), standingDistance(a.FiveHour.Distance))
		if a.Push {
			line += "; push"
		}
		if a.Mode != "" {
			line += "; " + a.Mode
		}
		out = append(out, line)
	}
	return out
}

// standingPct is a clock-standing fraction as a whole percent.
func standingPct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

// standingDistance is a window's Distance as `mesh node status` shows
// it: how far behind (negative) or ahead (positive) of the target
// line, as a whole percent with the word.
func standingDistance(d float64) string {
	word := "behind"
	if d > 0 {
		word = "ahead"
	}
	return fmt.Sprintf("%.0f%% %s", math.Abs(d)*100, word)
}

// heldGoalsSection is `mesh node status`'s report of the goals this
// node holds off claiming for (HeldGoal, goal
// a-refused-plan-is-released-not-replanned): a table naming each
// goal's org, kind, since, until and why, or "Held goals: none."
// with nothing held.
func heldGoalsSection(out io.Writer, held []node.HeldGoal) {
	if len(held) == 0 {
		fmt.Fprintln(out, "Held goals: none.")
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Held goals")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ORG\tGOAL\tKIND\tSINCE\tUNTIL\tWHY")
	for _, h := range held {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", h.Org, h.Slug, h.Kind, formatHeldTime(h.Since), formatHeldTime(h.Until), h.Reason)
	}
	_ = w.Flush()
}

// sharesLines is `mesh node status`'s table of each served org's share
// of the node as its last pass left it in node.shares (ADR-023,
// Reporting): ORG, FLOOR, HELD and READY, a ready read that failed shown
// as "-"; "shares: none recorded" with no rows.
func sharesLines(rows []node.HoldingOrg) []string {
	if len(rows) == 0 {
		return []string{"shares: none recorded"}
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ORG\tFLOOR\tHELD\tREADY")
	for _, r := range rows {
		ready := fmt.Sprint(r.Ready)
		if r.Ready < 0 {
			ready = "-"
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", r.Org, r.Floor, r.Held, ready)
	}
	_ = w.Flush()
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
}

// formatHeldTime is a HeldGoal's Since or Until on the status table:
// RFC 3339, or "-" for Until when the hold has none (a budget or pause
// refusal holds until the goal's row changes, not a fixed time).
func formatHeldTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

var nodeDrainCmd = &cobra.Command{
	Use:   "drain",
	Short: "Drain the node running on this machine: it claims nothing new, lets its sessions finish, then exits",
	Long: `Sends the running node (its pid is in ~/.mesh/node.pid) SIGUSR1 — the
same as ` + "`kill -USR1 <pid>`" + `. The node stops claiming items, plans, research
and evaluations, lets every session it has running end, and exits 0;
under systemd or launchd it is started again, on the binary and config
now on disk. ` + "`mesh node status`" + ` shows it as draining meanwhile.

Capacity and the orgs served need no drain: the node reads them every
tick.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := node.DefaultRunState()
		if err != nil {
			return err
		}
		return drainNode(state, cmd.OutOrStdout())
	},
}

// drainNode signals the node whose pid state holds to drain.
func drainNode(state node.RunState, out io.Writer) error {
	pid, err := state.PID()
	if err != nil {
		return err
	}
	if !processAlive(pid) {
		return fmt.Errorf("no node is running on this machine: pid %d in %s is gone", pid, state.PIDPath())
	}
	if err := signalDrain(pid); err != nil {
		return fmt.Errorf("signal node (pid %d): %w", pid, err)
	}
	fmt.Fprintf(out, "Node (pid %d) is draining: it claims nothing new and exits once its sessions have ended.\n", pid)
	return nil
}

// onDrainSignal calls drain on every drain signal until stop is called.
func onDrainSignal(drain func()) (stop func()) { return onSignal(drainSignal, drain) }

// onRestartSignal calls restart on every restart signal (SIGHUP) until
// stop is called.
func onRestartSignal(restart func()) (stop func()) { return onSignal(restartSignal, restart) }

// onSignal calls fn on every sig until stop is called; a nil sig (not
// on this OS) never calls it.
func onSignal(sig os.Signal, fn func()) (stop func()) {
	if sig == nil {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sig)
	done := make(chan struct{})
	go func() {
		defer crash.Guard()
		for {
			select {
			case <-ch:
				fn()
			case <-done:
				return
			}
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}

var nodeLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove ~/.mesh/token",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := auth.ClearToken(); err != nil {
			return err
		}
		fmt.Println("Credential removed from this machine. Revoke it for good in the Latchkey console (Machines) — or, for an rs_ token, under Settings → Tokens on the runsheet console.")
		return nil
	},
}

var nodeStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Run the node in the foreground (channel mode by default; --mode poll for claude -p per item)",
	Long: `Every --interval the node heartbeats, lists the org's ready work items and
claims up to its capacity. In channel mode (the default) one persistent
claude session runs with the mesh channel attached and each claimed item
is pushed into it as a work_item.ready event; the session reports back
through the channel's tools. In poll mode each claimed item runs its own
"claude -p" session and the node reports, pushes and finishes for it.

Channels are a Claude Code research preview: the session is started with
--dangerously-load-development-channels server:mesh and needs claude.ai
(Max) or Console auth — not Bedrock/Vertex. --unattended adds
--dangerously-skip-permissions so the session never waits on a prompt;
only use it on a machine you trust with that.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// a panic anywhere on the start path is one line in node.log
		// and exit 70, not Go's bare status 2 (internal/crash)
		defer crash.Guard()
		// the executable as it is now: a restart (SIGHUP, `mesh node
		// upgrade`) execs this path, which by then holds the new binary
		exe, exeErr := node.Executable()
		logs, err := openNodeLogs()
		if err != nil {
			return err
		}
		// a panic's fatal line and stack go to node.log as well as to
		// stderr (the journal, node.crash.log): the node owns its log now
		crash.Out = io.MultiWriter(os.Stderr, logs.Node.Writer())
		err = startNode(cmd, args, logs.Node)
		if !errors.Is(err, node.ErrRestart) {
			// the error that ends a start is in node.log too, not only
			// on stderr (the journal under systemd)
			if err != nil {
				logs.Node.Printf("exiting: %v", err)
			}
			logs.Close()
			return err
		}
		if exeErr != nil {
			logs.Close()
			return fmt.Errorf("restart: %w", exeErr)
		}
		// in node.log and on stderr: if the exec fails, the service's
		// own log (the journal, node.crash.log) says what was tried
		logs.Node.Printf("restarting: exec %s", exe)
		log.New(os.Stderr, "mesh node: ", log.LstdFlags).Printf("restarting: exec %s", exe)
		logs.Close()
		return execSelf(exe)
	},
}

// startNode is nodeStart; a test replaces it to panic on the start path.
var startNode = nodeStart

// nodeStart is `mesh node start` up to Run's end; node.ErrRestart asks
// its caller to exec the binary again, once the pidfile and the signal
// handlers are gone.
func nodeStart(cmd *cobra.Command, args []string, logger *log.Logger) error {
	// a pending update's confirm window opens here, at process start
	started := time.Now()
	// a new binary that has failed to start twice is rolled back
	// first, before anything else in it can fail again
	updates, exe, check := startUpdates(logger)
	if check.RolledBack {
		reportRollback(check, logger)
		return nil
	}
	// node.log's first lines, before anything that can block on the
	// platform: a silent exit before this point can never happen again
	cfgPath, _ := node.ConfigPath()
	for _, line := range node.StartLines(version.Resolved(), cfgPath, check) {
		logger.Print(line)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, gw, reg, workRoot, err := registerNode(ctx, cmd, logger, updates, check, started)
	if errors.Is(err, node.ErrUnconfirmed) {
		return endRun(err, logger)
	}
	if err != nil {
		return err
	}
	scopes, owned := reg.Scopes, reg.Owned

	// every session's plan standing lands in Limits: the heartbeat
	// carries it and the loop paces by it
	loop := &node.Loop{GW: gw, Cfg: cfg, Scopes: scopes, Owned: owned, ClaimsDir: node.ClaimsDir(workRoot), Version: version.Resolved(), Commit: version.Commit(), Harness: "claude-code", Interval: nodeIntervalFlag, Log: logger, Limits: &node.Limits{},
		// a ranged capacity scales by the plan's windows, capped by this
		// machine's load and memory (adr-004)
		Machine: node.HostMachine(),
		// every tick reports the tool runtimes and agent CLIs on PATH
		LookPath: exec.LookPath,
		// an item declined for a runtime it lacks is not taken again
		Declines: &node.Declines{}}
	// every session's list-price cost lands in the ledger, kept on
	// disk: the api keys' caps and the project budgets count it
	// (adr-005-capacity-across-accounts)
	ledgerPath, err := node.LedgerPath()
	if err != nil {
		return err
	}
	if loop.Ledger, err = node.OpenLedger(ledgerPath, nil); err != nil {
		return err
	}
	// the accounts the platform's claim-time read has issued lately,
	// remembered across a restart (goal loop-schedules-on-issued-accounts):
	// the loop paces, chooses and scales on them from its first tick,
	// and Credentials notes each one it is issued into the same store
	knownPath, err := node.KnownAccountsPath()
	if err != nil {
		return err
	}
	known, err := node.OpenKnownAccounts(knownPath, nil)
	if err != nil {
		return err
	}
	loop.Known = known
	// each known issued account, what it serves and its own pace point,
	// before the first claim (adr-005's amendment 2026-10-02)
	for _, line := range node.IssuedAccountLines(known.Accounts(time.Now())) {
		logger.Print(line)
	}
	// the node follows the version the platform asks for (adr-002);
	// --once is one tick, too short for an update to finish
	if updates != nil && !nodeOnceFlag {
		loop.Update = &node.ReleaseUpdater{Source: selfupdate.GitHub{}, State: updates, Exe: exe}
		// and the new version's node image is pulled before the
		// drain (mesh-specs ADR 007)
		loop.Image = ensureNodeImage
		if check.Failed != "" {
			loop.NoteUpdateFailure(check.Failed, check.Reason, check.Final, check.RetryNow)
		}
		// a pending update is confirmed by the first heartbeat through,
		// retried for node.yaml's update.confirm_within from start
		if check.Pending {
			loop.Confirming, loop.ConfirmWithin, loop.Started = check.To, cfg.ConfirmWithin(), started
		}
	}
	// capacity and the orgs are read again every tick, so a changed
	// node.yaml or a newly claimed org needs no restart: an owned node's
	// orgs come from each heartbeat's answer, never from MyOrgs
	if owned != nil {
		loop.Reload = node.OwnedReloader(nil)
	} else {
		loop.Reload = node.Reloader(gw, cfg, scopes, nil, func(ctx context.Context, sc node.Scope) error {
			return registerIn(ctx, gw, cfg, node.Scopes{sc})
		})
	}
	// the registration's answer is the loop's before its first tick: a
	// changed orgs restriction applies to it on reload even while every
	// heartbeat since answers nil
	loop.TakeAnswer(reg.Answer)
	// which of the served orgs' projects no credential is open to this
	// node for, said once the first heartbeat has answered the orgs:
	// node.yaml's accounts are retired, so those projects' work waits
	// (goal a-session-never-runs-on-the-machines-own-login)
	loop.FirstBeat = func(ctx context.Context, scopes node.Scopes) { reportCredentials(ctx, gw, loop, scopes, logger) }
	// SIGUSR1 (`mesh node drain`) drains: nothing new is claimed and
	// the loop returns nil once every session has ended, so the
	// process exits 0 and its service manager starts the new binary
	state, err := node.DefaultRunState()
	if err != nil {
		return err
	}
	pid := os.Getpid()
	if err := state.Started(pid); err != nil {
		return fmt.Errorf("pidfile: %w", err)
	}
	defer state.Stopped(pid)
	// a ranged capacity in force is left beside the pidfile for
	// `mesh node status`; a fixed one leaves nothing
	loop.Scaled = func(s node.Scaling, ranged bool) {
		if err := state.WriteScaling(s, ranged); err != nil {
			logger.Printf("scaling state: %v", err)
		}
	}
	// the goals this node holds off claiming for are left beside the
	// pidfile too, for `mesh node status` (goal
	// a-refused-plan-is-released-not-replanned)
	loop.Held = func(h []node.HeldGoal) {
		if err := state.WriteHeld(h); err != nil {
			logger.Printf("held state: %v", err)
		}
	}
	// and each served org's floor, held and ready (ADR-023, Reporting)
	loop.Shares = func(rows []node.HoldingOrg) {
		if err := state.WriteShares(rows); err != nil {
			logger.Printf("shares state: %v", err)
		}
	}
	// and each scheduled account's clock standing (goal
	// every-subscription-lands-full-at-its-reset, item 6)
	loop.Standing = func(rows []node.AccountStanding) {
		if err := state.WriteStanding(rows); err != nil {
			logger.Printf("standing state: %v", err)
		}
	}
	defer onDrainSignal(func() {
		if err := state.MarkDraining(); err != nil {
			logger.Printf("draining marker: %v", err)
		}
		loop.Drain()
	})()
	// SIGHUP (`mesh node upgrade` on a node no service runs) restarts
	// at once: every session is left running in its own unit, the
	// process execs its binary again, and the new one adopts them
	// (adr-009). A service's own restart (SIGTERM) leaves them the
	// same way: everything below runs in the loop's context.
	ctx = loop.Context(ctx)
	defer onRestartSignal(loop.Restart)()
	// one old-platform notice for the whole run, whatever runs the items
	fallback := &node.TokenFallback{}
	// every one-shot session runs in its own pod unless node.yaml
	// says `sandbox: off` or, leaving it out, podman does not answer;
	// an explicit `sandbox: on` without podman refuses to start (ADR 007)
	sb, sbInfo, err := node.OpenSandbox(ctx, cfg.Sandbox, &sandbox.Podman{}, version.Resolved(), logger.Printf)
	if err != nil {
		return err
	}
	if sb != nil {
		sb.Config.TZ = os.Getenv("TZ")
	}
	sessionSandbox, loop.Sandbox = sb, sbInfo
	// every one-shot session's account comes from the platform's
	// claim-time read, node.yaml the fallback, and every one reports
	// its usage record (adr-005); each fallback is said once per run
	// a read that issues no credential for the adapter is a hold, never
	// the machine's own login: the loop leaves that project's work
	// unclaimed and says why (adr-005's amendment 2026-10)
	loop.NoCredential = &node.NoCredentials{Log: logger, Now: loop.Now}
	accounts := &node.Credentials{GW: gw, Limits: loop.Limits, Thresholds: loop.Thresholds, Known: known, NoCredential: loop.NoCredential, Log: logger}
	switch nodeModeFlag {
	case "poll":
		return runPollMode(ctx, cfg, scopes, gw, loop, workRoot, fallback, accounts, logger)
	case "channel", "":
		return runChannelMode(ctx, cfg, scopes, gw, loop, workRoot, fallback, accounts, logger)
	default:
		return fmt.Errorf("--mode must be channel or poll")
	}
}

// hostUnit is the unit sessions run in on this machine; a test of the
// start path replaces it so the start-up sweep never judges the host's
// own scopes against a fake platform.
var hostUnit = sessionunit.Default

// sessionsOut is where what sessions say goes — the workspaces' git
// commands, the agents' stderr, channel mode's text: ~/.mesh/sessions.log
// once `mesh node start` has opened it, so node.log keeps the node's own
// lines; stderr for every other command.
var sessionsOut io.Writer = os.Stderr

// openNodeLogs opens ~/.mesh/node.log and ~/.mesh/sessions.log, each
// capped and rotated (node.NodeLogMaxBytes, node.SessionsLogMaxBytes),
// points sessionsOut at the second, and echoes both to stderr when it
// is a terminal — a node started by hand. Under a service stderr is the
// journal or node.crash.log, which get neither.
func openNodeLogs() (*node.NodeLogs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var echo io.Writer
	if term.IsTerminal(int(os.Stderr.Fd())) {
		echo = os.Stderr
	}
	logs, err := node.OpenNodeLogs(home, echo)
	if err != nil {
		return nil, fmt.Errorf("node log: %w", err)
	}
	sessionsOut = logs.SessionsOut
	return logs, nil
}

// channelText logs a channel session's text, its first line, on the
// sessions log: what a session says is not the node's reasoning.
func channelText(sessions *log.Logger) func(string) {
	return func(t string) { sessions.Printf("session: %s", firstLine(t)) }
}

// observed is a one-shot harness whose sessions report their plan
// standing into the loop's Limits and their cost into its Ledger.
func observed(loop *node.Loop, logger *log.Logger) harness.Harness {
	return node.ObserveSpend(node.ObserveLimits(oneShotHarness(), loop.Limits), loop.Ledger, logger.Printf)
}

func runPollMode(ctx context.Context, cfg *node.Config, scopes node.Scopes, gw *gateway.Client, loop *node.Loop, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) error {
	h := observed(loop, logger)
	// an item's handed-back build leaves what it spent on beside the
	// pidfile for the item's next claim (ADR 012 Hand-back)
	var state *node.RunState
	if st, err := node.DefaultRunState(); err == nil {
		state = &st
	}
	runner := &node.Runner{GW: gw, Harness: h, WS: &node.Git{Log: sessionsOut}, Cfg: cfg, Scopes: scopes, WorkRoot: workRoot, MCPFunc: func(ctx context.Context) map[string]any { return meshContextMCP(ctx, gw) }, MaxBudgetUSD: nodeMaxBudgetFlag, Model: nodeModels.For("build"), Fallback: fallback, Credentials: accounts, Runtimes: loop.ToolRuntimes, Declines: loop.Declines, Condition: loop.NoteCondition, Budgets: loop.KindModelBudget, ContextBudget: loop.ContextBudget, Sandbox: sessionSandbox, MeshBinary: meshBinary(), State: state, Notices: validationNotices, Log: logger}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		r := *runner
		r.Scopes = loop.CurrentScopes() // an org served since start is known to its build
		_ = r.RunItem(ctx, item)
	}
	// one per node: the evaluations of every tick and the adopted ones
	// share what has been said once per item
	second := codexSecondOpinion(cfg, loop, workRoot, logger)
	loop.Evaluate = evaluateWith(gw, cfg, h, second, loop, workRoot, fallback, accounts, logger)
	loop.Plan = planWith(gw, cfg, h, loop, workRoot, accounts, logger)
	loop.PlanReleased = (&node.Planner{WorkRoot: workRoot, Log: logger}).Released
	loop.Research = researchWith(gw, cfg, h, loop, workRoot, accounts, logger)
	loop.Rebase = rebaseWith(gw, cfg, h, loop, loop.CurrentScopes, workRoot, fallback, accounts, logger)
	logger.Printf("poll mode: node %s (%s) for %s, capacity %s, every %s", cfg.Name, cfg.Slug, strings.Join(scopes.Names(), ", "), cfg.Capacity, loop.Interval)
	// sessions the process before this one started run on in their own
	// units: each is taken up where it is, before anything is claimed
	found := adoptSessions(ctx, gw, cfg, h, second, loop, runner, workRoot, fallback, accounts, logger)
	// plan and research claims it held but had not opened are taken again
	reopenClaims(ctx, loop, workRoot, found, logger)
	// items this node held when it last stopped are taken up again: a
	// restart mid-build continues them (the workspace keeps the branch)
	for _, sc := range scopes {
		for _, status := range []string{"claimed", "running"} {
			items, err := gw.OrgWorkItems(ctx, sc.ID, status)
			if err != nil {
				logger.Printf("list held items of %s: %v", sc.Name(), err)
				continue
			}
			// an owned node holds, too, what it claimed under its
			// per-org id before it was owned (ADR-015)
			legacy, _ := cfg.NodeIDIn(sc.ID)
			for _, it := range items {
				if it.NodeID == sc.NodeID || (loop.Owned != nil && it.NodeID == legacy) {
					loop.Adopt(ctx, it)
				}
			}
		}
	}
	if nodeOnceFlag {
		loop.Tick(ctx)
		return nil
	}
	err := loop.Run(ctx)
	if errors.Is(err, context.Canceled) {
		logger.Printf("stopped; running items were left for a retry")
		return nil
	}
	return endRun(err, logger)
}

// registerNode loads node.yaml, refuses to start on an account whose
// secret cannot be read, makes the work root and registers the node with
// the platform: everything a start needs before the loop exists. While
// an update is unconfirmed, an error here — or a panic — is recorded
// (noteStartFailure) so a rollback names the real cause instead of the
// bare "did not start": this is the same 60-second registration timeout
// that once left node.log silent when the platform was slow.
func registerNode(ctx context.Context, cmd *cobra.Command, logger *log.Logger, updates *selfupdate.StateFile, check node.StartCheck, started time.Time) (cfg *node.Config, gw *gateway.Client, reg node.Registered, workRoot string, err error) {
	defer func() {
		if r := recover(); r != nil {
			noteStartFailure(updates, check, fmt.Sprintf("panic: %v", r), logger)
			panic(r)
		}
		// a platform that could not be reached inside the confirm window
		// is slow, not a start that failed: its error is already recorded
		if err != nil && !errors.Is(err, node.ErrUnconfirmed) {
			noteStartFailure(updates, check, err.Error(), logger)
		}
	}()
	cfg, err = node.LoadConfig()
	if err != nil {
		return
	}
	nodeModels = modelsFor(cmd, cfg)
	gw, err = platformClient()
	if err != nil {
		return
	}
	workRoot, err = node.WorkRoot()
	if err != nil {
		return
	}
	if err = os.MkdirAll(workRoot, 0o755); err != nil {
		return
	}

	register := func(ctx context.Context) error {
		regCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if cfg.OwnerID == "" {
			owner, err := gw.SignIn(regCtx)
			if err != nil {
				return err
			}
			cfg.OwnerID = owner
		}
		got, err := node.Register(regCtx, registrar(gw, cfg), cfg, cfg.Orgs)
		if err != nil {
			// an id the refusal named is kept even when the retry under
			// it was refused too: the next attempt registers under it
			for _, note := range got.Notes {
				logger.Print(note)
			}
			if serr := node.SaveAdoption(cfg, got, nil); serr != nil {
				logger.Print(serr)
			}
			return err
		}
		// the contract and the node's one id are kept: the next start
		// registers under it, status and the rollback reporter read it
		if err := node.SaveRegistration(cfg, got, nil); err != nil {
			return err
		}
		reg = got
		// node.log's first line after StartLines names the owner and the
		// orgs: "node mini (runsheet-mini) owned by Chris, serving …";
		// what the registration has to say follows it
		for _, line := range node.RegistrationLines(cfg, reg) {
			logger.Print(line)
		}
		return nil
	}
	if updates == nil || !check.Pending {
		if err = register(ctx); err != nil {
			err = fmt.Errorf("register node: %w", err)
		}
		return
	}
	// a pending update: the platform being slow is not the binary
	// failing, so registration is retried inside the confirm window
	w := confirmWindow{To: check.To, Started: started, Within: cfg.ConfirmWithin(), Every: nodeIntervalFlag, Note: updates.NoteConfirmError, Log: logger}
	err = w.retry(ctx, register)
	return
}

// reportCredentials logs which projects of the orgs served have no
// credential open to this node, for claude-code and, when its CLI is
// on PATH, codex. It is best effort: a refusal is one line, and nothing
// it reads stops the start.
func reportCredentials(ctx context.Context, gw node.CoverageGateway, loop *node.Loop, scopes node.Scopes, logger *log.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, adapters := loop.Runtimes()
	node.ReportCoverage(ctx, gw, scopes, node.CoverageAdapters(adapters), logger.Printf)
}

// confirmWindow retries what a pending update's new process must reach
// the platform for — its registration — until the window from Started
// elapses (node.yaml update.confirm_within). Now and Sleep are replaced
// in tests.
type confirmWindow struct {
	To      string
	Started time.Time
	Within  time.Duration
	// Every is the wait between attempts: the heartbeat interval.
	Every time.Duration
	// Note records a failed attempt's error in update.json.
	Note  func(reason string) error
	Log   *log.Logger
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// retry runs attempt until it goes through, logging and recording each
// failure with its error; the window elapsing ends it with an
// *node.UnconfirmedError naming the last error, and a cancelled ctx with
// ctx's error.
func (w confirmWindow) retry(ctx context.Context, attempt func(context.Context) error) error {
	now, sleep := w.Now, w.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepCtx
	}
	every := w.Every
	if every <= 0 {
		every = 30 * time.Second
	}
	deadline := w.Started.Add(w.Within)
	for {
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("register node: %w", err)
		}
		if nerr := w.Note(err.Error()); nerr != nil {
			w.Log.Printf("self-update: record confirm error: %v", nerr)
		}
		left := deadline.Sub(now())
		if left <= 0 {
			return &node.UnconfirmedError{Version: w.To, Window: w.Within, Last: fmt.Errorf("register node: %w", err)}
		}
		w.Log.Printf("self-update: confirming %s: register node: %s; retrying for %s more", w.To, node.PlatformSlow(err), left.Round(time.Second))
		if err := sleep(ctx, min(every, left)); err != nil {
			return err
		}
	}
}

// sleepCtx waits d, or less when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// noteStartFailure records why a start of a pending update ended before
// it could confirm, so Rollback (after MaxStarts of them) names the real
// cause. A start with no update pending, or a state file that cannot
// record it, is logged and nothing more.
func noteStartFailure(updates *selfupdate.StateFile, check node.StartCheck, reason string, logger *log.Logger) {
	if updates == nil || !check.Pending {
		return
	}
	logger.Printf("self-update: %s", reason)
	if err := updates.NoteStartFailure(reason); err != nil {
		logger.Printf("self-update: record start failure: %v", err)
	}
}

// startUpdates counts this start against a pending self-update and rolls
// the binary back when it is the third unconfirmed one. A state file or
// executable it cannot read leaves the node running without updates.
func startUpdates(logger *log.Logger) (*selfupdate.StateFile, string, node.StartCheck) {
	f, err := selfupdate.DefaultStateFile()
	if err == nil {
		var exe string
		if exe, err = node.Executable(); err == nil {
			var check node.StartCheck
			// the window a rollback names: node.yaml's when it reads,
			// though it may be what this start fails on
			window := selfupdate.DefaultConfirmWithin
			if cfg, cerr := node.LoadSettings(); cerr == nil {
				window = cfg.ConfirmWithin()
			}
			if check, err = node.CheckStart(f, exe, window); err == nil {
				if line := check.String(); line != "" {
					logger.Print(line)
				}
				return f, exe, check
			}
		}
	}
	logger.Printf("self-update: %v; not updating this run", err)
	return nil, "", node.StartCheck{}
}

// reportRollback puts a rollback's reason on one heartbeat of each of the
// node's orgs, as the version rolled back to, before the process exits
// for that version to start. It does what it can: the exit follows.
func reportRollback(check node.StartCheck, logger *log.Logger) {
	cfg, err := node.LoadConfig()
	if err != nil {
		logger.Printf("self-update: report rollback: %v", err)
		return
	}
	gw, err := platformClient()
	if err != nil {
		logger.Printf("self-update: report rollback: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reportRollbackVia(ctx, gw, cfg, check, logger)
}

// reportRollbackVia is reportRollback on a given platform. An owned
// node — by the contract its last registration saved — reports it once,
// under its one id, and a nil answer there ends it: it is never reached
// per org as well. A per-org node is reached per org alone, whatever
// node_id it kept. The rollback reports the version it rolls back to,
// not this binary's; this binary's commit would not be that version's,
// so the heartbeat carries none and the restored binary's first one does.
func reportRollbackVia(ctx context.Context, gw node.OutsideGateway, cfg *node.Config, check node.StartCheck, logger *log.Logger) {
	_, err := node.HeartbeatOutside(ctx, gw, cfg, check.From, gateway.HeartbeatExtras{UpdateError: check.Reason},
		func(sc node.Scope, err error) error {
			logger.Printf("self-update: report rollback to %s: %v", sc.Name(), err)
			return nil
		})
	if err != nil {
		logger.Printf("self-update: report rollback: %v", err)
	}
}

// endRun is how the node's process ends when Run returns: 0 on a drain
// and on a self-update's restart, which the service manager answers by
// starting the binary now in place; 1 on a pending update that could not
// confirm inside its window (node.ErrUnconfirmed).
func endRun(err error, logger *log.Logger) error {
	switch {
	case err == nil:
		logger.Printf("drained; exiting")
	case errors.Is(err, node.ErrRestartForUpdate):
		logger.Printf("updated; exiting for the service manager to start the new binary; its sessions are left for it to adopt")
		return nil
	case errors.Is(err, node.ErrRestart):
		logger.Printf("restarting; its sessions are left for the new process to adopt")
	case errors.Is(err, node.ErrUnconfirmed):
		// exit 1: the service starts it again, and the third such start
		// rolls back naming this error
		logger.Printf("self-update: %v; exiting for the service to start it again", err)
	}
	return err
}

func runChannelMode(ctx context.Context, cfg *node.Config, scopes node.Scopes, gw *gateway.Client, loop *node.Loop, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) error {
	sock, err := channel.SockPath()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// each held item is a slot: its event, kept for a re-push, and the
	// outcome the session reports through the channel's tools
	type slot struct {
		content  string
		meta     map[string]string
		done     chan string
		pushedAt time.Time
		// cred is the item's GitHub token on its workspace, renewed by
		// repush (nil on the old platform's fallback)
		cred *node.ItemCred
	}
	slots := map[string]*slot{}
	var mu sync.Mutex
	hub := &channel.Hub{OnDone: func(itemID, outcome string) {
		mu.Lock()
		s := slots[itemID]
		delete(slots, itemID)
		mu.Unlock()
		if s != nil {
			s.done <- outcome
		}
	}}
	if err := hub.Listen(sock); err != nil {
		return fmt.Errorf("channel hub: %w", err)
	}
	defer hub.Close()

	sess := &channel.Session{Cfg: cfg, Orgs: scopes.Names(), MCPFunc: func() map[string]any { return meshContextMCP(ctx, gw) }, WorkDir: filepath.Join(workRoot, cfg.Slug), Exe: exe, SockPath: sock, Unattended: nodeUnattended, Model: nodeModels.For("channel"), Stderr: sessionsOut, Log: logger,
		OnText:      channelText(log.New(sessionsOut, "mesh node: ", log.LstdFlags)),
		OnRateLimit: func(rl *harness.RateLimit) { loop.Limits.Observe(node.Account(os.Environ()), rl) }}
	var sessMu sync.Mutex
	sessionID := ""
	sess.OnSessionID = func(id string) { sessMu.Lock(); sessionID = id; sessMu.Unlock() }
	// an event goes both ways: the channel notification and a user
	// message on the session's stdin, so it is worked even if the
	// notification is missed
	push := func(content string, meta map[string]string) {
		sessMu.Lock()
		if sessionID != "" {
			meta["session_id"] = sessionID
		}
		sessMu.Unlock()
		hub.Push(content, meta)
		if err := sess.SendFor(meta["item_id"], channel.UserMessage(content, meta)); err != nil {
			logger.Printf("session stdin: %v (the channel still has it)", err)
		}
		if id := meta["item_id"]; id != "" {
			mu.Lock()
			if s := slots[id]; s != nil {
				s.pushedAt = time.Now()
			}
			mu.Unlock()
		}
	}

	ws := &node.Git{Log: sessionsOut}
	// the one-shot sessions the process before this one started run on
	// in their own units: each is taken up where it is (a build of a
	// poll-mode run before this one too), before anything is claimed
	oneShot := observed(loop, logger)
	// one per node: the evaluations of every tick and the adopted ones
	// share what has been said once per item
	second := codexSecondOpinion(cfg, loop, workRoot, logger)
	found := adoptSessions(ctx, gw, cfg, oneShot, second, loop, &node.Runner{GW: gw, Harness: oneShot, WS: ws, Cfg: cfg, Scopes: scopes, WorkRoot: workRoot, Fallback: fallback, Credentials: accounts, Runtimes: loop.ToolRuntimes, Declines: loop.Declines, Condition: loop.NoteCondition, Budgets: loop.KindModelBudget, ContextBudget: loop.ContextBudget, Log: logger}, workRoot, fallback, accounts, logger)
	loop.Evaluate = evaluateWith(gw, cfg, observed(loop, logger), second, loop, workRoot, fallback, accounts, logger)
	loop.Plan = planWith(gw, cfg, observed(loop, logger), loop, workRoot, accounts, logger)
	loop.PlanReleased = (&node.Planner{WorkRoot: workRoot, Log: logger}).Released
	loop.Research = researchWith(gw, cfg, observed(loop, logger), loop, workRoot, accounts, logger)
	loop.Rebase = rebaseWith(gw, cfg, observed(loop, logger), loop, loop.CurrentScopes, workRoot, fallback, accounts, logger)
	reopenClaims(ctx, loop, workRoot, found, logger)
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		ready, err := readyChannelItem(ctx, gw, ws, loop.CurrentScopes(), item, workRoot, fallback, logger.Printf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if ce := (*node.CredentialError)(nil); errors.As(err, &ce) {
				sc, _ := loop.CurrentScopes().ByOrg(item.OrgID)
				_ = node.ReleaseOnCredential(ctx, gw, item.ID, sc.NodeID, ce, logger.Printf)
				return
			}
			logger.Printf("item %s: failed: %v", item.ID, err)
			_ = gw.FailWorkItem(ctx, item.ID, err.Error())
			return
		}
		res := ready.Res
		content, meta := ready.Content, ready.Meta
		s := &slot{content: content, meta: meta, done: make(chan string, 1), cred: ready.Cred}
		mu.Lock()
		slots[item.ID] = s
		mu.Unlock()
		push(content, meta)
		logger.Printf("item %s/%d pushed to the session", res.Goal.Slug, item.Seq)
		select {
		case outcome := <-s.done:
			logger.Printf("item %s/%d %s", res.Goal.Slug, item.Seq, outcome)
		case <-ctx.Done():
		}
	}

	// repush takes up every item this node holds on the platform: those
	// already in a slot are pushed again when the session restarted (it
	// lost them) or when it has gone quiet on one — idle, with no progress
	// reported and no push within the stall window — so an item whose
	// session gave up mid-way (a hung subagent, an interrupted turn) is
	// offered again instead of sitting in running for ever; those without
	// a slot (a restarted daemon) are adopted unless someone reported
	// progress on them within the last interval.
	stalled := func(s *slot, it gateway.WorkItem) bool {
		idle, _ := sess.Idle()
		return idle && !recentProgress(it.HeartbeatAt, nodeStallFlag) && time.Since(s.pushedAt) >= nodeStallFlag
	}
	// hung: the session is busy and nothing has been reported on the item
	// for --turn-timeout since it was pushed — a subagent's call that never
	// returns, a turn that never ends. Killing the session restarts it,
	// and OnStart re-pushes every held item.
	hung := func(s *slot, it gateway.WorkItem) bool {
		if nodeTurnTimeoutFlag <= 0 {
			return false
		}
		idle, _ := sess.Idle()
		return !idle && !recentProgress(it.HeartbeatAt, nodeTurnTimeoutFlag) && time.Since(s.pushedAt) >= nodeTurnTimeoutFlag
	}
	repush := func(ctx context.Context, all bool) {
		creds := map[string]*node.ItemCred{}
		mu.Lock()
		for id, s := range slots {
			if s.cred != nil {
				creds[id] = s.cred
			}
		}
		mu.Unlock()
		refreshItemCreds(ctx, creds, logger.Printf)
		var held []gateway.WorkItem
		for _, sc := range loop.CurrentScopes() {
			for _, status := range []string{"claimed", "running"} {
				items, err := gw.OrgWorkItems(ctx, sc.ID, status)
				if err != nil {
					logger.Printf("list held items of %s: %v", sc.Name(), err)
					return
				}
				for _, it := range items {
					if it.NodeID == sc.NodeID {
						held = append(held, it)
					}
				}
			}
		}
		for _, it := range held {
			mu.Lock()
			s := slots[it.ID]
			mu.Unlock()
			if s != nil {
				if all {
					push(s.content, s.meta)
					logger.Printf("item %s re-pushed to the session", it.ID)
				} else if stalled(s, it) {
					push(s.content, s.meta)
					logger.Printf("item %s re-pushed: the session is idle and nothing has been reported on it for %s", it.ID, nodeStallFlag)
				} else if hung(s, it) {
					sess.Kill(fmt.Sprintf("item %s: busy, nothing reported for %s", it.ID, nodeTurnTimeoutFlag))
					mu.Lock()
					s.pushedAt = time.Now() // the restart re-pushes; count the patience from there
					mu.Unlock()
				}
				continue
			}
			if !all && recentProgress(it.HeartbeatAt, loop.Interval) {
				continue
			}
			loop.Adopt(ctx, it)
		}
	}
	sess.OnStart = func() {
		time.Sleep(2 * time.Second) // let the channel server attach
		repush(ctx, true)
	}
	// a turn's cost lands on the item its event was for, as the cents
	// since the last turn: claude's total_cost_usd is the session's
	// running total (a smaller figure is a fresh process, taken whole).
	// The report is refused only on a pending item, so it lands however
	// far the item has moved by the time the turn ends.
	var costMu sync.Mutex
	lastCost := 0.0
	sess.OnTurn = func(itemID string, costUSD float64) {
		costMu.Lock()
		delta := costUSD - lastCost
		if delta < 0 {
			delta = costUSD
		}
		lastCost = costUSD
		costMu.Unlock()
		cents := node.Cents(delta)
		if itemID == "" || cents == 0 {
			return
		}
		sessMu.Lock()
		sid := sessionID
		sessMu.Unlock()
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := gw.ReportWorkItemProgress(rctx, itemID, "", cents, sid); err != nil {
			logger.Printf("item %s: report $%.2f: %v", itemID, delta, err)
			return
		}
		logger.Printf("item %s: $%.2f reported", itemID, delta)
	}
	loop.AfterTick = func(ctx context.Context) { repush(ctx, false) }

	logger.Printf("channel mode: node %s (%s) for %s, capacity %s, every %s; session in %s", cfg.Name, cfg.Slug, strings.Join(scopes.Names(), ", "), cfg.Capacity, loop.Interval, sess.WorkDir)
	for _, sc := range scopes {
		go func() {
			defer crash.Guard()
			channel.WatchGoals(ctx, gw, sc.ID, loop.Interval, func(kind string, g gateway.Goal) {
				content, meta := channel.GoalEvent(kind, g)
				push(content, meta)
			})
		}()
	}
	go func() {
		defer crash.Guard()
		_ = sess.Run(ctx, workRoot)
	}()
	err = loop.Run(ctx)
	if errors.Is(err, context.Canceled) {
		logger.Printf("stopped; claimed items were left for a retry")
		return nil
	}
	return endRun(err, logger)
}

// meshContextMCP is the mesh-context MCP server a session is started
// with: the credential's bearer as a header, taken (and refreshed) at
// that moment, so the session has a whole token lifetime ahead of it.
// Nil when there is no usable bearer — the session then runs without
// mesh-context.
func meshContextMCP(ctx context.Context, gw *gateway.Client) map[string]any {
	tok, err := gw.Bearer(ctx)
	if err != nil {
		return nil
	}
	return node.MeshContextMCP(gw.BaseURL, tok)
}

// sessionSandbox is the pod every one-shot session runs in (OpenSandbox);
// nil runs it on the host, as `sandbox: off` does.
var sessionSandbox *sandbox.Sandbox

// oneShotHarness is `claude -p` for a session that ends with its turn:
// poll-mode builds and every evaluation.
func oneShotHarness() *harness.ClaudeCode {
	h := &harness.ClaudeCode{Stderr: sessionsOut, Sandbox: sessionSandbox}
	if nodeUnattended {
		h.ExtraArgs = []string{"--dangerously-skip-permissions"}
	}
	return h
}

// secondOpinion is the evaluator's Codex harness and how its accounts'
// limits are kept; the zero value is no Codex at all.
type secondOpinion struct {
	h harness.Harness
	// preflight is the Codex harness's: the built-in context server
	// checked before any Codex session starts
	preflight  func(context.Context, harness.Job) error
	limits     *node.Limits
	thresholds func() node.LimitsConfig
	// accounts are the Codex accounts an evaluation may run on first
	// (Loop.CodexCandidates)
	accounts func() []node.AccountConfig
	// waits says a Codex-pinned item's wait once per item
	waits *node.OncePer
	// blind says once per item that an evaluation saw no context and
	// could not run on the other family — one per node, Codex or not,
	// since every evaluator is built afresh per item
	blind *node.OncePer
	// noVerdict says once per run that the platform takes no no-verdict
	// outcome
	noVerdict *node.OncePer
	// left holds what an evaluation that recorded no verdict left
	// running, for the item's next verdict's notes
	left *node.LeftRunning
}

// codexSecondOpinion builds the evaluator's Codex harness whenever its
// binary is found, with or without a `codex:` block. It has no account
// of its own: an issued OpenAI credential serving codex supplies every
// session's login, and the operator's own Codex login is never one (ADR
// 011's amendment 2026-10-02). An evaluation runs on Codex when an
// issued Codex account has headroom for the item's priority, else on
// Claude, and the second opinion runs on the other family
// (docs/adr/adr-011-evaluations-run-on-the-family-with-headroom.md).
// Its readings land in the loop's Limits under codex:<label>. No
// binary, or no issued Codex account: the node evaluates as before. Every session's whole event stream and stderr
// are kept under workRoot/kept-sessions for harness.KeepSessionOutput
// (docs/adr/adr-005-codex-second-opinion.md, "Amendment, 2026-09-28:
// the stream is kept 24 hours").
func codexSecondOpinion(cfg *node.Config, loop *node.Loop, workRoot string, logger *log.Logger) secondOpinion {
	bin := cfg.Codex.Binary
	if bin == "" {
		bin = "codex"
	}
	if _, err := exec.LookPath(bin); err != nil {
		if !cfg.Codex.IsZero() {
			logger.Printf("codex: no Codex evaluations: %v", err)
		}
		return secondOpinion{blind: &node.OncePer{}, noVerdict: &node.OncePer{}, left: &node.LeftRunning{}}
	}
	c := &harness.Codex{Binary: bin, Stderr: sessionsOut, WorkRoot: workRoot}
	logger.Printf("codex: evaluations on Codex when an issued Codex credential has headroom, else on Claude; the second opinion on the other family")
	h := node.ObserveLimits(c, loop.Limits)
	loop.CodexRoom = (&node.Evaluator{Codex: h, CodexAccounts: loop.CodexCandidates, Limits: loop.Limits, Thresholds: loop.Thresholds}).CodexHeadroom
	return secondOpinion{h: h, preflight: c.Preflight, limits: loop.Limits, thresholds: loop.Thresholds, accounts: loop.CodexCandidates, waits: &node.OncePer{}, blind: &node.OncePer{}, noVerdict: &node.OncePer{}, left: &node.LeftRunning{}}
}

// evaluateWith is the loop's Evaluate hook: a fresh evaluator session per
// finished item, in either mode — and a bearer taken afresh for each, so
// a session never starts with a machine token about to expire. A failure to reach a verdict is logged
// and the item is left in evaluating for the next tick.
func evaluateWith(gw *gateway.Client, cfg *node.Config, h harness.Harness, second secondOpinion, loop *node.Loop, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) func(context.Context, gateway.WorkItem) {
	return func(ctx context.Context, item gateway.WorkItem) {
		ev := evaluator(ctx, gw, cfg, h, second, loop, workRoot, fallback, accounts, logger)
		if err := ev.Evaluate(ctx, item); err != nil && ctx.Err() == nil && !saidByEvaluator(err) {
			logger.Printf("item %d %q: evaluation: %v", item.Seq, item.Title, err)
		}
	}
}

// saidByEvaluator is an evaluation error the evaluator has already said
// once for the item — a Codex-pinned item waiting for headroom, or one
// whose evaluation saw no context and has no other family — and the
// loop does not log again every tick.
func saidByEvaluator(err error) bool {
	return errors.Is(err, node.ErrFamilyWaits) || errors.Is(err, node.ErrNoContext) || saidOnce(err)
}

// saidOnce is an error node.log has said already, once for its goal: a
// claim-time read that issued no credential for the session's adapter
// (node.NoCredentials.Say), or a plan declined for a required tool
// (node.ToolsDeclined, which the planner logs), or a plan released for a
// refusal repeated (node.PlanRefused.Released, which the planner logs).
func saidOnce(err error) bool {
	_, ok := node.NoCredentialOf(err)
	if pr, released := node.PlanRefusedOf(err); released && pr.Released {
		return true // the planner logged the release
	}
	return ok || node.ToolsDeclined(err)
}

// rebaseWith is the loop's Rebase hook: a fresh session of kind rebase
// per item whose rebase this node has claimed (runsheet ADR 008), in
// either mode. However it ends, the rebase is finished or failed on the
// platform, so a failed one hands the item to its builder at once
// rather than holding the claim until the platform's stall timer.
func rebaseWith(gw *gateway.Client, cfg *node.Config, h harness.Harness, loop *node.Loop, scopes func() node.Scopes, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) func(context.Context, gateway.WorkItem) {
	return func(ctx context.Context, item gateway.WorkItem) {
		rb := rebaser(ctx, gw, cfg, h, loop, scopes, workRoot, fallback, accounts, logger)
		if err := rb.Rebase(ctx, item); err != nil && ctx.Err() == nil && !saidOnce(err) {
			logger.Printf("item %d %q: rebase: %v", item.Seq, item.Title, err)
		}
	}
}

// planWith is the loop's Plan hook: a fresh planner session per goal in
// planning, in either mode, with a bearer taken afresh for each. A plan that cannot be handed in is logged
// and the goal left in planning for the next tick (three tries).
func planWith(gw *gateway.Client, cfg *node.Config, h harness.Harness, loop *node.Loop, workRoot string, accounts *node.Credentials, logger *log.Logger) func(context.Context, gateway.Goal, node.Scope) {
	return func(ctx context.Context, goal gateway.Goal, scope node.Scope) {
		pl := planner(ctx, gw, cfg, h, loop, workRoot, accounts, logger)
		if err := pl.Plan(ctx, goal, scope); err != nil && ctx.Err() == nil && !saidOnce(err) {
			logger.Printf("goal %s: planning: %v", goal.Slug, err)
		}
	}
}

// researchWith is the loop's Research hook: a fresh director session
// per direction whose research this node has claimed (runsheet trd
// §6c). A session that cannot be read hands in an empty research rather
// than leaving the direction claimed, so the next window is not lost.
func researchWith(gw *gateway.Client, cfg *node.Config, h harness.Harness, loop *node.Loop, workRoot string, accounts *node.Credentials, logger *log.Logger) func(context.Context, gateway.Direction, gateway.Project, node.Scope) {
	return func(ctx context.Context, direction gateway.Direction, project gateway.Project, scope node.Scope) {
		dir := director(ctx, gw, cfg, h, loop, workRoot, accounts, logger)
		if err := dir.Research(ctx, direction, project, scope); err != nil && ctx.Err() == nil && !saidOnce(err) {
			logger.Printf("%s/%s: research: %v", project.Slug, direction.Slug, err)
		}
	}
}

func evaluator(ctx context.Context, gw *gateway.Client, cfg *node.Config, h harness.Harness, second secondOpinion, loop *node.Loop, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) *node.Evaluator {
	return &node.Evaluator{GW: gw, Harness: h, WS: &node.Git{Log: sessionsOut}, Cfg: cfg, Scopes: loop.CurrentScopes(), WorkRoot: workRoot, MCP: meshContextMCP(ctx, gw), MaxBudgetUSD: nodeEvalBudgetFlag, Budgets: loop.KindModelBudget, ContextBudget: loop.ContextBudget, Timeout: nodeSessionTimeoutFlag, Model: nodeModels.For("evaluate"), Fallback: fallback, Credentials: accounts, Runtimes: loop.ToolRuntimes, Declines: loop.Declines, Sandbox: sessionSandbox, Log: logger,
		Codex: second.h, CodexPreflight: second.preflight, CodexModel: cfg.Codex.CodexModel, CodexAccounts: second.accounts, Waits: second.waits, Blind: second.blind, NoVerdict: second.noVerdict, Splits: &node.OncePer{}, LeftRunning: second.left, Limits: second.limits, Thresholds: second.thresholds}
}

// validationNotices are the node's once-per-run validation notices,
// shared by its Runner and every Rebaser.
var validationNotices = &node.ValidationNotices{}

func rebaser(ctx context.Context, gw *gateway.Client, cfg *node.Config, h harness.Harness, loop *node.Loop, scopes func() node.Scopes, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) *node.Rebaser {
	return &node.Rebaser{GW: gw, Harness: h, WS: &node.Git{Log: sessionsOut}, Cfg: cfg, Scopes: scopes(), WorkRoot: workRoot, MCP: meshContextMCP(ctx, gw), MaxBudgetUSD: nodeRebaseBudgetFlag, Budgets: loop.KindModelBudget, ContextBudget: loop.ContextBudget, Timeout: nodeSessionTimeoutFlag, Model: nodeModels.For("rebase"), Fallback: fallback, Credentials: accounts, Sandbox: sessionSandbox, Notices: validationNotices, Log: logger}
}

func planner(ctx context.Context, gw *gateway.Client, cfg *node.Config, h harness.Harness, loop *node.Loop, workRoot string, accounts *node.Credentials, logger *log.Logger) *node.Planner {
	return &node.Planner{GW: gw, Harness: h, WS: &node.Git{Log: sessionsOut}, Cfg: cfg, WorkRoot: workRoot, MCP: meshContextMCP(ctx, gw), MaxBudgetUSD: nodePlanBudgetFlag, Budgets: loop.KindModelBudget, ContextBudget: loop.ContextBudget, Timeout: nodeSessionTimeoutFlag, Model: nodeModels.PlanModel(), Credentials: accounts, Runtimes: loop.ToolRuntimes, Declines: loop.Declines, Log: logger}
}

func director(ctx context.Context, gw *gateway.Client, cfg *node.Config, h harness.Harness, loop *node.Loop, workRoot string, accounts *node.Credentials, logger *log.Logger) *node.Director {
	return &node.Director{GW: gw, Harness: h, WS: &node.Git{Log: sessionsOut}, Cfg: cfg, WorkRoot: workRoot, MCP: meshContextMCP(ctx, gw), MaxBudgetUSD: nodeResearchBudgetFlag, Budgets: loop.KindModelBudget, ContextBudget: loop.ContextBudget, Timeout: nodeSessionTimeoutFlag, Model: nodeModels.For("research"), Credentials: accounts, Runtimes: loop.ToolRuntimes, Declines: loop.Declines, Log: logger}
}

// adoptSessions takes up, before the first claim, every session under the
// work root whose state file names one of this node's ids (mesh-cli ADR
// 009): each kind by the session that would have run it, the item's
// heartbeat sent at once. The held items Adopt then runs from the start
// are only those with no session left to take up.
func adoptSessions(ctx context.Context, gw *gateway.Client, cfg *node.Config, h harness.Harness, second secondOpinion, loop *node.Loop, runner *node.Runner, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) []node.FoundSession {
	ids := make([]string, 0, len(loop.CurrentScopes()))
	for _, sc := range loop.CurrentScopes() {
		ids = append(ids, sc.NodeID)
	}
	// an owned node takes up, too, the sessions it started under its
	// per-org ids before it was owned (ADR-015)
	var legacy map[string]string
	if loop.Owned != nil {
		legacy = node.LegacyIDs(cfg, loop.CurrentScopes())
		for id := range legacy {
			ids = append(ids, id)
		}
	}
	found, err := node.FindSessions(workRoot, ids)
	if err != nil {
		logger.Printf("adopt: %v", err)
	}
	// The start-up sweep (ADR 008): a scope no found session is about to
	// adopt, whose subject is terminal on the platform, is a leak.
	if _, ok := hostUnit().(sessionunit.SystemdScope); ok {
		if n := node.SweepScopes(ctx, gw, sessionunit.HostSystemctl{}, found, logger.Printf); n > 0 {
			logger.Printf("sweep: %d leaked scope(s) stopped", n)
		}
	}
	if len(found) == 0 {
		return nil
	}
	loop.AdoptSession = node.AdoptWith{GW: gw, Scopes: loop.CurrentScopes, Logf: logger.Printf, Legacy: legacy,
		Build: func(ctx context.Context, item gateway.WorkItem, s node.FoundSession) error {
			r := *runner
			r.Scopes = loop.CurrentScopes()
			return r.AdoptItem(ctx, item, s)
		},
		Evaluate: func(ctx context.Context, item gateway.WorkItem, s node.FoundSession) error {
			return evaluator(ctx, gw, cfg, h, second, loop, workRoot, fallback, accounts, logger).AdoptEvaluation(ctx, item, s)
		},
		Rebase: func(ctx context.Context, item gateway.WorkItem, s node.FoundSession) error {
			return rebaser(ctx, gw, cfg, h, loop, loop.CurrentScopes, workRoot, fallback, accounts, logger).AdoptRebase(ctx, item, s)
		},
		Plan: func(ctx context.Context, goal gateway.Goal, sc node.Scope, s node.FoundSession) error {
			return planner(ctx, gw, cfg, h, loop, workRoot, accounts, logger).AdoptPlan(ctx, goal, sc, s)
		},
		Research: func(ctx context.Context, d gateway.Direction, p gateway.Project, sc node.Scope, s node.FoundSession) error {
			return director(ctx, gw, cfg, h, loop, workRoot, accounts, logger).AdoptResearch(ctx, d, p, sc, s)
		},
	}.Adopt
	logger.Printf("adopt: %d session(s) of the process before this one taken up", loop.AdoptSessions(ctx, found))
	return found
}

// reopenClaims re-opens, after adoptSessions and once the loop's Plan and
// Research are set, every plan or research claim the process before this
// one recorded and never opened a session for (mesh-cli ADR 009, Claims):
// one whose session was found is adoptSessions'.
func reopenClaims(ctx context.Context, loop *node.Loop, workRoot string, found []node.FoundSession, logger *log.Logger) {
	ids := make([]string, 0, len(loop.CurrentScopes()))
	for _, sc := range loop.CurrentScopes() {
		ids = append(ids, sc.NodeID)
	}
	claims, err := node.FindClaims(workRoot, ids)
	if err != nil {
		logger.Printf("claims: %v", err)
	}
	if len(claims) == 0 {
		return
	}
	logger.Printf("claims: %d unopened claim(s) of the process before this one re-opened", loop.ReopenClaims(ctx, claims, found))
}

// recentProgress reports whether an item's last heartbeat is within the
// interval — someone is working it.
func recentProgress(heartbeatAt string, interval time.Duration) bool {
	if heartbeatAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, heartbeatAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, heartbeatAt)
	}
	if err != nil {
		return false
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return time.Since(t) < interval
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 200 {
		s = s[:199] + "…"
	}
	return s
}

func init() {
	for _, c := range []*cobra.Command{nodeLoginCmd, nodeRegisterCmd, nodeStatusCmd, nodeStartCmd} {
		c.Flags().StringVar(&gatewayFlag, "gateway", "", "platform gateway URL (default "+gateway.DefaultURL+", or MESH_GATEWAY)")
	}
	nodeLoginCmd.Flags().StringVar(&latchkeyFlag, "latchkey", "", "Latchkey issuer (default "+latchkey.DefaultIssuer+", or MESH_LATCHKEY)")
	nodeLoginCmd.Flags().StringVar(&latchkeyClientFlag, "client-id", "", "the public Latchkey client the device flow starts from (or MESH_LATCHKEY_CLIENT_ID)")
	nodeLoginCmd.Flags().StringVar(&machineNameFlag, "name", "", "what to call this machine in the Latchkey console (default: the node's name, else the hostname)")
	nodeLoginCmd.Flags().BoolVar(&noBrowserFlag, "no-browser", false, "print the sign-in URL instead of opening it")
	nodeRegisterCmd.Flags().StringSliceVar(&nodeOrgsFlag, "org", nil, "serve only these orgs (repeatable; default: every org you belong to)")
	nodeRegisterCmd.Flags().StringVar(&nodeNameFlag, "name", "", "the node's name as the console shows it")
	nodeRegisterCmd.Flags().StringVar(&nodeSlugFlag, "slug", "", "the node's slug (default: from --name); the node id derives from it")
	nodeRegisterCmd.Flags().IntVar(&nodeCapacityFlag, "capacity", 1, "concurrent sessions")
	nodeRegisterCmd.Flags().StringVar(&nodeOwnerFlag, "owner", "", `the node's owner: "person" (default, bring your own infra) or "enterprise" (shared infra; requires --enterprise)`)
	nodeRegisterCmd.Flags().StringVar(&nodeEnterpriseFlag, "enterprise", "", "the enterprise that owns this node, by slug or id (required with --owner enterprise)")
	nodeStartCmd.Flags().DurationVar(&nodeIntervalFlag, "interval", 30*time.Second, "heartbeat and claim interval")
	nodeStartCmd.Flags().DurationVar(&nodeStallFlag, "stall", 10*time.Minute, "channel mode: re-push a held item the idle session has reported nothing on for this long")
	nodeStartCmd.Flags().DurationVar(&nodeTurnTimeoutFlag, "turn-timeout", 45*time.Minute, "channel mode: kill and restart a busy session that has reported nothing on its item for this long (0 never)")
	nodeStartCmd.Flags().DurationVar(&nodeSessionTimeoutFlag, "session-timeout", 40*time.Minute, "cap on one evaluator, planner, director or rebase session's wall clock (0 never)")
	nodeStartCmd.Flags().Float64Var(&nodeMaxBudgetFlag, "max-budget-usd", 10, "cap on one session's budget (poll mode), whatever the goal has left")
	nodeStartCmd.Flags().Float64Var(&nodeEvalBudgetFlag, "eval-budget-usd", 3, "cap on one evaluator session's budget")
	nodeStartCmd.Flags().Float64Var(&nodePlanBudgetFlag, "plan-budget-usd", 5, "cap on one planner session's budget")
	nodeStartCmd.Flags().Float64Var(&nodeResearchBudgetFlag, "research-budget-usd", 5, "cap on one director (research) session's budget")
	nodeStartCmd.Flags().Float64Var(&nodeRebaseBudgetFlag, "rebase-budget-usd", 5, "cap on one rebase session's budget")
	nodeStartCmd.Flags().StringVar(&nodeModelFlag, "model", "", "model every session runs on when its kind names none (an alias the CLI knows — opus, sonnet, haiku — or a full id); empty is the CLI's default")
	nodeStartCmd.Flags().StringVar(&nodeModelBuildFlag, "model-build", "", "model for build sessions (overrides --model)")
	nodeStartCmd.Flags().StringVar(&nodeModelEvaluateFlag, "model-evaluate", "", "model for evaluator sessions (overrides --model)")
	nodeStartCmd.Flags().StringVar(&nodeModelPlanFlag, "model-plan", "", "model for planner sessions (default: the strongest configured, else opus)")
	nodeStartCmd.Flags().StringVar(&nodeModelChannelFlag, "model-channel", "", "model for the channel-mode session (overrides --model)")
	nodeStartCmd.Flags().StringVar(&nodeModelResearchFlag, "model-research", "", "model for director (research) sessions (overrides --model)")
	nodeStartCmd.Flags().StringVar(&nodeModelRebaseFlag, "model-rebase", "", "model for rebase sessions (overrides --model)")
	nodeStartCmd.Flags().StringVar(&nodeModeFlag, "mode", "channel", "channel (one persistent session fed by the mesh channel) or poll (claude -p per item)")
	nodeStartCmd.Flags().BoolVar(&nodeUnattended, "unattended", false, "run sessions with --dangerously-skip-permissions (trusted machines only)")
	nodeStartCmd.Flags().BoolVar(&nodeOnceFlag, "once", false, "poll mode: one tick, then exit")
	nodeModelCmd.Flags().StringVar(&nodeModelFlag, "default", "", "model for any kind that names none")
	nodeModelCmd.Flags().StringVar(&nodeModelBuildFlag, "build", "", "model for build sessions")
	nodeModelCmd.Flags().StringVar(&nodeModelEvaluateFlag, "evaluate", "", "model for evaluator sessions")
	nodeModelCmd.Flags().StringVar(&nodeModelPlanFlag, "plan", "", "model for planner sessions")
	nodeModelCmd.Flags().StringVar(&nodeModelChannelFlag, "channel", "", "model for the channel-mode session")
	nodeModelCmd.Flags().StringVar(&nodeModelResearchFlag, "research", "", "model for director (research) sessions")
	nodeModelCmd.Flags().StringVar(&nodeModelRebaseFlag, "rebase", "", "model for rebase sessions")
	nodeCmd.AddCommand(nodeLoginCmd, nodeRegisterCmd, nodeStartCmd, nodeStatusCmd, nodeDrainCmd, nodeUpgradeCmd, nodeLogoutCmd, nodeModelCmd)
}

// modelPrecedenceNote follows the table of `mesh node model`: the rows
// are node.yaml's, and an item's own model comes before them.
const modelPrecedenceNote = `
build and rebase sessions run on the item's model, then its agent's, then
node.yaml's build (or default). The evaluator runs on the stronger of the
builder's model and sonnet. The planner runs on plan if set, else the
strongest model configured (opus when none is).
`

// nodeModelCmd reads or writes the models in ~/.mesh/node.yaml, so a
// node can be moved off a model without editing YAML or passing flags.
// Drain the node (`mesh node drain`) for a change to take.
var nodeModelCmd = &cobra.Command{
	Use:   "model",
	Short: "Show or set the model each kind of session runs on",
	Long: `With no flags, prints the models this node uses. A flag sets one and
saves it: an alias the CLI knows (opus, sonnet, haiku) or a full id
(claude-opus-5). "" clears it back to the CLI's default.

  mesh node model                                  # show
  mesh node model --default opus                   # everything on Opus
  mesh node model --plan sonnet --evaluate sonnet  # cheaper planning and review

A flag on ` + "`mesh node start`" + ` wins over these for that run.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := node.LoadConfig()
		if err != nil {
			return err
		}
		changed := false
		for _, o := range []struct {
			flag string
			val  string
			set  *string
		}{
			{"default", nodeModelFlag, &cfg.Models.Default},
			{"build", nodeModelBuildFlag, &cfg.Models.Build},
			{"evaluate", nodeModelEvaluateFlag, &cfg.Models.Evaluate},
			{"plan", nodeModelPlanFlag, &cfg.Models.Plan},
			{"research", nodeModelResearchFlag, &cfg.Models.Research},
			{"rebase", nodeModelRebaseFlag, &cfg.Models.Rebase},
			{"channel", nodeModelChannelFlag, &cfg.Models.Channel},
		} {
			if cmd.Flags().Changed(o.flag) {
				*o.set = strings.TrimSpace(o.val)
				changed = true
			}
		}
		if changed {
			if err := cfg.Save(); err != nil {
				return err
			}
		}
		shown := func(v string) string {
			if v == "" {
				return "(the CLI's default)"
			}
			return v
		}
		fmt.Printf("default   %s\n", shown(cfg.Models.Default))
		for _, k := range []struct{ name, val string }{
			{"build", cfg.Models.Build}, {"evaluate", cfg.Models.Evaluate},
			{"plan", cfg.Models.Plan}, {"channel", cfg.Models.Channel},
			{"research", cfg.Models.Research}, {"rebase", cfg.Models.Rebase},
		} {
			switch {
			case k.val != "":
				fmt.Printf("%-9s %s\n", k.name, k.val)
			case k.name == "plan":
				// the planner runs on the strongest configured, not default
				fmt.Printf("%-9s %s (the strongest configured)\n", k.name, cfg.Models.PlanModel())
			default:
				fmt.Printf("%-9s %s\n", k.name, shown(cfg.Models.Default))
			}
		}
		fmt.Print(modelPrecedenceNote)
		if changed {
			fmt.Println("\nsaved; drain the node for it to take (mesh node drain): it finishes its sessions and its service starts it again")
		}
		return nil
	},
}
