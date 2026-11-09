package commands

import (
	"github.com/google/uuid"

	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/latchkey"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/version"
)

var (
	nodeOrgsFlag       []string
	nodeNameFlag       string
	nodeSlugFlag       string
	nodeCapacityFlag   int
	nodeOwnerFlag      string
	nodeEnterpriseFlag string
)

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Run this machine as a Mesh node on the runsheet platform",
	Long: `A node runs work items of goals: it heartbeats, claims ready items for
its org and runs each in a fresh claude session, on the credential in
~/.mesh/token — the Latchkey machine credential ` + "`mesh node login`" + ` registers
for this computer, or the operator's rs_* console token.`,
	// node has no args of its own, only subcommands; unlike cobra's
	// default for a non-root command, an unmatched one is an error
	// here, not a silent fall-through to this command's own help. A
	// RunE makes the command Runnable, which Args needs: cobra skips
	// ValidateArgs for a command with neither (command.go's execute,
	// `if !c.Runnable() { return flag.ErrHelp }`).
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
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
	id, err := gw.SignIn(ctx) // guarded: an rs_* console token (SaveToken refuses any other), !cred.IsMachine()
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
		// owner: org keeps the org a prior node.yaml named
		org := ""
		if owner == "org" {
			org = prev.OrgLogin
		}
		if err := (&node.Config{Owner: owner, Enterprise: enterprise, OrgLogin: org}).ValidateOwner(); err != nil {
			path, _ := node.ConfigPath()
			return fmt.Errorf("%s: %w", path, err)
		}
		gw, err := platformClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// an org-owned node has no person to sign in (the org owns it); a
		// machine credential's owner is its operator, never signIn's
		var me string
		cred := clientCredential(gw)
		if owner == "org" {
			if !gw.TakesOwnerOrg(ctx) {
				return node.ErrOwnerOrgGateway
			}
		} else if !cred.IsMachine() {
			if me, err = gw.SignIn(ctx); err != nil { // guarded: !cred.IsMachine()
				return err
			}
		} else if me, err = machineOwnerID(cred, owner); err != nil {
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
		if org != "" {
			cfg.OrgLogin, cfg.OrgID = org, prev.OrgID
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
			fmt.Printf("Registered %s, capacity %s. Saved %s.\nStart it with: mesh node run\n", node.StartLine(cfg, reg), cfg.Capacity, path)
			return nil
		}
		fmt.Printf("Registered node %s (%s) in %s, capacity %s. Saved %s.\nStart it with: mesh node run\n", cfg.Name, slug, strings.Join(reg.Scopes.Names(), ", "), cfg.Capacity, path)
		return nil
	},
}

// machineOwnerID is the owner id of a node on a machine credential: the
// operator the machine acts for — the token file's userId, else the
// access token's operator.sub — never signIn, which the platform refuses
// for a machine bearer (`signIn: a person's own, not a machine's`). An
// enterprise-owned node registers on the enterprise's id, so a machine
// that names no operator is refused only when the owner is a person.
func machineOwnerID(cred *auth.Credential, ownerKind string) (string, error) {
	id := strings.TrimSpace(cred.Machine.UserID)
	if id == "" {
		if claims, err := latchkey.ParseClaims(cred.Machine.AccessToken); err == nil {
			id = strings.TrimSpace(claims.Operator.Sub)
		}
	}
	if id == "" && ownerKind != "enterprise" {
		return "", errors.New("the machine credential names no operator (userId or the access token's operator.sub); set owner_id in node.yaml or run `mesh node login` again")
	}
	return id, nil
}

// registrar is node.Register's steps on the platform client: the
// owner probe and the field it found, the orgs of the token's person,
// the enterprise's id from its slug (myEnterprises), the one
// registration on the owner contract (the node's own id, the owner's id
// and kind), the legacy registration per org, and the retirement of the
// legacy per-org rows on the move to the owner contract.
//
// Its CredentialID is the machine id of the client's own credential
// (nodeCredentialID), sent as credentialId on every registerNode; a
// console token sends none.
func registrar(gw *gateway.Client, cfg *node.Config) node.Registrar {
	cred := clientCredential(gw)
	credID := nodeCredentialID(cred)
	return node.Registrar{
		CredentialID: credID,
		Machine:      cred.IsMachine(),
		CredentialLine: func(ctx context.Context) string {
			return node.CredentialLine(cred.IsMachine(), credID, credID != "" && gw.TakesNodeCredential(ctx))
		},
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
		// owner: org — the machine token's orgs read, never signIn
		ResolveOrg: func(ctx context.Context, slugOrID string) (string, error) {
			if _, err := uuid.Parse(strings.TrimSpace(slugOrID)); err == nil {
				return strings.TrimSpace(slugOrID), nil
			}
			orgs, err := gw.MyOrgs(ctx)
			if err != nil {
				return "", fmt.Errorf("org %q: my orgs: %w", slugOrID, err)
			}
			return node.FindOrg(slugOrID, orgs)
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
			return gw.RegisterNode(ctx, node.RegisterInput(cfg, nodeID, orgID, ownerID, cfg.OwnerKind(), credID, version.Resolved()))
		},
		RegisterIn: func(ctx context.Context, scopes node.Scopes) error { return registerIn(ctx, gw, cfg, scopes) },
	}
}

// registerIn registers the node in every scope (a Node per org, id from
// (org, slug); registering again is a heartbeat), each naming the
// client's machine credential as credentialId when it is one.
func registerIn(ctx context.Context, gw *gateway.Client, cfg *node.Config, scopes node.Scopes) error {
	credID := nodeCredentialID(clientCredential(gw))
	for _, sc := range scopes {
		if _, err := gw.RegisterNode(ctx, node.RegisterInput(cfg, sc.NodeID, sc.ID, cfg.OwnerID, "", credID, version.Resolved())); err != nil {
			return fmt.Errorf("register in %s: %w", sc.Name(), err)
		}
	}
	return nil
}

var nodeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "What the Mesh v1 node on this machine is doing",
	RunE: func(cmd *cobra.Command, args []string) error {
		v1, err := v1RunState()
		if err != nil {
			return err
		}
		lines, _ := v1StatusLines(v1, time.Now())
		for _, l := range lines {
			fmt.Fprintln(cmd.OutOrStdout(), l)
		}
		return nil
	},
}

var nodeDrainWait time.Duration

var nodeDrainCmd = &cobra.Command{
	Use:   "drain",
	Short: "Drain the node running on this machine: it claims nothing new, lets its sessions finish, then exits",
	Long: `Sends the running Mesh v1 node (` + "`mesh node run`" + `), whose pid is in
~/.mesh/node-v1/node.pid, SIGUSR1 — the same as ` + "`kill -USR1 <pid>`" + `.

The node asks Mesh to stop offering it leases (POST /v1/nodes/{id}/drain),
lets every lease it is running end, and exits 0; its next start asks Mesh to
resume it, and under systemd or launchd it is started again, on the binary
and config now on disk. With --wait, what it still runs once the wait is up
is handed back node_update (the job keeps its place, no attempt spent) and
the node exits. SIGTERM is not a drain: it stops at once, handing every
lease back node_restart.

Capacity and the orgs served need no drain: the node reads them every
tick.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		v1, err := v1RunState()
		if err != nil {
			return err
		}
		return drainEither(v1, nodeDrainWait, cmd.OutOrStdout())
	},
}

// drainEither drains the Mesh v1 node running on this machine. A legacy
// node (`mesh node start`) is no longer reachable through this command
// (the --api v0 path is gone, adr-044's amendment): rather than
// drainV1Node's "no Mesh v1 node is running" — true of the command's
// own target, misleading about the machine, since a legacy node in
// fact runs — a live one is refused clearly, naming its pid; it keeps
// draining on its own signal, `kill -USR1 <pid>`, or by upgrading it to
// a v1 node with `mesh node run`.
func drainEither(v1 node.RunState, wait time.Duration, out io.Writer) error {
	if wait < 0 {
		return fmt.Errorf("--wait %s: a wait is not negative", wait)
	}
	if _, ok := liveNode(v1); !ok {
		if pid, ok := legacyNode(); ok {
			return fmt.Errorf("a legacy node (mesh node start) is running at pid %d; mesh node drain no longer supports it — signal it directly (kill -USR1 %d) or migrate to mesh node run", pid, pid)
		}
	}
	return drainV1Node(v1, wait, out)
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
	for _, c := range []*cobra.Command{nodeLoginCmd, nodeRegisterCmd, nodeStatusCmd} {
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
	nodeDrainCmd.Flags().DurationVar(&nodeDrainWait, "wait", 0, "hand back node_update what still runs after this long, then exit (default wait for every lease)")
	nodeCmd.AddCommand(nodeLoginCmd, nodeRegisterCmd, nodeStatusCmd, nodeDrainCmd, nodeUpgradeCmd, nodeLogoutCmd, nodeModelCmd)
}

// nodeModelCmd used to read and write the models in ~/.mesh/node.yaml
// for the legacy node (which chose the model per kind of session before
// each one started). A Mesh v1 node (`mesh node run`) runs each lease
// on the model its job brief names (ADR-044): there is no node.yaml
// table to show or set, so the command is message-only and touches
// node.yaml not at all.
var nodeModelCmd = &cobra.Command{
	Use:   "model",
	Short: "Where a v1 node's model comes from",
	Long: `A Mesh v1 node (` + "`mesh node run`" + `) runs every job on the model its
lease's job brief names — the platform's choice, sent with the job — never
a model configured on this machine. There is no node.yaml model table left
to show or set, and this command writes nothing to node.yaml.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		fmt.Fprintln(cmd.OutOrStdout(), "a v1 node's model comes from the lease's job brief, not node.yaml: this command writes nothing to node.yaml")
		return nil
	},
}
