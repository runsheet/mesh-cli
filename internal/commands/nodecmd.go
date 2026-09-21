package commands

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/channel"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/version"
)

var (
	nodeOrgFlag       string
	nodeNameFlag      string
	nodeSlugFlag      string
	nodeCapacityFlag  int
	nodeIntervalFlag  time.Duration
	nodeStallFlag     time.Duration
	nodeMaxBudgetFlag float64
	nodeModeFlag      string
	nodeUnattended    bool
	nodeOnceFlag      bool
)

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Run this machine as a Mesh node on the runsheet platform",
	Long: `A node runs work items of goals: it heartbeats, claims ready items for
its org and runs each in a fresh claude session, as the person whose
console token it holds (~/.mesh/token).`,
}

var nodeLoginCmd = &cobra.Command{
	Use:   "login <rs_token>",
	Short: "Save a runsheet console token (Settings → Tokens) to ~/.mesh/token",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return saveToken(args[0]) },
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
	return nil
}

var nodeRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register this machine as a node of an org (converging on its slug)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(nodeOrgFlag) == "" || strings.TrimSpace(nodeNameFlag) == "" {
			return errors.New("--org <login> and --name <name> are required")
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
		org, err := gw.OrgByLogin(ctx, nodeOrgFlag)
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
		orgID, err := uuid.Parse(org.ID)
		if err != nil {
			return fmt.Errorf("org id %q: %w", org.ID, err)
		}
		if nodeCapacityFlag <= 0 {
			nodeCapacityFlag = 1
		}
		cfg := &node.Config{Gateway: gw.BaseURL, OrgLogin: org.Login, OrgID: org.ID, AccountID: org.AccountID, OwnerID: me,
			Slug: slug, Name: strings.TrimSpace(nodeNameFlag), NodeID: gateway.NodeID(orgID, slug).String(), Capacity: nodeCapacityFlag}
		if err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: cfg.OrgID, OwnerID: me, Name: cfg.Name, Slug: slug,
			Harnesses: []string{"claude-code"}, Capacity: cfg.Capacity, Version: version.Version}); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		path, _ := node.ConfigPath()
		fmt.Printf("Registered node %s (%s) for %s: id %s, capacity %d. Saved %s.\nStart it with: mesh node start\n", cfg.Name, slug, org.Login, cfg.NodeID, cfg.Capacity, path)
		return nil
	},
}

var nodeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Heartbeat once and list the org's nodes",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := node.LoadConfig()
		if err != nil {
			return err
		}
		gw, err := platformClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := gw.HeartbeatNode(ctx, cfg.NodeID, version.Version, 0); err != nil {
			return fmt.Errorf("heartbeat: %w", err)
		}
		nodes, err := gw.Nodes(ctx, cfg.OrgID)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSLUG\tSTATUS\tRUNNING\tCAPACITY\tVERSION\tLAST_SEEN")
		for _, n := range nodes {
			mark := ""
			if n.ID == cfg.NodeID {
				mark = " *"
			}
			fmt.Fprintf(w, "%s%s\t%s\t%s\t%d\t%d\t%s\t%s\n", n.Name, mark, n.Slug, n.Status, n.Running, n.Capacity, n.Version, n.LastSeenAt)
		}
		return w.Flush()
	},
}

var nodeLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove ~/.mesh/token",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := auth.ClearToken(); err != nil {
			return err
		}
		fmt.Println("Token removed. Revoke it on the console under Settings → Tokens.")
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
		cfg, err := node.LoadConfig()
		if err != nil {
			return err
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
		logger := log.New(os.Stderr, "mesh node: ", log.LstdFlags)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		regCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err = gw.RegisterNode(regCtx, gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: cfg.OrgID, OwnerID: cfg.OwnerID, Name: cfg.Name, Slug: cfg.Slug,
			Harnesses: []string{"claude-code"}, Capacity: cfg.Capacity, Version: version.Version})
		cancel()
		if err != nil {
			return fmt.Errorf("register node: %w", err)
		}

		loop := &node.Loop{GW: gw, Cfg: cfg, Version: version.Version, Harness: "claude-code", Interval: nodeIntervalFlag, Log: logger}
		switch nodeModeFlag {
		case "poll":
			return runPollMode(ctx, cfg, gw, loop, workRoot, logger)
		case "channel", "":
			return runChannelMode(ctx, cfg, gw, loop, workRoot, logger)
		default:
			return fmt.Errorf("--mode must be channel or poll")
		}
	},
}

func runPollMode(ctx context.Context, cfg *node.Config, gw *gateway.Client, loop *node.Loop, workRoot string, logger *log.Logger) error {
	h := &harness.ClaudeCode{Stderr: os.Stderr}
	if nodeUnattended {
		h.ExtraArgs = []string{"--dangerously-skip-permissions"}
	}
	runner := &node.Runner{GW: gw, Harness: h, WS: &node.Git{Log: os.Stderr}, Cfg: cfg, WorkRoot: workRoot, MaxBudgetUSD: nodeMaxBudgetFlag, Log: logger}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }
	logger.Printf("poll mode: node %s (%s) for %s, capacity %d, every %s", cfg.Name, cfg.Slug, cfg.OrgLogin, cfg.Capacity, loop.Interval)
	if nodeOnceFlag {
		loop.Tick(ctx)
		return nil
	}
	err := loop.Run(ctx)
	if errors.Is(err, context.Canceled) {
		logger.Printf("stopped; running items were left for a retry")
		return nil
	}
	return err
}

func runChannelMode(ctx context.Context, cfg *node.Config, gw *gateway.Client, loop *node.Loop, workRoot string, logger *log.Logger) error {
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

	sess := &channel.Session{Cfg: cfg, WorkDir: filepath.Join(workRoot, cfg.Slug), Exe: exe, SockPath: sock, Unattended: nodeUnattended, Stderr: os.Stderr, Log: logger,
		OnText: func(t string) { logger.Printf("session: %s", firstLine(t)) }}
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
		if err := sess.Send(channel.UserMessage(content, meta)); err != nil {
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
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		res, err := node.Resolve(ctx, gw, item)
		if err != nil {
			logger.Printf("item %s: resolve: %v", item.ID, err)
			_ = gw.FailWorkItem(ctx, item.ID, "resolve: "+err.Error())
			return
		}
		spec := node.WorkspaceSpec{Root: workRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: res.Branch(), BaseRef: res.BaseRef()}
		dir, err := ws.Prepare(ctx, spec)
		if err != nil {
			logger.Printf("item %s: workspace: %v (the session will retry the clone)", item.ID, err)
			dir = spec.Dir()
		}
		content, meta := channel.ReadyEvent(res, dir)
		s := &slot{content: content, meta: meta, done: make(chan string, 1)}
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
	repush := func(ctx context.Context, all bool) {
		var held []gateway.WorkItem
		for _, status := range []string{"claimed", "running"} {
			items, err := gw.OrgWorkItems(ctx, cfg.OrgID, status)
			if err != nil {
				logger.Printf("list held items: %v", err)
				return
			}
			for _, it := range items {
				if it.NodeID == cfg.NodeID {
					held = append(held, it)
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
	loop.AfterTick = func(ctx context.Context) { repush(ctx, false) }

	logger.Printf("channel mode: node %s (%s) for %s, capacity %d, every %s; session in %s", cfg.Name, cfg.Slug, cfg.OrgLogin, cfg.Capacity, loop.Interval, sess.WorkDir)
	go channel.WatchGoals(ctx, gw, cfg.OrgID, loop.Interval, func(kind string, g gateway.Goal) {
		content, meta := channel.GoalEvent(kind, g)
		push(content, meta)
	})
	go func() { _ = sess.Run(ctx, workRoot) }()
	err = loop.Run(ctx)
	if errors.Is(err, context.Canceled) {
		logger.Printf("stopped; claimed items were left for a retry")
		return nil
	}
	return err
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
	nodeRegisterCmd.Flags().StringVar(&nodeOrgFlag, "org", "", "the org's GitHub login")
	nodeRegisterCmd.Flags().StringVar(&nodeNameFlag, "name", "", "the node's name as the console shows it")
	nodeRegisterCmd.Flags().StringVar(&nodeSlugFlag, "slug", "", "the node's slug (default: from --name); the node id derives from it")
	nodeRegisterCmd.Flags().IntVar(&nodeCapacityFlag, "capacity", 1, "concurrent sessions")
	nodeStartCmd.Flags().DurationVar(&nodeIntervalFlag, "interval", 30*time.Second, "heartbeat and claim interval")
	nodeStartCmd.Flags().DurationVar(&nodeStallFlag, "stall", 10*time.Minute, "channel mode: re-push a held item the idle session has reported nothing on for this long")
	nodeStartCmd.Flags().Float64Var(&nodeMaxBudgetFlag, "max-budget-usd", 10, "cap on one session's budget (poll mode), whatever the goal has left")
	nodeStartCmd.Flags().StringVar(&nodeModeFlag, "mode", "channel", "channel (one persistent session fed by the mesh channel) or poll (claude -p per item)")
	nodeStartCmd.Flags().BoolVar(&nodeUnattended, "unattended", false, "run sessions with --dangerously-skip-permissions (trusted machines only)")
	nodeStartCmd.Flags().BoolVar(&nodeOnceFlag, "once", false, "poll mode: one tick, then exit")
	nodeCmd.AddCommand(nodeLoginCmd, nodeRegisterCmd, nodeStartCmd, nodeStatusCmd, nodeLogoutCmd)
}
