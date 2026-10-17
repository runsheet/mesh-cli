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
// job's, never the node's) that reach its pod: the item's GitHub token
// and gh config dir (ADR-001), the account's secret and label (adr-005),
// the Codex keys and CODEX_HOME, and the session's agent identity
// (ADR-024). Tool secrets (ADR 006) are allowed by ToolEnvPrefix. The
// gh config dir and CODEX_HOME are admitted by name only: Session.Env
// moves each under the session's HOME, so a host path never reaches the
// pod.
var AllowedEnv = []string{
	"GH_TOKEN", "GH_CONFIG_DIR",
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY",
	"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_HOME",
	"MESH_ACCOUNT",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
}

// ToolEnvPrefix names a tool secret's variable (tools.EnvName).
const ToolEnvPrefix = "MESH_TOOL_"

// BaseEnv are the variables the sandbox sets itself for every session.
// DISABLE_AUTOUPDATER keeps claude from updating itself: the node image's
// own ENV says so too, but --unsetenv-all drops the image's variables.
// The package variables are set only when the node names its Stores, the
// scratch and Go cache ones only for a run given a ValidationCache.
var BaseEnv = []string{"PATH", "HOME", "LANG", "TZ", "TERM", "CLAUDE_CONFIG_DIR", "MESH_ITEM", "MESH_SESSION_KIND", "DISABLE_AUTOUPDATER", "npm_config_store_dir", "pnpm_config_store_dir", "PNPM_HOME", "npm_config_cache", "TMPDIR", "GOTMPDIR", "GOCACHE"}

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
// the price of not mounting the cache root whole. Empty is none.
type ValidationCache struct {
	Scratch string
	GoCache string
	Blobs   string
}

// Mounts are the cache's mounts: the scratch and the Go cache
// read-write, the blob store read-only.
func (c ValidationCache) Mounts() []Mount {
	var m []Mount
	for _, d := range []struct {
		path string
		ro   bool
	}{{c.Scratch, false}, {c.GoCache, false}, {c.Blobs, true}} {
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
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := set[name]; !ok {
			out = append(out, kv)
		}
	}
	for _, name := range []string{"TMPDIR", "GOTMPDIR", "GOCACHE"} {
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
	// Stores are the node's package store and cache every session
	// installs against (ADR 017 §3).
	Stores Stores
}

// Sandbox opens sessions on a runtime.
type Sandbox struct {
	Runtime Runtime
	Config  Config
	// TempDir is where temporary HOMEs are made ("" is os.TempDir).
	TempDir string
	// Logf is where the sandbox says what it fell back on (nil is the
	// standard logger).
	Logf func(format string, args ...any)
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
	// hooks is the checkout's hooks directory, mounted beside it.
	hooks string
	// image is the image ref the session's agent runs, the Sandbox's
	// Config.Image when it was opened.
	image string
	// detached is a session whose agent outlives the node process: Close
	// leaves its pods and HOME to whoever Reattaches it.
	detached bool
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
	return &Session{sb: s, Home: home, Label: label, Kind: kind, hooks: git.HooksDir, image: s.Config.Image, name: "mesh-" + name + "-" + hex.EncodeToString(rnd[:])}, nil
}

// gitConfigValue quotes v for a .gitconfig line: `Mesh builder (mini)`
// keeps its parentheses, a quote or backslash is escaped, and a line
// break is dropped.
func gitConfigValue(v string) string {
	v = strings.NewReplacer("\r", "", "\n", "", `\`, `\\`, `"`, `\"`).Replace(v)
	return `"` + v + `"`
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

// GHConfigDir is the session's empty GH_CONFIG_DIR, under its HOME.
func (s *Session) GHConfigDir() string { return filepath.Join(s.Home, ".config", "gh") }

// CodexHome is the session's CODEX_HOME, under its HOME: where a Codex
// session's config.toml and the issued login's auth.json are placed.
func (s *Session) CodexHome() string { return filepath.Join(s.Home, ".codex") }

// Env is the pod's whole environment, built from the allow-list: the
// sandbox's own BaseEnv — the node's package stores among them, the same
// for every session — then those of jobEnv's variables AllowedEnv or
// ToolEnvPrefix admits — with the gh config dir and CODEX_HOME moved
// under the HOME (GHConfigDir, CodexHome).
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
	for _, kv := range jobEnv {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !Allowed(name) {
			continue
		}
		switch name {
		case "GH_CONFIG_DIR":
			kv = name + "=" + s.GHConfigDir()
		case "CODEX_HOME":
			kv = name + "=" + s.CodexHome()
		}
		out = append(out, kv)
	}
	return out
}

// Allowed says whether a job's variable may reach its pod.
func Allowed(name string) bool {
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
// (agentArgs), so the deeper mount wins.
func (s *Session) Mounts(workspace string) []Mount {
	m := []Mount{
		{Source: workspace, Target: workspace},
		{Source: s.Home, Target: s.Home},
	}
	// A workspace whose common dir cannot be read is mounted as a full
	// clone would be: logged, and today's list, rather than no session.
	if common, err := GitCommonDir(workspace); err != nil {
		s.sb.logf("sandbox: %v; mounting the workspace alone", err)
	} else if common != "" {
		m = append(m, Mount{Source: common, Target: common})
		m = append(m, s.worktreeMounts(workspace, common)...)
	}
	for _, d := range s.sb.Config.Stores.dirs() {
		m = append(m, Mount{Source: d, Target: d})
	}
	// the checkout's hooks, which its core.hooksPath names (ADR-024)
	if s.hooks != "" {
		m = append(m, Mount{Source: s.hooks, Target: s.hooks, ReadOnly: true})
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

// Run runs the command in the agent's container of the session's pod —
// the one Start made with its services, else a new one — to its end and
// removes the pod. A cancelled ctx returns ctx.Err(); a kill by a limit
// a *LimitError; any other non-zero exit an *ExitError.
func (s *Session) Run(ctx context.Context, spec RunSpec) error {
	cfg := s.sb.Config
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
			return err
		}
	}
	defer s.remove(ctx, pod)

	mounts, env := s.Mounts(spec.Workspace), s.Env(spec.Env)
	if c := spec.Cache; c != nil {
		if err := c.ensure(); err != nil {
			return err
		}
		mounts, env = append(mounts, c.Mounts()...), c.Env(env)
	}
	timeout := runtimeTimeout(spec.Timeout)
	tail := &tailBuffer{max: 8 << 10}
	stderr := io.Writer(tail)
	if spec.Stderr != nil {
		stderr = io.MultiWriter(spec.Stderr, tail)
	}
	start := time.Now()
	exit, err := s.sb.Runtime.RunAgent(ctx, pod, AgentSpec{
		Name:           AgentName(pod),
		Image:          cfg.Image,
		Workdir:        spec.Workspace,
		Mounts:         mounts,
		Env:            env,
		Command:        spec.Command,
		Limits:         cfg.Limits,
		TimeoutSeconds: timeout,
		Stdin:          spec.Stdin,
		Stdout:         spec.Stdout,
		Stderr:         stderr,
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("sandbox: run: %w", err)
	}
	return classify(exit, time.Since(start), spec.Timeout, cfg.Limits, tail.String())
}

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
		Mounts:         s.Mounts(spec.Workspace),
		Env:            s.Env(spec.Env),
		Command:        spec.Command,
		Limits:         cfg.Limits,
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
	return classify(exit, elapsed, timeout, s.sb.Config.Limits, stderr)
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
	s.mu.Unlock()
	pod, err := s.sb.Runtime.CreatePod(ctx, PodSpec{
		Name:    name,
		Labels:  map[string]string{SessionLabel: s.Label, "mesh.kind": s.Kind},
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
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.sb.Runtime.Remove(rctx, pod); err == nil {
		s.mu.Lock()
		for i, p := range s.pods {
			if p == pod {
				s.pods = append(s.pods[:i], s.pods[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}
}

// Close removes any pod still standing — its containers and their
// anonymous volumes with it — and the session's HOME, and returns every
// removal that failed, joined.
//
// A Detached session's Close removes nothing: its agent runs on.
func (s *Session) Close() error {
	s.mu.Lock()
	pods := append([]Pod(nil), s.pods...)
	detached := s.detached
	s.mu.Unlock()
	if detached {
		return nil
	}
	var errs []error
	for _, p := range pods {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := s.sb.Runtime.Remove(ctx, p); err != nil {
			errs = append(errs, err)
		}
		cancel()
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
