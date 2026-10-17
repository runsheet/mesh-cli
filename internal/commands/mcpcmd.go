package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/latchkey"
	"projectmesh.io/mesh-cli/internal/mcpinstall"
	"projectmesh.io/mesh-cli/internal/meshmcp"
	"projectmesh.io/mesh-cli/internal/node"
)

// mcpCmd is `mesh mcp`: Mesh inside Claude Code, added once with
// `claude mcp add runsheet -- mesh mcp` (ADR-023, mesh mcp). It speaks MCP
// on stdin/stdout on the credential in ~/.mesh/token — the person's
// Latchkey login or an rs_* token, refreshed under the lock — and
// writes nothing else to stdout.
var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve Mesh's tools to Claude Code: a stdio MCP server on your own login",
	Long: "mesh mcp is a stdio MCP server for Claude Code: add it once with\n\n" +
		"  claude mcp add --scope user runsheet -- mesh mcp\n\n" +
		"and Claude Code gets mesh_goals, mesh_status, mesh_join, mesh_switch, mesh_progress,\n" +
		"mesh_ask, mesh_finish and mesh_release on the credential in ~/.mesh/token.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		gw, err := platformClient()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "mesh mcp: "+format+"\n", a...) }
		// the platform's /mcp is asked for its work tools at the start, and
		// again hourly as the tools are used (ADR-023, Hosted path)
		hosted := meshmcp.NewHosted(gw)
		go hosted.Serves(ctx)
		// the personal node is the person's own, on their own login, never
		// a machine's: it registers nothing through registrar and so
		// sends no credentialId (runsheet#2290 asks it of a machine alone)
		p := &meshmcp.Personal{GW: gw, Join: &meshmcp.Join{
			GW: gw, WS: &node.Git{Log: os.Stderr}, NodeID: mcpNodeID,
			Fallback: &node.TokenFallback{}, Hosted: hosted, Logf: logf,
		}}
		return p.Server().Serve(ctx, os.Stdin, os.Stdout)
	},
}

// mcpNodeID is the node mesh_join claims for: node.yaml's node id until
// the personal node's registration (the goal's item 2) answers its own.
func mcpNodeID(context.Context) (string, error) {
	cfg, err := node.LoadConfig()
	if err != nil {
		return "", err
	}
	if cfg.NodeID == "" {
		return "", errors.New("no node registered on this machine yet")
	}
	return cfg.NodeID, nil
}

var (
	mcpInstallForce   bool
	mcpInstallDryRun  bool
	mcpInstallClients []string
	mcpInstallRemote  bool
	mcpInstallURL     string
)

// mcpExecutable is the running mesh binary, the command every client's
// entry runs; tests see the test binary.
var mcpExecutable = os.Executable

// mcpDiscoverAuth asks the MCP URL how it is signed into; tests answer
// without the network.
var mcpDiscoverAuth = mcpinstall.DiscoverAuth

// mcpInstallCmd is `mesh mcp install`: for each client of the install
// table (mcpinstall.Clients, ADR-025) — claude, codex, cursor — writes
// its MCP entry for `<this binary> mcp` into its own config file and
// format, and its instruction files rendered from the embedded `/mesh`
// skill (meshmcp.Skill; ADR-023, Skill). --client picks the clients
// (default: every one Detect finds), --dry-run prints each diff and
// writes nothing, --force overwrites an instruction file that differs.
// --remote writes the hosted server's entry instead — the gateway's
// /mcp, or --url — signed in by OAuth when its issuer offers dynamic
// registration, else a bearer read from $MESH_TOKEN, never a token in a
// file (ADR-025, Remote).
// Every other entry of the client's servers map that is runsheet's
// (mcpinstall.IsRunsheetEntry) — a legacy `mesh`, `mesh-context` or
// `prologue` — is removed, so one `runsheet` entry remains, each said
// `<client>: removed <name> from <path>`; a legacy name pointing
// elsewhere is kept, `<client>: kept <name> (not runsheet's)`.
// A file it cannot parse, or one that differs without --force, is
// reported and left alone, the other clients still written, and the
// command ends with status 1.
var mcpInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Add mesh mcp and the /mesh instructions to Claude Code, Codex and Cursor",
	Long: "mesh mcp install writes each client's MCP server entry for `mesh mcp` and its\n" +
		"instructions: Claude Code's ~/.claude.json and /mesh skill, Codex's\n" +
		"$CODEX_HOME/config.toml, Cursor's ~/.cursor/mcp.json. Every other entry is left\n" +
		"byte-for-byte; re-running changes nothing.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		exe, err := mcpExecutable()
		if err != nil {
			return err
		}
		if exe, err = filepath.Abs(exe); err != nil {
			return err
		}
		all := mcpinstall.Clients(meshmcp.Skill)
		var clients []mcpinstall.Client
		if len(mcpInstallClients) > 0 {
			if clients, err = mcpinstall.Select(all, mcpInstallClients); err != nil {
				return err
			}
		} else if clients = mcpinstall.Detected(all, home); len(clients) == 0 {
			return errors.New("no client found; pass --client")
		}

		opts := mcpinstall.PlanOptions{Force: mcpInstallForce}
		// the configured gateway's /mcp is runsheet's too
		if h := mcpinstall.HostOf(node.MeshMCPURL(gatewayURL())); h != "" {
			opts.Hosts = append(opts.Hosts, h)
		}
		if mcpInstallRemote {
			opts.Remote, opts.URL = true, strings.TrimSpace(mcpInstallURL)
			if opts.URL == "" {
				opts.URL = node.MeshMCPURL(gatewayURL())
			}
			// discovered once, for every client
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			opts.Auth = mcpDiscoverAuth(ctx, opts.URL)
			if h := mcpinstall.HostOf(opts.URL); h != "" {
				opts.Hosts = append(opts.Hosts, h)
			}
			fmt.Fprintf(out, "url: %s\nauth: %s\n", opts.URL, opts.Auth.Describe())
		} else if mcpInstallURL != "" {
			return errors.New("--url needs --remote")
		}
		changes, planErr := mcpinstall.Plan(home, exe, clients, opts)
		if !mcpInstallDryRun {
			if err := mcpinstall.Apply(changes); err != nil {
				return err
			}
		}
		claudeWritten := false
		for _, c := range changes {
			if mcpInstallDryRun {
				fmt.Fprint(out, mcpinstall.Diff(c.Path, c.Before, c.After))
			}
			fmt.Fprintf(out, "%s: %s %s\n", c.Client, c.Status(mcpInstallDryRun), c.Path)
			for _, name := range c.Removed {
				verb := "removed"
				if mcpInstallDryRun {
					verb = "would remove"
				}
				fmt.Fprintf(out, "%s: %s %s from %s\n", c.Client, verb, name, c.Path)
			}
			for _, name := range c.Kept {
				fmt.Fprintf(out, "%s: kept %s (not runsheet's)\n", c.Client, name)
			}
			claudeWritten = claudeWritten || (c.Client == "claude" && c.What == "mcp")
		}
		left := 0
		for _, err := range unjoin(planErr) {
			left++
			var u *mcpinstall.UnparseableError
			if errors.As(err, &u) {
				fmt.Fprintf(out, "%s: cannot parse (%v); left alone\n", u.Path, u.Err)
			} else {
				fmt.Fprintf(out, "%v; left alone\n", err)
			}
		}
		for _, c := range clients {
			if _, oauth := opts.Auth.(mcpinstall.OAuth); oauth && c.Name == "codex" {
				fmt.Fprintf(out, "codex: sign in with `codex mcp login %s`\n", mcpinstall.ServerName)
			}
			if c.Name == "claude" && !claudeWritten && mcpInstallRemote {
				fmt.Fprintf(out, "\nAdd runsheet to Claude Code by hand with:\n\n  %s\n", claudeRemoteAdd(opts))
			} else if c.Name == "claude" && !claudeWritten {
				fmt.Fprintf(out, "\nAdd runsheet (mesh mcp) to Claude Code by hand with:\n\n  claude mcp add --scope user %s -- %s mcp\n", mcpinstall.ServerName, exe)
			}
		}
		if left > 0 {
			cmd.SilenceUsage = true
			return fmt.Errorf("%d file(s) left alone", left)
		}
		return nil
	},
}

var mcpDoctorJSON bool

// mcpLoadCredential is the credential `mesh mcp doctor` names the
// identity of — MESH_TOKEN, else ~/.mesh/token; tests load a fake.
var mcpLoadCredential = auth.LoadCredential

// identityGateway is what `mesh mcp doctor` asks the gateway: who the
// bearer is, and the person's orgs and their projects.
type identityGateway interface {
	Me(ctx context.Context) (*gateway.Person, error)
	MyOrgs(ctx context.Context) ([]gateway.Org, error)
	Projects(ctx context.Context, orgID string) ([]gateway.Project, error)
}

// mcpIdentityGateway is the gateway client on cred; tests answer
// without the network.
var mcpIdentityGateway = func(cred *auth.Credential) identityGateway {
	return gateway.NewWithSource(gatewayURL(), auth.NewSource(cred, refreshMachine))
}

// mcpIdentity is who the runsheet server acts as and what it reaches,
// by the identity rule of runsheet ADR 056 and Latchkey ADR 013: a
// person's MCP resolves to the person and their orgs; a machine's
// subject is the machine with its operator beside it, so the person is
// the operator, Via the machine. A session token is never a person and
// is never what ~/.mesh/token holds.
type mcpIdentity struct {
	// Person is the person's email (else their id); "" not signed in.
	Person string `json:"person"`
	// Via is `machine <id>` for a Latchkey machine credential.
	Via string `json:"via,omitempty"`
	// Projects are the person's orgs' projects, `<org>/<slug>`, sorted.
	Projects []string `json:"projects"`
	// Error is why the identity or its projects could not be read:
	// `not signed in; run mesh auth login`, or the gateway's error.
	Error string `json:"error,omitempty"`
	// signedIn is false when no credential loads; doctor exits 1.
	signedIn bool
}

// notSignedIn is the identity line's reason with no credential.
const notSignedIn = "not signed in; run mesh auth login"

// doctorIdentity loads the credential and asks the gateway who it is:
// a console token's user, a machine credential's operator (the token
// file's userEmail, else the gateway's me, else its operator id), then
// each of the person's orgs' projects. A gateway error is kept on
// Error, never fatal.
func doctorIdentity(ctx context.Context) mcpIdentity {
	cred, err := mcpLoadCredential()
	if err != nil || cred == nil {
		return mcpIdentity{Projects: []string{}, Error: notSignedIn}
	}
	id := mcpIdentity{Projects: []string{}, signedIn: true}
	if cred.IsMachine() {
		m := cred.Machine
		id.Person = strings.TrimSpace(m.UserEmail)
		machine := nodeCredentialID(cred)
		if machine == "" {
			machine = "unnamed"
		}
		id.Via = "machine " + machine
	}
	gw := mcpIdentityGateway(cred)
	if id.Person == "" {
		me, err := gw.Me(ctx)
		switch {
		case err != nil:
			id.Error = err.Error()
		case me != nil && strings.TrimSpace(me.Email) != "":
			id.Person = strings.TrimSpace(me.Email)
		case me != nil:
			id.Person = strings.TrimSpace(me.ID)
		}
	}
	if id.Person == "" && cred.IsMachine() {
		id.Person = operatorID(cred)
	}
	if id.Person == "" {
		id.Person = "unknown"
	}
	if id.Error != "" {
		return id
	}
	orgs, err := gw.MyOrgs(ctx)
	if err != nil {
		id.Error = err.Error()
		return id
	}
	for _, o := range orgs {
		ps, err := gw.Projects(ctx, o.ID)
		if err != nil {
			id.Error = err.Error()
			id.Projects = []string{}
			return id
		}
		name := o.Name()
		if name == "" {
			name = o.Slug
		}
		for _, p := range ps {
			id.Projects = append(id.Projects, name+"/"+p.Slug)
		}
	}
	sort.Strings(id.Projects)
	return id
}

// operatorID is a machine credential's operator: the token file's
// userId, else the access token's operator.sub, else "".
func operatorID(cred *auth.Credential) string {
	if id := strings.TrimSpace(cred.Machine.UserID); id != "" {
		return id
	}
	if claims, err := latchkey.ParseClaims(cred.Machine.AccessToken); err == nil {
		return strings.TrimSpace(claims.Operator.Sub)
	}
	return ""
}

// identityLines are doctor's first lines: `identity: <email> (person)`
// or `identity: <email> (person, via machine <id>)` and `projects:
// <org>/<slug>, …` (or `none`); `identity: not signed in; run mesh auth
// login`; or, the gateway failing, `identity: <email> (projects
// unavailable: <err>)`.
func identityLines(id mcpIdentity) []string {
	switch {
	case !id.signedIn:
		return []string{"identity: " + notSignedIn}
	case id.Error != "":
		return []string{fmt.Sprintf("identity: %s (projects unavailable: %s)", id.Person, id.Error)}
	}
	kind := "person"
	if id.Via != "" {
		kind += ", via " + id.Via
	}
	projects := "none"
	if len(id.Projects) > 0 {
		projects = strings.Join(id.Projects, ", ")
	}
	return []string{
		fmt.Sprintf("identity: %s (%s)", id.Person, kind),
		"projects: " + projects,
	}
}

// mcpDoctorCmd is `mesh mcp doctor`: first who the runsheet server
// will act as and what it reaches (doctorIdentity: the person, the
// machine it is via, the person's projects; runsheet ADR 056, Latchkey
// ADR 013), then for each client of the install table found on this
// machine (mcpinstall.Detected), whether its MCP file holds runsheet's
// entry and the binary or URL it points at, and each instruction file
// current, stale or missing — read only, an unparseable file reported
// and left alone (ADR-025, Doctor). --json prints `{"identity":
// {person, via, projects, error}, "clients": [...mcpinstall.Report]}`.
// It exits 1 when no credential loads, or when any client found is not
// Healthy: its entry missing, unparseable, duplicate or legacy, or an
// instruction stale or missing. A gateway error is printed, not fatal.
var mcpDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Say who runsheet acts as and, per client found, whether it is configured, what it points at and whether its instructions are current",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		id := doctorIdentity(ctx)
		reports := mcpinstall.Doctor(mcpinstall.Clients(meshmcp.Skill), home)
		if reports == nil {
			reports = []mcpinstall.Report{}
		}
		if mcpDoctorJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(struct {
				Identity mcpIdentity         `json:"identity"`
				Clients  []mcpinstall.Report `json:"clients"`
			}{id, reports}); err != nil {
				return err
			}
		} else {
			for _, l := range identityLines(id) {
				fmt.Fprintln(out, l)
			}
			if len(reports) == 0 {
				fmt.Fprintln(out, "no client found")
			}
			for _, r := range reports {
				fmt.Fprintln(out, doctorLine(r))
				for _, i := range r.Instructions {
					fmt.Fprintf(out, "  %s: %s\n", i.Path, i.Status)
				}
			}
		}
		var unhealthy []string
		for _, r := range reports {
			if !r.Healthy() {
				unhealthy = append(unhealthy, r.Client)
			}
		}
		var problems []string
		if !id.signedIn {
			problems = append(problems, notSignedIn)
		}
		if len(unhealthy) > 0 {
			problems = append(problems, fmt.Sprintf("not configured and current: %s; run mesh mcp install", strings.Join(unhealthy, ", ")))
		}
		if len(problems) > 0 {
			cmd.SilenceUsage = true
			return errors.New(strings.Join(problems, "; "))
		}
		return nil
	},
}

// doctorLine is one client's line: `<client>: configured <binary or
// url> (<path>)`, `<client>: duplicate runsheet entries: runsheet, mesh
// (<path>); run mesh mcp install`, `<client>: legacy entry mesh
// (<path>); run mesh mcp install`, `<client>: missing in <path>`, or
// `<client>: unparseable <path> (<err>); left alone`.
func doctorLine(r mcpinstall.Report) string {
	switch r.Status {
	case mcpinstall.Configured:
		target := r.Command
		if r.URL != "" {
			target = r.URL
		}
		if target == "" {
			target = "(no command or url)"
		}
		return fmt.Sprintf("%s: configured %s (%s)", r.Client, target, r.Path)
	case mcpinstall.Duplicate:
		names := append([]string{mcpinstall.ServerName}, r.Duplicates...)
		return fmt.Sprintf("%s: duplicate runsheet entries: %s (%s); run mesh mcp install",
			r.Client, strings.Join(names, ", "), r.Path)
	case mcpinstall.Legacy:
		return fmt.Sprintf("%s: legacy entry %s (%s); run mesh mcp install",
			r.Client, strings.Join(r.Duplicates, ", "), r.Path)
	case mcpinstall.Unparseable:
		return fmt.Sprintf("%s: unparseable %s (%s); left alone", r.Client, r.Path, r.Error)
	default:
		return fmt.Sprintf("%s: %s in %s", r.Client, r.Status, r.Path)
	}
}

// claudeRemoteAdd is the `claude mcp add` line for the remote entry.
func claudeRemoteAdd(opts mcpinstall.PlanOptions) string {
	line := "claude mcp add --scope user --transport http " + mcpinstall.ServerName + " " + opts.URL
	if b, ok := opts.Auth.(mcpinstall.Bearer); ok {
		line += ` --header "Authorization: Bearer ${` + b.EnvVar + `}"`
	}
	return line
}

// unjoin is the errors errors.Join joined, or err alone.
func unjoin(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

func init() {
	mcpInstallCmd.Flags().BoolVar(&mcpInstallForce, "force", false, "overwrite an instruction file that differs from the one mesh-cli ships")
	mcpInstallCmd.Flags().BoolVar(&mcpInstallDryRun, "dry-run", false, "print every file and diff the install would write; write nothing")
	mcpInstallCmd.Flags().StringSliceVar(&mcpInstallClients, "client", nil, "claude, codex, cursor or all (repeatable or a comma list; default every client found)")
	mcpInstallCmd.Flags().BoolVar(&mcpInstallRemote, "remote", false, "write the hosted server's entry (no local binary): OAuth when the issuer registers clients, else a bearer from $"+mcpinstall.BearerEnvVar)
	mcpInstallCmd.Flags().StringVar(&mcpInstallURL, "url", "", "the MCP URL for --remote (default: the gateway's /mcp)")
	mcpCmd.AddCommand(mcpInstallCmd)
	mcpDoctorCmd.Flags().BoolVar(&mcpDoctorJSON, "json", false, "print the reports as JSON")
	mcpCmd.AddCommand(mcpDoctorCmd)
}
