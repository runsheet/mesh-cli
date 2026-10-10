package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
// the Codex keys, and the session's agent identity (ADR-024). Tool secrets (ADR 006) are allowed by
// ToolEnvPrefix.
var AllowedEnv = []string{
	"GH_TOKEN", "GH_CONFIG_DIR",
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY",
	"OPENAI_API_KEY", "CODEX_API_KEY",
	"MESH_ACCOUNT",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
}

// ToolEnvPrefix names a tool secret's variable (tools.EnvName).
const ToolEnvPrefix = "MESH_TOOL_"

// BaseEnv are the variables the sandbox sets itself for every session.
// DISABLE_AUTOUPDATER keeps claude from updating itself: the node image's
// own ENV says so too, but --unsetenv-all drops the image's variables.
var BaseEnv = []string{"PATH", "HOME", "LANG", "TZ", "TERM", "CLAUDE_CONFIG_DIR", "MESH_ITEM", "MESH_SESSION_KIND", "DISABLE_AUTOUPDATER"}

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
}

// Sandbox opens sessions on a runtime.
type Sandbox struct {
	Runtime Runtime
	Config  Config
	// TempDir is where temporary HOMEs are made ("" is os.TempDir).
	TempDir string
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
	home, err := os.MkdirTemp(s.TempDir, "mesh-home-")
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

// Env is the pod's whole environment, built from the allow-list: the
// sandbox's own BaseEnv, then those of jobEnv's variables AllowedEnv or
// ToolEnvPrefix admits — with the gh config dir moved under the HOME.
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
	for _, kv := range jobEnv {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !Allowed(name) {
			continue
		}
		if name == "GH_CONFIG_DIR" {
			kv = name + "=" + s.GHConfigDir()
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
// workspace and the session's HOME read-write, and the checkout's hooks
// directory, when it has one, read-only.
func (s *Session) Mounts(workspace string) []Mount {
	m := []Mount{
		{Source: workspace, Target: workspace},
		{Source: s.Home, Target: s.Home},
	}
	// the checkout's hooks, which its core.hooksPath names (ADR-024)
	if s.hooks != "" {
		m = append(m, Mount{Source: s.hooks, Target: s.hooks, ReadOnly: true})
	}
	return m
}

// RunSpec is what runs in a session's pod.
type RunSpec struct {
	Workspace string
	// Env is the job's environment; Env filters it (Session.Env).
	Env     []string
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
	name := pod.Name

	timeout := 0
	if spec.Timeout > 0 {
		timeout = int((spec.Timeout + TimeoutGrace) / time.Second)
	}
	tail := &tailBuffer{max: 8 << 10}
	stderr := io.Writer(tail)
	if spec.Stderr != nil {
		stderr = io.MultiWriter(spec.Stderr, tail)
	}
	start := time.Now()
	exit, err := s.sb.Runtime.RunAgent(ctx, pod, AgentSpec{
		Name:           name + "-agent",
		Image:          cfg.Image,
		Workdir:        spec.Workspace,
		Mounts:         s.Mounts(spec.Workspace),
		Env:            s.Env(spec.Env),
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

// createPod makes the session's next pod and its network rules, kept
// for Close until it is removed.
func (s *Session) createPod(ctx context.Context) (Pod, error) {
	cfg := s.sb.Config
	s.mu.Lock()
	name := fmt.Sprintf("%s-%d", s.name, len(s.pods))
	s.mu.Unlock()
	pod, err := s.sb.Runtime.CreatePod(ctx, PodSpec{
		Name:    name,
		Labels:  map[string]string{"mesh.session": s.Label, "mesh.kind": s.Kind},
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

// Close removes any pod still standing and the session's HOME.
func (s *Session) Close() error {
	s.mu.Lock()
	pods := append([]Pod(nil), s.pods...)
	s.mu.Unlock()
	var errs []error
	for _, p := range pods {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := s.sb.Runtime.Remove(ctx, p); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}
	if err := os.RemoveAll(s.Home); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
