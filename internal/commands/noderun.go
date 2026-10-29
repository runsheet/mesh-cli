package commands

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/nodev1"
	"projectmesh.io/mesh-cli/internal/paths"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/sigstore"
	"projectmesh.io/mesh-cli/internal/version"
)

var (
	nodeRunAPI        string
	nodeRunURL        string
	nodeRunName       string
	nodeRunWorkRoot   string
	nodeRunOnce       bool
	nodeRunUnattended bool
	nodeRunCPUs       float64
	nodeRunMemGB      float64
	nodeRunSlots      int
	nodeRunGitHost    string
	nodeRunNoMirrors  bool
	nodeRunConnect    []string
	nodeRunFulcioURL  string
	nodeRunRekorURL   string
	// The actor node's flags (ADR-044), each off by default.
	nodeRunScheduler    bool
	nodeRunAccountSlots int
	nodeRunActors       bool
)

// nodeRunCmd is the Mesh v1 node runtime (internal/nodev1), beside the
// legacy `mesh node start`: it speaks only Mesh's v1 node API, on the
// machine credential `mesh node login` saved.
var nodeRunCmd = &cobra.Command{
	Use:   "run --api v1",
	Short: "Run this machine as a Mesh v1 node (beside `mesh node start`)",
	Long: `Run this machine as a Mesh v1 node: register with Mesh (api.projectmesh.io),
take one lease at a time, run its claude-code session with the brief's product
MCP servers through a sessionproxy of the lease's own, renew the lease while it
runs, and finish or hand back. The legacy runtime is ` + "`mesh node start`" + `.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if nodeRunAPI != "v1" {
			return fmt.Errorf("mesh node run: --api must be v1 (the legacy runtime is `mesh node start`), not %q", nodeRunAPI)
		}
		src, err := tokenSource()
		if err != nil {
			return fmt.Errorf("mesh node run: no machine credential (run `mesh node login`): %w", err)
		}
		dir, err := paths.MeshDir()
		if err != nil {
			return err
		}
		root := nodeRunWorkRoot
		if root == "" {
			root = filepath.Join(dir, "v1")
		}
		key, err := nodev1.LoadSigningKey(filepath.Join(root, "signing.key"))
		if err != nil {
			return err
		}
		name := nodeRunName
		if name == "" {
			name, _ = os.Hostname()
		}
		cpus := nodeRunCPUs
		if cpus <= 0 {
			cpus = float64(runtime.NumCPU())
		}
		connect, err := connectionsOf(nodeRunConnect)
		if err != nil {
			return err
		}
		logger := log.New(os.Stderr, "mesh node v1: ", log.LstdFlags)
		sess := &nodev1.ClaudeSession{Stderr: os.Stderr, Logf: logger.Printf}
		if nodeRunUnattended {
			sess.ExtraArgs = []string{"--dangerously-skip-permissions"}
		}
		rt := &nodev1.Runtime{
			API: &meshapi.Client{BaseURL: meshAPIURL(), Token: src.Token},
			Node: meshapi.RegisterRequest{
				Name: name, Version: version.Resolved(), Harnesses: []string{"claude-code"},
				CapacityTotal: meshapi.Capacity{CPUs: cpus, MemGB: nodeRunMemGB, Slots: max(nodeRunSlots, 1)},
				SigningKey:    nodev1.PublicKey(key),
			},
			SigningKey:  key,
			Sigstore:    nodeRunSigstore(logger),
			Connect:     connect,
			WorkRoot:    root,
			MeshBinary:  meshBinary(),
			GitUpstream: gitUpstream(),
			NoMirrors:   nodeRunNoMirrors,
			Session:     sess,
			Once:        nodeRunOnce,
			Logf:        logger.Printf,
		}
		if nodeRunScheduler {
			rt.Scheduler = scheduler.New(scheduler.Capacity{
				CPUs: rt.Node.CapacityTotal.CPUs, MemGB: rt.Node.CapacityTotal.MemGB, Slots: rt.Node.CapacityTotal.Slots,
				AccountSlots: nodeRunAccountSlots,
			})
			rt.Scheduler.Logf = logger.Printf
		}
		rt.Actors = nodeRunActors
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := rt.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	},
}

// meshAPIURL is Mesh's v1 API: --url, else MESH_API_URL, else
// api.projectmesh.io.
func meshAPIURL() string {
	if v := strings.TrimSpace(nodeRunURL); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("MESH_API_URL")); v != "" {
		return v
	}
	return meshapi.DefaultURL
}

// gitUpstream is the git host a lease's relay reaches on its minted
// token: --git-upstream, else MESH_GIT_UPSTREAM, else GitHub (empty).
// Mesh's e2e rig points it at its fake GitHub.
func gitUpstream() string {
	if v := strings.TrimSpace(nodeRunGitHost); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("MESH_GIT_UPSTREAM"))
}

// nodeRunSigstore is the Fulcio and Rekor a lease's node tools sign
// their records through (Mesh v1 spec, "Evidence signing"):
// --fulcio-url and --rekor-url, else MESH_FULCIO_URL and MESH_REKOR_URL,
// else node.yaml's `sigstore: {fulcio_url, rekor_url}` (the legacy
// node's, which dl360 sets to its own Fulcio and Rekor on loopback).
// None leaves every record signed by the node's key, marked
// node-signed.
func nodeRunSigstore(logger *log.Logger) *sigstore.Signer {
	fulcio, rekor := strings.TrimSpace(nodeRunFulcioURL), strings.TrimSpace(nodeRunRekorURL)
	if fulcio == "" {
		fulcio = strings.TrimSpace(os.Getenv("MESH_FULCIO_URL"))
	}
	if rekor == "" {
		rekor = strings.TrimSpace(os.Getenv("MESH_REKOR_URL"))
	}
	if fulcio == "" && rekor == "" {
		if cfg, err := node.LoadSettings(); err == nil {
			fulcio, rekor = cfg.Sigstore.FulcioURL, cfg.Sigstore.RekorURL
		}
	}
	s := &sigstore.Signer{FulcioURL: fulcio, RekorURL: rekor}
	if !s.Enabled() {
		logger.Printf("sigstore: no Fulcio and Rekor: node tools sign their records with the node's key, marked node-signed")
		return nil
	}
	logger.Printf("sigstore: node tools sign their records through %s and log them in %s", fulcio, rekor)
	return s
}

// connectionsOf reads --connect's tenant/product pairs.
func connectionsOf(pairs []string) ([]meshapi.ConnectRequest, error) {
	var out []meshapi.ConnectRequest
	for _, p := range pairs {
		tenant, product, ok := strings.Cut(strings.TrimSpace(p), "/")
		if !ok || tenant == "" || product == "" || strings.Contains(product, "/") {
			return nil, fmt.Errorf("mesh node run: --connect is tenant/product, not %q", p)
		}
		out = append(out, meshapi.ConnectRequest{Tenant: tenant, Product: product})
	}
	return out, nil
}

func init() {
	f := nodeRunCmd.Flags()
	f.StringVar(&nodeRunAPI, "api", "", "the API the node speaks: v1 (required)")
	f.StringVar(&nodeRunURL, "url", "", "Mesh's API (default $MESH_API_URL, else "+meshapi.DefaultURL+")")
	f.StringVar(&nodeRunName, "name", "", "the node's name (default the hostname)")
	f.StringVar(&nodeRunWorkRoot, "work-root", "", "where leases run (default ~/.mesh/v1)")
	f.BoolVar(&nodeRunOnce, "once", false, "stop after the first lease ends")
	f.BoolVar(&nodeRunUnattended, "unattended", false, "run sessions with --dangerously-skip-permissions (trusted machines only)")
	f.Float64Var(&nodeRunCPUs, "cpus", 0, "CPUs offered (default all)")
	f.Float64Var(&nodeRunMemGB, "mem-gb", 0, "memory offered, in GB")
	f.IntVar(&nodeRunSlots, "slots", 1, "sessions at once")
	f.StringVar(&nodeRunGitHost, "git-upstream", "", "a test override: the git host every lease's relay reaches, whatever its brief's vendor and base_url name (default $MESH_GIT_UPSTREAM)")
	f.BoolVar(&nodeRunNoMirrors, "no-mirrors", false, "clone each lease's repository whole, keeping no per-tenant mirror under the work root")
	f.StringSliceVar(&nodeRunConnect, "connect", nil, "offer the node to a tenant for a product, tenant/product (repeatable); Mesh offers a node only its connections' jobs")
	f.StringVar(&nodeRunFulcioURL, "fulcio-url", "", "the Fulcio node tools' records are certified by (default $MESH_FULCIO_URL, else node.yaml's sigstore.fulcio_url)")
	f.StringVar(&nodeRunRekorURL, "rekor-url", "", "the Rekor node tools' records are logged in (default $MESH_REKOR_URL, else node.yaml's sigstore.rekor_url)")
	f.BoolVar(&nodeRunScheduler, "actors-scheduler", false, "the scheduler owns the node's capacity (ADR-044): leases hold their resources as scheduler leases, the node reports what is free on each take and runs up to --slots leases side by side")
	f.IntVar(&nodeRunAccountSlots, "account-slots", 0, "with --actors-scheduler, sessions at once per model account (0 is unbounded)")
	f.BoolVar(&nodeRunActors, "actors", false, "run each lease as a job actor (ADR-044): it owns the lease's directory, room, grant and proxy, writes actor.json before each state change, releases everything however the lease ends, and hands back pod_killed a session killed from outside; a restarted node releases the actors it left")
	_ = nodeRunCmd.MarkFlagRequired("api")
	nodeCmd.AddCommand(nodeRunCmd)
}
