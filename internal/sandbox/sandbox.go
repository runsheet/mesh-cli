package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Defaults of node.yaml's `sandbox:` (ADR 007 Limits).
const (
	DefaultCPUs           = 2.0
	DefaultMemory         = "6g"
	DefaultPids           = 4096
	DefaultServicesCPUs   = 2.0
	DefaultServicesMemory = "4g"
	// DefaultImageRepo is the node image; its tag is the node's version.
	DefaultImageRepo = "ghcr.io/runsheet/mesh-node"
	// DefaultPath is the agent's PATH in the node image.
	DefaultPath = "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	// TimeoutGrace is how much longer than the session's own timeout the
	// runtime's backstop waits: the node's context stops the session
	// first, as it always has; the runtime's is for when it could not.
	TimeoutGrace = 30 * time.Second
)

// DefaultImage is the node image of a node version: the version is its
// tag, as it is (v1.2.0 is mesh-node:v1.2.0, a dev build mesh-node:dev,
// which no release pushes: `sandbox.image: build` makes it locally).
func DefaultImage(version string) string {
	if version == "" {
		version = "dev"
	}
	return DefaultImageRepo + ":" + version
}

// AllowedEnv are the variables of a session's own environment (the
// job's, never the node's) that reach its pod: the account's secret and
// label (adr-005), the Codex keys and CODEX_HOME, and the session's agent
// identity (ADR-024). Tool secrets (ADR 006) are allowed by
// ToolEnvPrefix. CODEX_HOME is admitted by name only: Session.Env moves
// it under the session's HOME, so a host path never reaches the pod.
//
// The item's GitHub token and gh config dir (ADR-001, ghcred.go's
// SessionEnv) are not among them: the job container holds no GitHub
// credential (runsheet adr-061 §4, one exchange). A pod's gh is signed
// out, its HOME's gh config dir empty, and what a session would have
// asked of GitHub it asks of the mesh tools; the node fetches, pushes and
// opens the pull request on the item's token outside the pod. HeldOutEnv
// names them.
var AllowedEnv = []string{
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY",
	"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_HOME",
	"MESH_ACCOUNT",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
}

// HeldOutEnv are the credential variables a job's environment may carry
// on the host that never reach its pod, whatever a later AllowedEnv says:
// the item's GitHub token and gh's config dir, and the other names gh and
// git read a GitHub token from.
var HeldOutEnv = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_CONFIG_DIR"}

// ToolEnvPrefix names a tool secret's variable (tools.EnvName).
const ToolEnvPrefix = "MESH_TOOL_"

// BaseEnv are the variables the sandbox sets itself for every session.
// DISABLE_AUTOUPDATER keeps claude from updating itself: the node image's
// own ENV says so too, but --unsetenv-all drops the image's variables.
// The package variables are set only when the node names its Stores, the
// scratch and Go cache ones only for a run given a ValidationCache.
var BaseEnv = []string{"PATH", "HOME", "LANG", "TZ", "TERM", "CLAUDE_CONFIG_DIR", "MESH_ITEM", "MESH_SESSION_KIND", "DISABLE_AUTOUPDATER", "npm_config_store_dir", "pnpm_config_store_dir", "PNPM_HOME", "npm_config_cache", "TMPDIR", "GOTMPDIR", "GOCACHE", "GOMODCACHE"}

// Stores are the node's package store and cache, one of each per node and
// shared by every session (mesh-specs ADR 017 §3): Pnpm is pnpm's
// content-addressable store (npm_config_store_dir, and
// pnpm_config_store_dir for pnpm 11 on, which reads no npm_config_
// variable and would otherwise put it under PNPM_HOME/store; PNPM_HOME,
// its global bin dir, the same dir), Npm the npm cache
// (npm_config_cache). Neither holds a credential. Empty is
// none: the session installs into its throwaway HOME as before.
//
// Each is its own bind mount in the pod, separate from the workspace's.
// link(2) refuses to cross mounts (EXDEV) even on one filesystem, so
// inside a pod pnpm falls back from hardlinking to copying into
// node_modules: what the shared store saves there is the download, not
// the disk. Mounting a parent of both the store and the workspace would
// let pnpm hardlink, but it would hand the session every other item's
// worktree, which ADR 017 §2 refuses. A session on the host — one bind of
// nothing — hardlinks from the same store.
type Stores struct {
	Pnpm string
	Npm  string
}

// dirs are the stores' directories that are set.
func (st Stores) dirs() []string {
	var d []string
	for _, p := range []string{st.Pnpm, st.Npm} {
		if p != "" {
			d = append(d, p)
		}
	}
	return d
}

// Env is the stores' variables.
func (st Stores) Env() []string {
	var e []string
	if st.Pnpm != "" {
		e = append(e, "npm_config_store_dir="+st.Pnpm, "pnpm_config_store_dir="+st.Pnpm, "PNPM_HOME="+st.Pnpm)
	}
	if st.Npm != "" {
		e = append(e, "npm_config_cache="+st.Npm)
	}
	return e
}

// ValidationCache is what a validation's pod is given of the node beside
// its workspace (goal a-node-validates-from-a-served-diff-with-a-shared-cache):
// Scratch, the run's own dir under the node's scratch root, read-write
// and the steps' TMPDIR and GOTMPDIR, so nothing a step makes lands in
// the host's /tmp; GoCache, the node's one Go build cache, read-write
// and their GOCACHE; and Blobs, the shared tree cache's blob store,
// read-only, so no session user can change what every later validation
// copies out of it. Nothing else of the cache root — its trees, its
// commits, its counts — is mounted. Each is its own bind mount at its
// own path, as the Stores are: link(2) refuses to cross them (EXDEV), so
// a step copies out of the blob store rather than hardlinking, which is
// the price of not mounting the cache root whole. GoModCache, when set,
// is the node's shared Go module cache, filled by the node before the
// run (FillGoModCache) and mounted read-only as the steps' GOMODCACHE, so
// no step downloads a module and none can change one every later run
// reads. Empty is none.
type ValidationCache struct {
	Scratch    string
	GoCache    string
	Blobs      string
	GoModCache string
}

// Mounts are the cache's mounts: the scratch and the Go cache
// read-write, the blob store and the Go module cache read-only.
func (c ValidationCache) Mounts() []Mount {
	var m []Mount
	for _, d := range []struct {
		path string
		ro   bool
	}{{c.Scratch, false}, {c.GoCache, false}, {c.Blobs, true}, {c.GoModCache, true}} {
		if d.path != "" {
			m = append(m, Mount{Source: d.path, Target: d.path, ReadOnly: d.ro})
		}
	}
	return m
}

// Env is env with TMPDIR and GOTMPDIR the scratch and GOCACHE the Go
// cache, each once: a value env already has is replaced, not shadowed.
func (c ValidationCache) Env(env []string) []string {
	set := map[string]string{}
	if c.Scratch != "" {
		set["TMPDIR"], set["GOTMPDIR"] = c.Scratch, c.Scratch
	}
	if c.GoCache != "" {
		set["GOCACHE"] = c.GoCache
	}
	if c.GoModCache != "" {
		set["GOMODCACHE"] = c.GoModCache
	}
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := set[name]; !ok {
			out = append(out, kv)
		}
	}
	for _, name := range []string{"TMPDIR", "GOTMPDIR", "GOCACHE", "GOMODCACHE"} {
		if v, ok := set[name]; ok {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// ensure makes the directories a bind mount needs: the Go cache and the
// blob store on first use. The scratch is the caller's, made per run.
func (c ValidationCache) ensure() error {
	for _, d := range []string{c.GoCache, c.Blobs} {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("sandbox: validation cache: %w", err)
		}
	}
	return nil
}

// Config is a node's sandbox: the image, the agent's and the services'
// limits, the registries services may come from, and the network policy.
type Config struct {
	Image          string
	Limits         Limits
	ServicesLimits Limits
	Registries     []string
	Network        NetworkPolicy
	// TZ is the session's time zone ("" is UTC).
	TZ string
	// ServiceTimeout bounds the wait for the services' health checks
	// (DefaultServiceTimeout when 0).
	ServiceTimeout time.Duration
	// ServicesTmpfs is the size of a service's tmpfs that names none
	// (DefaultServicesTmpfs when "").
	ServicesTmpfs string
	// Stores are the package store and cache every session installs
	// against (ADR 017 §3): the node's, or one org's (WithStores).
	Stores Stores
}

// Sandbox opens sessions on a runtime.
type Sandbox struct {
	Runtime Runtime
	Config  Config
	// TempDir is where temporary HOMEs are made ("" is os.TempDir).
	TempDir string
	// Confine, set (WithConfinement), confines its sessions to one job's
	// directory and its connection's caches: every mount must resolve
	// inside them (CheckedMounts, mesh-cli ADR-039).
	Confine Confinement
	// Logf is where the sandbox says what it fell back on (nil is the
	// standard logger).
	Logf func(format string, args ...any)
	// open is the sessions this process has open, shared with every
	// WithStores copy (openSet).
	open *openSet
	// Reserve, set, is told the CPUs each session's pod asks for — the
	// agent's Limits.CPUs at Open, the services' ServicesLimits.CPUs when
	// Start starts a sidecar — and hands back what gives them back, which
	// Close calls: the node's CPU budget (mesh-cli ADR-043). nil reserves
	// nothing; WithReserve makes a copy for pods whose CPUs are reserved
	// another way (a validation's, by its job).
	Reserve ReserveFunc
}

// ReserveFunc reserves cpus for one pod and returns what releases them.
type ReserveFunc func(cpus float64) (release func())

// WithReserve is a copy of sb whose sessions reserve their CPUs by fn
// (nil: none), sharing sb's open set.
func (sb *Sandbox) WithReserve(fn ReserveFunc) *Sandbox {
	if sb == nil {
		return nil
	}
	sb.openSessions()
	c := *sb
	c.Reserve = fn
	return &c
}

// reserve reserves cpus for s by its sandbox's Reserve, kept for Close.
func (s *Session) reserve(cpus float64) {
	if s.sb.Reserve == nil || cpus <= 0 {
		return
	}
	rel := s.sb.Reserve(cpus)
	if rel == nil {
		return
	}
	s.mu.Lock()
	s.releases = append(s.releases, rel)
	s.mu.Unlock()
}

// release gives back every CPU reservation s holds, once.
func (s *Session) release() {
	s.mu.Lock()
	rels := s.releases
	s.releases = nil
	s.mu.Unlock()
	for _, r := range rels {
		r()
	}
}

// openSet is the sessions of this node process between Open and Close:
// what a sweep run while the node runs (the quartermaster's, ADR-034)
// must keep whatever state their containers are in — a validation
// between two steps, a session between Open and its agent's first
// container, each a pod with only its infra running — which the
// start-up sweep, run before any session of this process, never meets.
type openSet struct {
	mu sync.Mutex
	m  map[*Session]struct{}
}

var openInit sync.Mutex

// openSessions is sb's open set, made on first use under openInit so a
// Sandbox and the WithStores copies made of it afterwards share one.
func (sb *Sandbox) openSessions() *openSet {
	openInit.Lock()
	defer openInit.Unlock()
	if sb.open == nil {
		sb.open = &openSet{m: map[*Session]struct{}{}}
	}
	return sb.open
}

// Live is what a sweep run while this node process runs keeps: the pods
// still standing and the HOME of every session Open made that Close has
// not ended (openSet). It is the age guard ADR-034 owed, on the exact
// sessions rather than an age.
func (sb *Sandbox) Live() Keep {
	var keep Keep
	if sb == nil {
		return keep
	}
	set := sb.openSessions()
	set.mu.Lock()
	sessions := make([]*Session, 0, len(set.m))
	for s := range set.m {
		sessions = append(sessions, s)
	}
	set.mu.Unlock()
	for _, s := range sessions {
		for _, p := range s.Pods() {
			keep.Pods = append(keep.Pods, p.Name)
		}
		keep.Homes = append(keep.Homes, s.Home)
	}
	return keep
}

func (sb *Sandbox) logf(format string, args ...any) {
	if sb != nil && sb.Logf != nil {
		sb.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// CodexVersion asks the node image for its codex (`codex --version` in
// a throwaway container, Runtime.ImageExec) and answers `codex <version>`
// ("codex 0.157.0"). An error is an image without codex — or one the
// runtime could not run — and a sandboxed node runs no Codex session
// then (ADR 007's amendment for Codex).
func (s *Sandbox) CodexVersion(ctx context.Context) (string, error) {
	if s == nil || s.Runtime == nil {
		return "", errors.New("sandbox: off")
	}
	out, err := s.Runtime.ImageExec(ctx, s.Config.Image, []string{"codex", "--version"})
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("codex --version in %s said nothing", s.Config.Image)
	}
	// "codex-cli 0.157.0": the last word is the version
	return "codex " + fields[len(fields)-1], nil
}

// Session is one agent session's sandbox: its temporary HOME, made on
// Open and removed on Close, and the pod Run creates and removes.
type Session struct {
	sb    *Sandbox
	Home  string
	Label string
	Kind  string
	name  string
	mu    sync.Mutex
	pods  []Pod
	// started is the pod Start made, which the next Run runs in.
	started *Pod
	// services is the pod Start made when it started sidecars: every
	// Run runs in it, beside them, and none removes it — Close does. A
	// validation runs its steps one Run each, and the second step on
	// must reach the services as the first did.
	services *Pod
	// runs counts the Runs made in the services pod, naming each its
	// own container.
	runs int
	// hooks is the checkout's hooks directory, mounted beside it.
	hooks string
	// image is the image ref the session's agent runs, the Sandbox's
	// Config.Image when it was opened.
	image string
	// detached is a session whose agent outlives the node process: Close
	// leaves its pods and HOME to whoever Reattaches it.
	detached bool
	// execs counts the commands Exec ran, naming each its own container.
	execs int
	// confine is the roots every mount must resolve inside (the
	// Sandbox's Confine when it was opened); zero confines nothing.
	confine Confinement
	// proxyDirs are the session's sessionproxy socket directories
	// (ShareProxyDir), mounted at their own paths; any set makes the
	// session proxied, and its env admits no tool secret.
	proxyDirs []string
	// releases give back the CPUs the session reserved (Sandbox.Reserve),
	// called by Close.
	releases []func()
	// pin names the session's pods and labels them beside SessionLabel
	// (Pin); agentLimits, set, replace the sandbox's agent Limits
	// (SetLimits). Both are a v1 node's, per lease (leasepod.go).
	pin         *podPin
	agentLimits *Limits
}

// ShareProxyDir mounts dir, the directory of the session's sessionproxy
// socket, read-write at its own path in every pod the session runs
// from now on — checked against the org root like every other mount —
// and makes the session proxied: its env (Env) admits no ToolEnvPrefix
// variable, since every MCP server is reached through the proxy, which
// holds the secrets on the host (mesh-cli goal
// a-session-reaches-every-mcp-server-through-one-proxy). The directory
// should hold the socket alone.
func (s *Session) ShareProxyDir(dir string) {
	if dir == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.proxyDirs {
		if d == dir {
			return
		}
	}
	s.proxyDirs = append(s.proxyDirs, dir)
}

// Proxied says whether the session reaches its MCP servers through a
// sessionproxy (ShareProxyDir).
func (s *Session) Proxied() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.proxyDirs) > 0
}

// GitIdentity is a session's commit identity: the agent its .gitconfig
// names (never the node's owner, ADR-024) and the hooks directory the
// checkout's core.hooksPath names, mounted at its own path. Empty
// fields write and mount nothing.
type GitIdentity struct {
	Name     string
	Email    string
	HooksDir string
}

// GitIdentityFromEnv is the identity a job's GIT_AUTHOR_NAME and
// GIT_AUTHOR_EMAIL name, with no hooks directory.
func GitIdentityFromEnv(env []string) GitIdentity {
	var id GitIdentity
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_AUTHOR_NAME":
			id.Name = v
		case "GIT_AUTHOR_EMAIL":
			id.Email = v
		}
	}
	return id
}

var unsafeName = regexp.MustCompile(`[^a-z0-9-]+`)

// Open makes a session's empty temporary HOME, with the .gitconfig
// naming git's identity and the empty gh and Claude config directories
// it is given.
func (s *Sandbox) Open(label, kind string, git GitIdentity) (*Session, error) {
	// the node's stores are made on first use: a bind mount's source
	// must exist
	for _, d := range s.Config.Stores.dirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("sandbox: package store: %w", err)
		}
	}
	if err := s.ensureHomes(); err != nil {
		return nil, fmt.Errorf("sandbox: home: %w", err)
	}
	home, err := os.MkdirTemp(s.TempDir, HomePrefix)
	if err != nil {
		return nil, fmt.Errorf("sandbox: home: %w", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		os.RemoveAll(home)
		return nil, fmt.Errorf("sandbox: home: %w", err)
	}
	for _, d := range []string{".claude", ".config/gh"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			os.RemoveAll(home)
			return nil, fmt.Errorf("sandbox: home: %w", err)
		}
	}
	var gc strings.Builder
	if git.Name != "" || git.Email != "" {
		gc.WriteString("[user]\n")
		if git.Name != "" {
			fmt.Fprintf(&gc, "\tname = %s\n", gitConfigValue(git.Name))
		}
		if git.Email != "" {
			fmt.Fprintf(&gc, "\temail = %s\n", gitConfigValue(git.Email))
		}
	}
	gc.WriteString("[safe]\n\tdirectory = *\n")
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gc.String()), 0o600); err != nil {
		os.RemoveAll(home)
		return nil, fmt.Errorf("sandbox: home: %w", err)
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(name) > 40 {
		name = name[:40]
	}
	sess := &Session{sb: s, Home: home, Label: label, Kind: kind, hooks: git.HooksDir, image: s.Config.Image, confine: s.Confine, name: "mesh-" + name + "-" + hex.EncodeToString(rnd[:])}
	set := s.openSessions()
	set.mu.Lock()
	set.m[sess] = struct{}{}
	set.mu.Unlock()
	sess.reserve(s.Config.Limits.CPUs)
	return sess, nil
}

// gitConfigValue quotes v for a .gitconfig line: `Mesh builder (mini)`
// keeps its parentheses, a quote or backslash is escaped, and a line
// break is dropped.
func gitConfigValue(v string) string {
	v = strings.NewReplacer("\r", "", "\n", "", `\`, `\\`, `"`, `\"`).Replace(v)
	return `"` + v + `"`
}

// ProbeLabel marks the throwaway pod Probe creates and removes, so a
// stale one from a killed probe is still recognisable as the node's own
// rather than a session's.
const ProbeLabel = "mesh.probe"

// Probe is the cheapest check that the runtime can start a pod to run a
// session in: its version, then a throwaway pod of the node image's
// labels, created and removed at once. A runtime that cannot — its
// rootless storage corrupted or locked, its user namespace refused, or
// no runtime there to ask at all — answers a *RuntimeError
// (validate.RuntimeBroken); a healthy one answers nil.
func (s *Sandbox) Probe(ctx context.Context) error {
	if _, err := s.Runtime.Version(ctx); err != nil {
		return asRuntimeError("version", err)
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	spec := PodSpec{
		Name:    "mesh-probe-" + hex.EncodeToString(rnd[:]),
		Labels:  map[string]string{ProbeLabel: "1"},
		Image:   s.Config.Image,
		Network: s.Config.Network,
	}
	pod, err := s.Runtime.CreatePod(ctx, spec)
	if err != nil {
		return asRuntimeError("pod create", err)
	}
	if err := s.Runtime.Remove(ctx, pod); err != nil {
		return asRuntimeError("pod remove", err)
	}
	return nil
}

// Image is the image ref the session's agent runs.
func (s *Session) Image() string { return s.image }

// Describe names the session and its image for the node's log:
// `session <label> — image <ref>`.
func (s *Session) Describe() string {
	return "session " + s.Label + " — image " + s.image
}

// WriteFile writes data at rel under the session's HOME and returns its
// path, which is the same inside the pod.
func (s *Session) WriteFile(rel string, data []byte) (string, error) {
	p := filepath.Join(s.Home, filepath.Clean("/"+rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, data, 0o600)
}

// ClaudeConfigDir is the session's CLAUDE_CONFIG_DIR, under its HOME.
func (s *Session) ClaudeConfigDir() string { return filepath.Join(s.Home, ".claude") }

// GHConfigDir is the session's empty gh config dir, under its HOME: gh's
// own default there, signed out (no GH_CONFIG_DIR or GH_TOKEN reaches the
// pod, HeldOutEnv).
func (s *Session) GHConfigDir() string { return filepath.Join(s.Home, ".config", "gh") }

// CodexHome is the session's CODEX_HOME, under its HOME: where a Codex
// session's config.toml and the issued login's auth.json are placed.
func (s *Session) CodexHome() string { return filepath.Join(s.Home, ".codex") }

// Env is the pod's whole environment, built from the allow-list: the
// sandbox's own BaseEnv — the node's package stores among them, the same
// for every session — then those of jobEnv's variables AllowedEnv or
// ToolEnvPrefix admits — with CODEX_HOME moved under the HOME
// (CodexHome) — never a HeldOutEnv one.
// Nothing comes from the node's own environment.
func (s *Session) Env(jobEnv []string) []string {
	tz := s.sb.Config.TZ
	if tz == "" {
		tz = "UTC"
	}
	out := []string{
		"PATH=" + DefaultPath,
		"HOME=" + s.Home,
		"LANG=C.UTF-8",
		"TZ=" + tz,
		"TERM=dumb",
		"CLAUDE_CONFIG_DIR=" + s.ClaudeConfigDir(),
		"MESH_ITEM=" + s.Label,
		"MESH_SESSION_KIND=" + s.Kind,
		"DISABLE_AUTOUPDATER=1",
	}
	out = append(out, s.sb.Config.Stores.Env()...)
	proxied := s.Proxied()
	for _, kv := range jobEnv {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !Allowed(name) {
			continue
		}
		if proxied && strings.HasPrefix(name, ToolEnvPrefix) {
			// the proxy holds every tool's secret on the host
			continue
		}
		if name == "CODEX_HOME" {
			kv = name + "=" + s.CodexHome()
		}
		out = append(out, kv)
	}
	return out
}

// SetEnv is env with each of vars (KEY=VALUE) set: a variable env has
// already is replaced where it stands, the rest appended in order.
func SetEnv(env, vars []string) []string {
	if len(vars) == 0 {
		return env
	}
	set := make(map[string]string, len(vars))
	for _, kv := range vars {
		name, _, _ := strings.Cut(kv, "=")
		set[name] = kv
	}
	out := make([]string, 0, len(env)+len(vars))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if v, ok := set[name]; ok {
			if v != "" {
				out = append(out, v)
				set[name] = ""
			}
			continue
		}
		out = append(out, kv)
	}
	for _, kv := range vars {
		name, _, _ := strings.Cut(kv, "=")
		if v := set[name]; v != "" {
			out = append(out, v)
			set[name] = ""
		}
	}
	return out
}

// Allowed says whether a job's variable may reach its pod.
func Allowed(name string) bool {
	for _, h := range HeldOutEnv {
		if name == h {
			return false
		}
	}
	if strings.HasPrefix(name, ToolEnvPrefix) && len(name) > len(ToolEnvPrefix) {
		return true
	}
	for _, a := range AllowedEnv {
		if name == a {
			return true
		}
	}
	return false
}

// Mounts are the pod's only host paths, each at its own path: the
// workspace and the session's HOME read-write, the shared git common
// directory of a workspace that is a worktree read-write (mesh-specs ADR
// 017, so its `.git` file resolves in the pod and its commits land in the
// clone's object store), and the checkout's hooks directory, when it has
// one, read-only. Under that common dir, `<common>/worktrees` is mounted
// again read-only and the workspace's own admin entry,
// `<common>/worktrees/<id>` (GitDir), read-write over it — the only
// writable path under `.git/worktrees`, which its commits need (HEAD,
// index, logs). Every other item's work tree is absent in the pod, so
// its entry's gitdir pointer dangles there and a `git worktree prune`
// would delete the live entry on the host; read-only, it cannot. A full
// clone mounts no common dir; nothing else of the clone — its parent,
// its work tree, another item's worktree — is ever mounted. The node's
// package store and cache are mounted read-write, each at its own path
// (Stores, ADR 017 §3). podman applies them shallowest target first
// (agentArgs), so the deeper mount wins. A session confined to an org
// runs on CheckedMounts, which refuses any of these outside its root.
func (s *Session) Mounts(workspace string) []Mount {
	named := s.namedMounts(workspace)
	m := make([]Mount, len(named))
	for i, n := range named {
		m[i] = n.Mount
	}
	return m
}

// namedMounts are Mounts, each with what it is.
func (s *Session) namedMounts(workspace string) []namedMount {
	m := []namedMount{
		{Mount{Source: workspace, Target: workspace}, "workspace"},
		{Mount{Source: s.Home, Target: s.Home}, "home"},
	}
	// A workspace whose common dir cannot be read is mounted as a full
	// clone would be: logged, and today's list, rather than no session.
	if common, err := GitCommonDir(workspace); err != nil {
		s.sb.logf("sandbox: %v; mounting the workspace alone", err)
	} else if common != "" {
		m = append(m, namedMount{Mount{Source: common, Target: common}, "git common dir"})
		for _, w := range s.worktreeMounts(workspace, common) {
			m = append(m, namedMount{w, "git dir"})
		}
	}
	for _, d := range s.sb.Config.Stores.dirs() {
		m = append(m, namedMount{Mount{Source: d, Target: d}, "package store"})
	}
	// the checkout's hooks, which its core.hooksPath names (ADR-024)
	if s.hooks != "" {
		m = append(m, namedMount{Mount{Source: s.hooks, Target: s.hooks, ReadOnly: true}, "hooks"})
	}
	// the sessionproxy's socket directory, the only way to its servers
	s.mu.Lock()
	dirs := append([]string(nil), s.proxyDirs...)
	s.mu.Unlock()
	for _, d := range dirs {
		m = append(m, namedMount{Mount{Source: d, Target: d}, "mcp proxy"})
	}
	return m
}

// worktreeMounts are the mounts over a worktree's common dir:
// `<common>/worktrees` read-only and the workspace's own admin entry
// read-write. An own entry that cannot be read, or is not under
// `<common>/worktrees`, leaves the whole of `.git/worktrees` read-only
// — logged; the session's commits then fail rather than a sibling's
// entry being pruned.
func (s *Session) worktreeMounts(workspace, common string) []Mount {
	wts := filepath.Join(common, "worktrees")
	m := []Mount{{Source: wts, Target: wts, ReadOnly: true}}
	own, err := GitDir(workspace)
	if err != nil {
		s.sb.logf("sandbox: %v; .git/worktrees mounted read-only whole", err)
		return m
	}
	if filepath.Dir(own) != wts {
		s.sb.logf("sandbox: git dir of %s is %s, not under %s; .git/worktrees mounted read-only whole", workspace, own, wts)
		return m
	}
	return append(m, Mount{Source: own, Target: own})
}

// RunSpec is what runs in a session's pod.
type RunSpec struct {
	Workspace string
	// Env is the job's environment; Env filters it (Session.Env).
	Env []string
	// Vars are the node's own variables for this run — never the job's,
	// never a secret: a served validation's HEAD_SHA and BASE_SHA — set
	// past the allow-list, last, each replacing any of its name (SetEnv).
	Vars []string
	// Cache, set, is a validation's scratch and shared cache, mounted
	// beside the session's own and named in its env (ValidationCache).
	Cache   *ValidationCache
	Command []string
	Timeout time.Duration
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// pidsExhausted is what a process says when it cannot fork past the
// pids cap.
var pidsExhausted = regexp.MustCompile(`(?i)fork: (retry: )?resource temporarily unavailable|cannot fork|pthread_create failed|EAGAIN.*(fork|clone|spawn)`)

// Run runs the command to its end in a container of the session's pod.
// A session whose Start started sidecars runs every Run in that pod,
// beside them, each in a container of its own (RunName), and keeps the
// pod for the next — Close removes it: a validation's steps are one Run
// each, and every step must reach the services, not the first alone
// (runsheet's integration suite skipped every database test on dl360
// from its second step on, 2026-10-28). Otherwise it runs in the pod
// Start made, else a new one, and removes the pod. A cancelled ctx
// returns ctx.Err(); a kill by a limit a *LimitError; any other non-zero
// exit an *ExitError.
func (s *Session) Run(ctx context.Context, spec RunSpec) error {
	cfg := s.sb.Config
	// every mount is checked before a pod is made: a refused one starts
	// nothing (CheckedMounts)
	mounts, err := s.CheckedMounts(spec.Workspace)
	if err != nil {
		return err
	}
	s.mu.Lock()
	started := s.started
	s.started = nil
	services := s.services
	if services != nil {
		s.runs++
	}
	n := s.runs
	s.mu.Unlock()
	var pod Pod
	name := ""
	switch {
	case services != nil:
		// the pod with the session's sidecars: kept for the next Run, its
		// container named for this one (RunName)
		pod, name = *services, RunName(*services, n)
	case started != nil:
		pod = *started
	default:
		if pod, err = s.createPod(ctx); err != nil {
			return err
		}
	}
	if name == "" {
		name = AgentName(pod)
		defer s.remove(ctx, pod)
	}

	env := s.Env(spec.Env)
	if c := spec.Cache; c != nil {
		if err := c.ensure(); err != nil {
			return err
		}
		mounts, env = append(mounts, c.Mounts()...), c.Env(env)
	}
	env = SetEnv(env, spec.Vars)
	timeout := runtimeTimeout(spec.Timeout)
	tail := &tailBuffer{max: 8 << 10}
	stderr := io.Writer(tail)
	if spec.Stderr != nil {
		stderr = io.MultiWriter(spec.Stderr, tail)
	}
	start := time.Now()
	exit, err := s.sb.Runtime.RunAgent(ctx, pod, AgentSpec{
		Name:           name,
		Image:          cfg.Image,
		Workdir:        spec.Workspace,
		Mounts:         mounts,
		Env:            env,
		Command:        spec.Command,
		Limits:         s.limits(),
		TimeoutSeconds: timeout,
		Stdin:          spec.Stdin,
		Stdout:         spec.Stdout,
		Stderr:         stderr,
	})
	if ctx.Err() != nil {
		if services != nil {
			// killing podman's client leaves the step's container
			// running beside the services: removed here, as the pod
			// of a Run without services is (s.remove)
			s.removeRun(ctx, name)
		}
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("sandbox: run: %w", err)
	}
	return classify(exit, time.Since(start), spec.Timeout, s.limits(), tail.String())
}

// Exec runs a short command — a preflight's step — in the session's pod
// beside where its agent will run: the pod Start made with its services,
// else one made now and kept for the next Run or StartDetached, with the
// same mounts, environment (Session.Env of spec.Env, then spec.Vars) and
// limits as the agent, in a container of its own (ExecName). The pod is never removed
// here. A cancelled ctx returns ctx.Err(); a kill by a limit a
// *LimitError; any other non-zero exit an *ExitError.
func (s *Session) Exec(ctx context.Context, spec RunSpec) error {
	cfg := s.sb.Config
	// a mount outside the session's org root refuses the exec before any
	// pod is made (CheckedMounts, mesh-cli ADR-034 Refusals)
	mounts, err := s.CheckedMounts(spec.Workspace)
	if err != nil {
		return err
	}
	s.mu.Lock()
	started := s.started
	s.execs++
	n := s.execs
	s.mu.Unlock()
	var pod Pod
	if started != nil {
		pod = *started
	} else {
		var err error
		if pod, err = s.createPod(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		s.started = &pod
		s.mu.Unlock()
	}
	tail := &tailBuffer{max: 8 << 10}
	stderr := io.Writer(tail)
	if spec.Stderr != nil {
		stderr = io.MultiWriter(spec.Stderr, tail)
	}
	start := time.Now()
	exit, err := s.sb.Runtime.RunAgent(ctx, pod, AgentSpec{
		Name:           ExecName(pod, n),
		Image:          cfg.Image,
		Workdir:        spec.Workspace,
		Mounts:         mounts,
		Env:            SetEnv(s.Env(spec.Env), spec.Vars),
		Command:        spec.Command,
		Limits:         s.limits(),
		TimeoutSeconds: runtimeTimeout(spec.Timeout),
		Stdin:          spec.Stdin,
		Stdout:         spec.Stdout,
		Stderr:         stderr,
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("sandbox: exec: %w", err)
	}
	return classify(exit, time.Since(start), spec.Timeout, s.limits(), tail.String())
}

// removeRun removes a cancelled Run's container from the services pod
// it ran in, past ctx's end, so the step stops when its run is
// stopped and not when the pod is removed.
func (s *Session) removeRun(ctx context.Context, name string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RemoveTimeout)
	defer cancel()
	if err := s.sb.Runtime.RemoveContainer(rctx, name); err != nil {
		s.sb.logf("sandbox: session %s: remove stopped run %s: %s", s.Label, name, oneLine(err.Error()))
	}
}

// ExecName is the container name of the session's nth Exec in a pod.
func ExecName(pod Pod, n int) string { return fmt.Sprintf("%s-exec-%d", pod.Name, n) }

// RunName is the container name of the session's nth Run in its
// services pod, which every Run shares.
func RunName(pod Pod, n int) string { return fmt.Sprintf("%s-run-%d", pod.Name, n) }

// AgentName is the name of the agent's container in a pod.
func AgentName(pod Pod) string { return pod.Name + "-agent" }

// runtimeTimeout is the runtime's --timeout backstop for a session of
// timeout, in seconds: TimeoutGrace past it; 0 for none.
func runtimeTimeout(timeout time.Duration) int {
	if timeout <= 0 {
		return 0
	}
	return int((timeout + TimeoutGrace) / time.Second)
}

// DetachedSpec is an agent started to outlive the node process: Run's
// spec with the streams as host files rather than pipes. StdinFile is
// the prompt; StdoutFile and StderrFile are where the agent writes
// (the session's `<workdir>.session/output.jsonl` and `stderr.log`).
// Each file's directory is mounted into the agent's container at its
// own path.
type DetachedSpec struct {
	Workspace  string
	Env        []string
	Command    []string
	Timeout    time.Duration
	StdinFile  string
	StdoutFile string
	StderrFile string
}

// DetachedAgent names a started detached agent: its pod and container,
// what a node after this one needs to find, wait on and stop it.
type DetachedAgent struct {
	Pod  Pod
	Name string
}

// StartDetached starts the agent in the session's pod — the one Start
// made with its services, else a new one — detached (Runtime.StartAgent)
// and returns at once. The pod stays the session's: Close removes it
// unless the session is Detached first. A start that fails removes the
// pod.
func (s *Session) StartDetached(ctx context.Context, spec DetachedSpec) (DetachedAgent, error) {
	cfg := s.sb.Config
	// every mount, the streams' directories (StartAgentArgs) among them,
	// is checked before a pod is made: a refused one starts nothing
	mounts, err := s.CheckedMounts(spec.Workspace)
	if err != nil {
		return DetachedAgent{}, err
	}
	for _, f := range []string{spec.StdinFile, spec.StdoutFile, spec.StderrFile} {
		if f == "" {
			continue
		}
		if err := s.confine.Check("stream dir", filepath.Dir(f)); err != nil {
			return DetachedAgent{}, err
		}
	}
	s.mu.Lock()
	started := s.started
	s.started = nil
	s.mu.Unlock()
	var pod Pod
	if started != nil {
		pod = *started
	} else {
		var err error
		if pod, err = s.createPod(ctx); err != nil {
			return DetachedAgent{}, err
		}
	}
	name := AgentName(pod)
	if err := s.sb.Runtime.StartAgent(ctx, pod, AgentSpec{
		Name:           name,
		Image:          cfg.Image,
		Workdir:        spec.Workspace,
		Mounts:         mounts,
		Env:            s.Env(spec.Env),
		Command:        spec.Command,
		Limits:         s.limits(),
		TimeoutSeconds: runtimeTimeout(spec.Timeout),
		StdinFile:      spec.StdinFile,
		StdoutFile:     spec.StdoutFile,
		StderrFile:     spec.StderrFile,
	}); err != nil {
		s.remove(ctx, pod)
		return DetachedAgent{}, fmt.Errorf("sandbox: start: %w", err)
	}
	return DetachedAgent{Pod: pod, Name: name}, nil
}

// Ended is a detached agent's end as the session's error, as Run
// classifies an attached one: nil, a *LimitError or an *ExitError.
// elapsed is how long it ran, timeout the session's own, stderr the
// tail of what it wrote there (for the pids heuristic).
func (s *Session) Ended(exit Exit, elapsed, timeout time.Duration, stderr string) error {
	return classify(exit, elapsed, timeout, s.limits(), stderr)
}

// Detach leaves the session's pods and HOME to outlive this process:
// Close after it removes nothing. A node that detaches has recorded
// the pod and HOME, so the next one can Reattach them.
func (s *Session) Detach() {
	s.mu.Lock()
	s.detached = true
	s.mu.Unlock()
}

// Runtime is the runtime the session's pods run on.
func (s *Session) Runtime() Runtime { return s.sb.Runtime }

// Pods are the session's pods still standing.
func (s *Session) Pods() []Pod {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Pod(nil), s.pods...)
}

// WithStores is sb for sessions that install against st in place of
// the node's Stores: the same runtime, image, limits, network and
// TempDir, its sessions mounting st's dirs and naming them in their env
// (Session.Mounts, Session.Env). A node serving several orgs opens each
// org's sessions on its own org's store (mesh-cli ADR-034), so no
// session mounts or names another org's pnpm store or npm cache. A nil
// sb is nil.
func (sb *Sandbox) WithStores(st Stores) *Sandbox {
	if sb == nil {
		return nil
	}
	sb.openSessions()
	c := *sb
	c.Config.Stores = st
	return &c
}

// Reattach is the session an earlier node process detached: its pod and
// HOME, whose Close removes both as an opened session's does.
func (sb *Sandbox) Reattach(pod Pod, home, label string) *Session {
	return &Session{sb: sb, Home: home, Label: label, name: pod.Name, image: sb.Config.Image, pods: []Pod{pod}}
}

// ReattachHome is Reattach for a session whose pod is gone but whose
// HOME is not: its Close removes the HOME alone, and a pod it makes
// (Run) is named after name as the session's next one.
func (sb *Sandbox) ReattachHome(name, home, label string) *Session {
	return &Session{sb: sb, Home: home, Label: label, name: name, image: sb.Config.Image}
}

// WaitAgent polls the named agent's state every interval until it is
// no longer running, and returns how it ended. A cancelled ctx returns
// ctx.Err(); a container the runtime no longer has, ErrNoAgent.
func WaitAgent(ctx context.Context, rt Runtime, name string, every time.Duration) (Exit, error) {
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		st, err := rt.AgentState(ctx, name)
		if err != nil {
			if ctx.Err() != nil {
				return Exit{}, ctx.Err()
			}
			return Exit{}, err
		}
		if !st.Running && st.Status != "created" {
			return st.Exit, nil
		}
		select {
		case <-ctx.Done():
			return Exit{}, ctx.Err()
		case <-t.C:
		}
	}
}

// createPod makes the session's next pod and its network rules, kept
// for Close until it is removed.
func (s *Session) createPod(ctx context.Context) (Pod, error) {
	cfg := s.sb.Config
	s.mu.Lock()
	name := fmt.Sprintf("%s-%d", s.name, len(s.pods))
	labels := map[string]string{SessionLabel: s.Label, "mesh.kind": s.Kind}
	if p := s.pin; p != nil {
		name = p.podName(len(s.pods))
		for k, v := range p.labels {
			labels[k] = v
		}
	}
	s.mu.Unlock()
	pod, err := s.sb.Runtime.CreatePod(ctx, PodSpec{
		Name:    name,
		Labels:  labels,
		Image:   cfg.Image,
		Network: cfg.Network,
	})
	if err != nil {
		return Pod{}, fmt.Errorf("sandbox: pod: %w", err)
	}
	s.mu.Lock()
	s.pods = append(s.pods, pod)
	s.mu.Unlock()
	return pod, nil
}

// classify turns the agent's exit into the session's error: nil, a
// *LimitError, or an *ExitError.
func classify(exit Exit, elapsed, timeout time.Duration, l Limits, stderr string) error {
	switch {
	case exit.Code == 0 && !exit.OOMKilled:
		return nil
	case exit.OOMKilled:
		return &LimitError{Limit: "memory", Value: l.Memory}
	case exit.Code == 137 && timeout > 0 && elapsed >= timeout:
		return &LimitError{Limit: "timeout", Value: timeout.String()}
	case exit.Code == 137:
		// SIGKILL the node did not send: the kernel's, for memory
		return &LimitError{Limit: "memory", Value: l.Memory}
	case l.Pids > 0 && pidsExhausted.MatchString(stderr):
		return &LimitError{Limit: "pids", Value: fmt.Sprint(l.Pids)}
	}
	return &ExitError{Code: exit.Code}
}

func (s *Session) remove(ctx context.Context, pod Pod) {
	_ = s.removePod(ctx, pod)
}

// RemoveTries, RemoveTimeout and RemoveBackoff bound one pod's removal:
// each try may take RemoveTimeout — a store busy with dozens of pods
// took past the old 30 seconds to remove one (`podman rm: signal:
// killed`, dl360 2026-10-28) — and a failed one is tried again after
// RemoveBackoff, up to RemoveTries in all. The pod's containers are
// killed first (Podman.Remove), so what it held is given back at once
// even while its removal waits.
var (
	RemoveTries   = 3
	RemoveTimeout = 2 * time.Minute
	RemoveBackoff = 5 * time.Second
)

// removePod removes pod, tried again as RemoveTries says, and drops it
// from the session's pods once it is gone; the last try's error when
// none worked. It runs past ctx's end: a cancelled run's pod is removed
// all the same.
func (s *Session) removePod(ctx context.Context, pod Pod) error {
	var err error
	for try := range max(RemoveTries, 1) {
		if try > 0 {
			time.Sleep(RemoveBackoff)
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RemoveTimeout)
		err = s.sb.Runtime.Remove(rctx, pod)
		cancel()
		if err == nil {
			s.mu.Lock()
			for i, p := range s.pods {
				if p == pod {
					s.pods = append(s.pods[:i], s.pods[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
			return nil
		}
	}
	return err
}

// Close removes any pod still standing — its containers killed first,
// then removed with their anonymous volumes, each pod tried again when
// its removal fails (removePod) — and the session's HOME, and returns
// every removal that failed, joined. A pod removed is the session's no
// more, so a second Close removes only what the first could not.
//
// A Detached session's Close removes nothing: its agent runs on. Either
// way the session leaves the sandbox's open set (Sandbox.Live).
func (s *Session) Close() error {
	s.mu.Lock()
	pods := append([]Pod(nil), s.pods...)
	detached := s.detached
	s.mu.Unlock()
	set := s.sb.openSessions()
	set.mu.Lock()
	delete(set.m, s)
	set.mu.Unlock()
	// the CPUs go back whatever happens to the pods: a detached agent is
	// the next process's to count, and a pod whose removal fails is
	// killed first (removePod)
	s.release()
	if detached {
		return nil
	}
	var errs []error
	for _, p := range pods {
		if err := s.removePod(context.Background(), p); err != nil {
			errs = append(errs, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.sb.RemoveTree(ctx, s.Home); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// CloseLine is the log line of a session whose Close failed.
func CloseLine(label string, err error) string {
	return fmt.Sprintf("sandbox: session %s: close: %s", label, strings.ReplaceAll(err.Error(), "\n", "; "))
}

// CloseAndLog is Close with its error logged through the sandbox's
// Logf as CloseLine, for a caller with nothing else to do with it.
func (s *Session) CloseAndLog() {
	if err := s.Close(); err != nil {
		s.sb.logf("%s", CloseLine(s.Label, err))
	}
}

// RemoveTree removes a dir a pod wrote in — a session's HOME, a
// validation's scratch — whatever the agent left in it: a plain
// RemoveAll; failing that, the tree made writable (Go's module cache is
// 0555 directories of 0444 files) and RemoveAll again; failing that, the
// runtime's removal inside its user namespace, for files the pod's
// mapped users own. The error names every step that failed; a dir
// already gone is nil.
func (sb *Sandbox) RemoveTree(ctx context.Context, home string) error {
	if home == "" {
		return nil
	}
	err := removeAll(home)
	if err == nil {
		return nil
	}
	makeWritable(home)
	err2 := removeAll(home)
	if err2 == nil {
		return nil
	}
	if err3 := sb.Runtime.RemoveTree(ctx, home); err3 != nil {
		return fmt.Errorf("remove %s: %w; in the user namespace: %w", home, err2, err3)
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w; still there after the removal in the user namespace", home, err2)
	}
	return nil
}

// removeAll is os.RemoveAll; tests stand in a failing one, as files a
// subuid owns make it fail on a node.
var removeAll = os.RemoveAll

// makeWritable gives the owner write and search on every directory of
// the tree it can reach, so its entries can be removed. What it cannot
// change (another uid's) is left for the user namespace.
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if d == nil || !d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			_ = os.Chmod(path, info.Mode().Perm()|0o700)
		}
		if err != nil {
			// A directory unreadable before the chmod: walk it again now.
			if path != root {
				makeWritable(path)
			}
			return filepath.SkipDir
		}
		return nil
	})
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}
