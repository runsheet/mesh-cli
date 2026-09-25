package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/channel"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/latchkey"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/selfupdate"
	"projectmesh.io/mesh-cli/internal/version"
)

var (
	nodeOrgsFlag           []string
	nodeNameFlag           string
	nodeSlugFlag           string
	nodeCapacityFlag       int
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
		logins = append(logins, o.Login)
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
		if prev, err := node.LoadSettings(); err == nil {
			cfg.Latchkey, cfg.LatchkeyClient = prev.Latchkey, prev.LatchkeyClient
		}
		scopes, err := node.ResolveScopes(ctx, gw, cfg)
		if err != nil {
			return err
		}
		if err := registerIn(ctx, gw, cfg, scopes); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		path, _ := node.ConfigPath()
		fmt.Printf("Registered node %s (%s) in %s, capacity %s. Saved %s.\nStart it with: mesh node start\n", cfg.Name, slug, strings.Join(scopes.Logins(), ", "), cfg.Capacity, path)
		return nil
	},
}

// registerIn registers the node in every scope (a Node per org, id from
// (org, slug); registering again is a heartbeat).
func registerIn(ctx context.Context, gw *gateway.Client, cfg *node.Config, scopes node.Scopes) error {
	for _, sc := range scopes {
		if err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: sc.NodeID, OrgID: sc.ID, OwnerID: cfg.OwnerID, Name: cfg.Name, Slug: cfg.Slug,
			Harnesses: []string{"claude-code"}, Capacity: cfg.Capacity.Max, Version: version.Version}); err != nil {
			return fmt.Errorf("register in %s: %w", sc.Login, err)
		}
	}
	return nil
}

var nodeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Heartbeat once and list the org's nodes",
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
		scopes, err := node.ResolveScopes(ctx, gw, cfg)
		if err != nil {
			return err
		}
		fmt.Println(credentialLine(src.Credential()))
		state, err := node.DefaultRunState()
		if err != nil {
			return err
		}
		local, draining := localNodeState(state)
		fmt.Println(local)
		scaling, scales := localScaling(state)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ORG\tNAME\tSLUG\tSTATUS\tRUNNING\tCAPACITY\tVERSION\tLAST_SEEN")
		for _, sc := range scopes {
			if err := gw.HeartbeatNode(ctx, sc.NodeID, version.Version, 0); err != nil {
				return fmt.Errorf("heartbeat in %s: %w", sc.Login, err)
			}
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
				fmt.Fprintf(w, "%s\t%s%s\t%s\t%s\t%d\t%s\t%s\t%s\n", sc.Login, n.Name, mark, n.Slug, status, n.Running, capacity, n.Version, n.LastSeenAt)
			}
		}
		return w.Flush()
	},
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

// localScaling is the capacity the node on this machine has in force
// when node.yaml's capacity is a range, as it left it at its last tick,
// and true; false when it is fixed or no node runs here. The platform's
// node record carries the registered capacity only.
func localScaling(state node.RunState) (node.Scaling, bool) {
	pid, err := state.PID()
	if err != nil || !processAlive(pid) {
		return node.Scaling{}, false
	}
	return state.Scaling()
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
func onDrainSignal(drain func()) (stop func()) {
	if drainSignal == nil {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, drainSignal)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				drain()
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
		logger := log.New(os.Stderr, "mesh node: ", log.LstdFlags)
		// a new binary that has failed to start twice is rolled back
		// first, before anything else in it can fail again
		updates, exe, check := startUpdates(logger)
		if check.RolledBack {
			reportRollback(check, logger)
			return nil
		}
		cfg, err := node.LoadConfig()
		if err != nil {
			return err
		}
		nodeModels = modelsFor(cmd, cfg)
		// an account whose secret cannot be read would fail every
		// session it ran: refuse to start on it (adr-005)
		for _, a := range cfg.Accounts {
			if _, err := a.Secret(); err != nil {
				return err
			}
		}
		gw, err := platformClient()
		if err != nil {
			return err
		}
		workRoot, err := node.WorkRoot()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(workRoot, 0o755); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		regCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if cfg.OwnerID == "" {
			cfg.OwnerID, err = gw.SignIn(regCtx)
			if err != nil {
				cancel()
				return err
			}
		}
		scopes, err := node.ResolveScopes(regCtx, gw, cfg)
		if err == nil {
			err = registerIn(regCtx, gw, cfg, scopes)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("register node: %w", err)
		}

		// every session's plan standing lands in Limits: the heartbeat
		// carries it and the loop paces by it
		loop := &node.Loop{GW: gw, Cfg: cfg, Scopes: scopes, Version: version.Version, Harness: "claude-code", Interval: nodeIntervalFlag, Log: logger, Limits: &node.Limits{},
			// a ranged capacity scales by the plan's windows, capped by this
			// machine's load and memory (adr-004)
			Machine: node.HostMachine(),
			// every tick reports the tool runtimes and agent CLIs on PATH
			LookPath: exec.LookPath}
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
		// the node follows the version the platform asks for (adr-002);
		// --once is one tick, too short for an update to finish
		if updates != nil && !nodeOnceFlag {
			loop.Update = &node.ReleaseUpdater{Source: selfupdate.GitHub{}, State: updates, Exe: exe}
			if check.Failed != "" {
				loop.NoteUpdateFailure(check.Failed, check.Reason, check.Final)
			}
		}
		// capacity and the orgs are read again every tick, so a changed
		// node.yaml or a newly claimed org needs no restart
		loop.Reload = node.Reloader(gw, cfg, scopes, nil, func(ctx context.Context, sc node.Scope) error {
			return registerIn(ctx, gw, cfg, node.Scopes{sc})
		})
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
		defer onDrainSignal(func() {
			if err := state.MarkDraining(); err != nil {
				logger.Printf("draining marker: %v", err)
			}
			loop.Drain()
		})()
		// one old-platform notice for the whole run, whatever runs the items
		fallback := &node.TokenFallback{}
		// every one-shot session's account comes from the platform's
		// claim-time read, node.yaml the fallback, and every one reports
		// its usage record (adr-005); each fallback is said once per run
		accounts := &node.Credentials{GW: gw, Limits: loop.Limits, Thresholds: loop.Thresholds, Log: logger}
		switch nodeModeFlag {
		case "poll":
			return runPollMode(ctx, cfg, scopes, gw, loop, workRoot, fallback, accounts, logger)
		case "channel", "":
			return runChannelMode(ctx, cfg, scopes, gw, loop, workRoot, fallback, accounts, logger)
		default:
			return fmt.Errorf("--mode must be channel or poll")
		}
	},
}

// observed is a one-shot harness whose sessions report their plan
// standing into the loop's Limits and their cost into its Ledger.
func observed(loop *node.Loop, logger *log.Logger) harness.Harness {
	return node.ObserveSpend(node.ObserveLimits(oneShotHarness(), loop.Limits), loop.Ledger, logger.Printf)
}

func runPollMode(ctx context.Context, cfg *node.Config, scopes node.Scopes, gw *gateway.Client, loop *node.Loop, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) error {
	h := observed(loop, logger)
	runner := &node.Runner{GW: gw, Harness: h, WS: &node.Git{Log: os.Stderr}, Cfg: cfg, Scopes: scopes, WorkRoot: workRoot, MCPFunc: func(ctx context.Context) map[string]any { return prologueMCP(ctx, gw) }, MaxBudgetUSD: nodeMaxBudgetFlag, Model: nodeModels.For("build"), Fallback: fallback, Accounts: cfg.Accounts, Credentials: accounts, Log: logger}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		r := *runner
		r.Scopes = loop.CurrentScopes() // an org served since start is known to its build
		_ = r.RunItem(ctx, item)
	}
	loop.Evaluate = evaluateWith(gw, cfg, h, codexSecondOpinion(cfg, loop, logger), loop.CurrentScopes, workRoot, fallback, accounts, logger)
	loop.Plan = planWith(gw, cfg, h, workRoot, accounts, logger)
	loop.Research = researchWith(gw, cfg, h, workRoot, accounts, logger)
	loop.Rebase = rebaseWith(gw, cfg, h, loop.CurrentScopes, workRoot, fallback, accounts, logger)
	logger.Printf("poll mode: node %s (%s) for %s, capacity %s, every %s", cfg.Name, cfg.Slug, strings.Join(scopes.Logins(), ", "), cfg.Capacity, loop.Interval)
	// items this node held when it last stopped are taken up again: a
	// restart mid-build continues them (the workspace keeps the branch)
	for _, sc := range scopes {
		for _, status := range []string{"claimed", "running"} {
			items, err := gw.OrgWorkItems(ctx, sc.ID, status)
			if err != nil {
				logger.Printf("list held items of %s: %v", sc.Login, err)
				continue
			}
			for _, it := range items {
				if it.NodeID == sc.NodeID {
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

// startUpdates counts this start against a pending self-update and rolls
// the binary back when it is the third unconfirmed one. A state file or
// executable it cannot read leaves the node running without updates.
func startUpdates(logger *log.Logger) (*selfupdate.StateFile, string, node.StartCheck) {
	f, err := selfupdate.DefaultStateFile()
	if err == nil {
		var exe string
		if exe, err = node.Executable(); err == nil {
			var check node.StartCheck
			if check, err = node.CheckStart(f, exe); err == nil {
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
	scopes, err := node.ResolveScopes(ctx, gw, cfg)
	if err != nil {
		logger.Printf("self-update: report rollback: %v", err)
		return
	}
	for _, sc := range scopes {
		if err := gw.HeartbeatNode(ctx, sc.NodeID, check.From, 0, gateway.HeartbeatExtras{UpdateError: check.Reason}); err != nil {
			logger.Printf("self-update: report rollback to %s: %v", sc.Login, err)
		}
	}
}

// endRun is how the node's process ends when Run returns: 0 on a drain
// and on a self-update's restart, which the service manager answers by
// starting the binary now in place.
func endRun(err error, logger *log.Logger) error {
	switch {
	case err == nil:
		logger.Printf("drained; exiting")
	case errors.Is(err, node.ErrRestartForUpdate):
		logger.Printf("updated; exiting for the service manager to start the new binary")
		return nil
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

	sess := &channel.Session{Cfg: cfg, Orgs: scopes.Logins(), MCPFunc: func() map[string]any { return prologueMCP(ctx, gw) }, WorkDir: filepath.Join(workRoot, cfg.Slug), Exe: exe, SockPath: sock, Unattended: nodeUnattended, Model: nodeModels.For("channel"), Stderr: os.Stderr, Log: logger,
		OnText:      func(t string) { logger.Printf("session: %s", firstLine(t)) },
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

	ws := &node.Git{Log: os.Stderr}
	loop.Evaluate = evaluateWith(gw, cfg, observed(loop, logger), codexSecondOpinion(cfg, loop, logger), loop.CurrentScopes, workRoot, fallback, accounts, logger)
	loop.Plan = planWith(gw, cfg, observed(loop, logger), workRoot, accounts, logger)
	loop.Research = researchWith(gw, cfg, observed(loop, logger), workRoot, accounts, logger)
	loop.Rebase = rebaseWith(gw, cfg, observed(loop, logger), loop.CurrentScopes, workRoot, fallback, accounts, logger)
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		ready, err := readyChannelItem(ctx, gw, ws, loop.CurrentScopes(), item, workRoot, fallback, logger.Printf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if ce := (*node.CredentialError)(nil); errors.As(err, &ce) {
				_ = node.ReleaseOnCredential(ctx, gw, item.ID, ce, logger.Printf)
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
					logger.Printf("list held items of %s: %v", sc.Login, err)
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

	logger.Printf("channel mode: node %s (%s) for %s, capacity %s, every %s; session in %s", cfg.Name, cfg.Slug, strings.Join(scopes.Logins(), ", "), cfg.Capacity, loop.Interval, sess.WorkDir)
	for _, sc := range scopes {
		go channel.WatchGoals(ctx, gw, sc.ID, loop.Interval, func(kind string, g gateway.Goal) {
			content, meta := channel.GoalEvent(kind, g)
			push(content, meta)
		})
	}
	go func() { _ = sess.Run(ctx, workRoot) }()
	err = loop.Run(ctx)
	if errors.Is(err, context.Canceled) {
		logger.Printf("stopped; claimed items were left for a retry")
		return nil
	}
	return endRun(err, logger)
}

// prologueMCP is the Prologue MCP server a session is started with: the
// credential's bearer as a header, taken (and refreshed) at that moment,
// so the session has a whole token lifetime ahead of it. Nil when there
// is no usable bearer — the session then runs without Prologue.
func prologueMCP(ctx context.Context, gw *gateway.Client) map[string]any {
	tok, err := gw.Bearer(ctx)
	if err != nil {
		return nil
	}
	return node.PrologueMCP(gw.BaseURL, tok)
}

// oneShotHarness is `claude -p` for a session that ends with its turn:
// poll-mode builds and every evaluation.
func oneShotHarness() *harness.ClaudeCode {
	h := &harness.ClaudeCode{Stderr: os.Stderr}
	if nodeUnattended {
		h.ExtraArgs = []string{"--dangerously-skip-permissions"}
	}
	return h
}

// secondOpinion is the evaluator's Codex harness and how its account's
// limits are kept; the zero value is no second opinion.
type secondOpinion struct {
	h          harness.Harness
	account    string
	limits     *node.Limits
	thresholds func() node.LimitsConfig
}

// codexSecondOpinion builds the Codex harness for the evaluator's second
// opinion (docs/adr/adr-005-codex-second-opinion.md): when node.yaml has
// a `codex:` block and its binary is found, on the block's first account
// (none: the operator's own Codex login). Its readings land in the
// loop's Limits under codex:<account>. Otherwise none, and the node
// evaluates as before.
func codexSecondOpinion(cfg *node.Config, loop *node.Loop, logger *log.Logger) secondOpinion {
	if cfg.Codex.IsZero() {
		return secondOpinion{}
	}
	bin := cfg.Codex.Binary
	if bin == "" {
		bin = "codex"
	}
	if _, err := exec.LookPath(bin); err != nil {
		logger.Printf("codex: no second opinion: %v", err)
		return secondOpinion{}
	}
	c := &harness.Codex{Binary: bin, Stderr: os.Stderr}
	if len(cfg.Codex.Accounts) > 0 {
		c.Account = cfg.Codex.Accounts[0].Harness()
	}
	account := node.CodexLimitsAccount(c.Account.Name)
	logger.Printf("codex: second opinion on urgent and high goals, account %s", account)
	return secondOpinion{h: node.ObserveLimitsAs(c, loop.Limits, account), account: account, limits: loop.Limits, thresholds: loop.Thresholds}
}

// evaluateWith is the loop's Evaluate hook: a fresh evaluator session per
// finished item, in either mode — and a bearer taken afresh for each, so
// a session never starts with a machine token about to expire. A failure to reach a verdict is logged
// and the item is left in evaluating for the next tick.
func evaluateWith(gw *gateway.Client, cfg *node.Config, h harness.Harness, second secondOpinion, scopes func() node.Scopes, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) func(context.Context, gateway.WorkItem) {
	return func(ctx context.Context, item gateway.WorkItem) {
		ev := &node.Evaluator{GW: gw, Harness: h, WS: &node.Git{Log: os.Stderr}, Scopes: scopes(), WorkRoot: workRoot, MCP: prologueMCP(ctx, gw), MaxBudgetUSD: nodeEvalBudgetFlag, Timeout: nodeSessionTimeoutFlag, Model: nodeModels.For("evaluate"), Fallback: fallback, Accounts: cfg.Accounts, Credentials: accounts, Log: logger,
			Second: second.h, SecondModel: cfg.Codex.CodexModel(nodeModels.For("evaluate")), SecondAccount: second.account, Limits: second.limits, Thresholds: second.thresholds}
		if err := ev.Evaluate(ctx, item); err != nil && ctx.Err() == nil {
			logger.Printf("item %d %q: evaluation: %v", item.Seq, item.Title, err)
		}
	}
}

// rebaseWith is the loop's Rebase hook: a fresh session of kind rebase
// per item whose rebase this node has claimed (runsheet ADR 008), in
// either mode. However it ends, the rebase is finished or failed on the
// platform, so a failed one hands the item to its builder at once
// rather than holding the claim until the platform's stall timer.
func rebaseWith(gw *gateway.Client, cfg *node.Config, h harness.Harness, scopes func() node.Scopes, workRoot string, fallback *node.TokenFallback, accounts *node.Credentials, logger *log.Logger) func(context.Context, gateway.WorkItem) {
	return func(ctx context.Context, item gateway.WorkItem) {
		rb := &node.Rebaser{GW: gw, Harness: h, WS: &node.Git{Log: os.Stderr}, Scopes: scopes(), WorkRoot: workRoot, MCP: prologueMCP(ctx, gw), MaxBudgetUSD: nodeRebaseBudgetFlag, Timeout: nodeSessionTimeoutFlag, Model: nodeModels.For("rebase"), Fallback: fallback, Accounts: cfg.Accounts, Credentials: accounts, Log: logger}
		if err := rb.Rebase(ctx, item); err != nil && ctx.Err() == nil {
			logger.Printf("item %d %q: rebase: %v", item.Seq, item.Title, err)
		}
	}
}

// planWith is the loop's Plan hook: a fresh planner session per goal in
// planning, in either mode, with a bearer taken afresh for each. A plan that cannot be handed in is logged
// and the goal left in planning for the next tick (three tries).
func planWith(gw *gateway.Client, cfg *node.Config, h harness.Harness, workRoot string, accounts *node.Credentials, logger *log.Logger) func(context.Context, gateway.Goal, node.Scope) {
	return func(ctx context.Context, goal gateway.Goal, scope node.Scope) {
		pl := &node.Planner{GW: gw, Harness: h, WS: &node.Git{Log: os.Stderr}, Cfg: cfg, WorkRoot: workRoot, MCP: prologueMCP(ctx, gw), MaxBudgetUSD: nodePlanBudgetFlag, Timeout: nodeSessionTimeoutFlag, Model: nodeModels.PlanModel(), Accounts: cfg.Accounts, Credentials: accounts, Log: logger}
		if err := pl.Plan(ctx, goal, scope); err != nil && ctx.Err() == nil {
			logger.Printf("goal %s: planning: %v", goal.Slug, err)
		}
	}
}

// researchWith is the loop's Research hook: a fresh director session
// per direction whose research this node has claimed (runsheet trd
// §6c). A session that cannot be read hands in an empty research rather
// than leaving the direction claimed, so the next window is not lost.
func researchWith(gw *gateway.Client, cfg *node.Config, h harness.Harness, workRoot string, accounts *node.Credentials, logger *log.Logger) func(context.Context, gateway.Direction, gateway.Project, node.Scope) {
	return func(ctx context.Context, direction gateway.Direction, project gateway.Project, scope node.Scope) {
		dir := &node.Director{GW: gw, Harness: h, WS: &node.Git{Log: os.Stderr}, Cfg: cfg, WorkRoot: workRoot, MCP: prologueMCP(ctx, gw), MaxBudgetUSD: nodeResearchBudgetFlag, Timeout: nodeSessionTimeoutFlag, Model: nodeModels.For("research"), Accounts: cfg.Accounts, Credentials: accounts, Log: logger}
		if err := dir.Research(ctx, direction, project, scope); err != nil && ctx.Err() == nil {
			logger.Printf("%s/%s: research: %v", project.Slug, direction.Slug, err)
		}
	}
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
	nodeCmd.AddCommand(nodeLoginCmd, nodeRegisterCmd, nodeStartCmd, nodeStatusCmd, nodeDrainCmd, nodeLogoutCmd, nodeModelCmd)
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
