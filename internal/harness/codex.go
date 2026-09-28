package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/tools"
)

// Codex runs OpenAI's Codex CLI, `codex exec --json`, in the workspace
// (verified against codex-cli 0.157.0; docs/codex-events.md). Each Run
// gets a fresh temporary CODEX_HOME holding only the account's
// credential, removed when the session ends.
//
// Codex has no budget flag: Job.MaxBudgetUSD is not enforced by the
// CLI. The session's cost is known only from its usage at the end
// (CodexPrices), so a cap has to be held by the caller — the Timeout,
// and the platform's caps on the account.
type Codex struct {
	// Binary is the codex executable ("codex" on PATH by default).
	Binary string
	// ExtraArgs are appended to every invocation, before the prompt.
	ExtraArgs []string
	// Stderr receives the process's stderr (nil discards it).
	Stderr io.Writer
	// Account is the OpenAI account the session runs on. The zero
	// account is the operator's own login: the auth.json in $CODEX_HOME,
	// else ~/.codex.
	Account CodexAccount
	// HomeRoot is where the temporary CODEX_HOMEs are made; empty is
	// <user cache dir>/mesh/codex. Not /tmp: Codex refuses to put its
	// helper binaries under /tmp.
	HomeRoot string
	// WorkRoot is the node's work root (node.WorkRoot()); when set, Run
	// keeps the session's whole event stream and stderr under
	// WorkRoot/kept-sessions (KeepSessionOutput, openKeptSession) and
	// sweeps directories older than that on every run
	// (SweepKeptSessions). Empty keeps nothing: the stream lives only in
	// the 64KB in-memory buffer, as before.
	WorkRoot string
}

// CodexAccount is one OpenAI account Codex sessions run on: a
// ChatGPT-plan login (Home, where `codex login` wrote its auth.json) or
// an API key (APIKeyEnv names the variable holding it, APIKeyFile the
// file). A leading ~ in a path is the operator's home.
type CodexAccount struct {
	Name       string
	Home       string
	APIKeyEnv  string
	APIKeyFile string
}

// IsAPIKey says the account runs on an API key rather than a plan login.
func (a CodexAccount) IsAPIKey() bool { return a.APIKeyEnv != "" || a.APIKeyFile != "" }

// apiKey reads the account's key. The error never carries the key.
func (a CodexAccount) apiKey() (string, error) {
	var key string
	switch {
	case a.APIKeyEnv != "":
		key = strings.TrimSpace(os.Getenv(a.APIKeyEnv))
		if key == "" {
			return "", fmt.Errorf("codex account %q: $%s is empty", a.Name, a.APIKeyEnv)
		}
	default:
		p, err := expandTilde(a.APIKeyFile)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("codex account %q: api key file: %w", a.Name, err)
		}
		key = strings.TrimSpace(string(data))
		if key == "" {
			return "", fmt.Errorf("codex account %q: api key file %s is empty", a.Name, p)
		}
	}
	return key, nil
}

// loginHome is where the account's auth.json lives: Home, or for the
// zero account the operator's own CODEX_HOME (~/.codex by default).
func (a CodexAccount) loginHome() (string, error) {
	if a.Home != "" {
		return expandTilde(a.Home)
	}
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func expandTilde(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p, nil
}

// codexInherited are the operator's own Codex variables, never passed
// to a session: its account is the one in its temporary home.
var codexInherited = []string{"CODEX_HOME", "CODEX_API_KEY", "OPENAI_API_KEY"}

// Name implements Harness.
func (c *Codex) Name() string { return "codex" }

// Args builds the command line for a job (exposed for tests).
//
// `--full-auto` is gone from codex-cli 0.157.0 (it was `--sandbox
// workspace-write` with on-request approval, and exec never asks), so
// the session is sandboxed to its workspace with `--sandbox
// workspace-write`. The prompt is not here but on stdin (`-`), for the
// same reason as ClaudeCode.Args: MAX_ARG_STRLEN. MCP servers are not
// here either: they go in the session's CODEX_HOME/config.toml
// (codexConfig), as ADR 006 has it.
func (c *Codex) Args(job Job) []string {
	args := []string{"exec", "--json", "--sandbox", "workspace-write"}
	if job.Model != "" {
		args = append(args, "--model", job.Model)
	}
	args = append(args, c.ExtraArgs...)
	return append(args, "-")
}

// CodexMCPConfig renders MCP servers (the mcp.json shape Job.MCPServers
// holds) as Codex's `[mcp_servers.<name>]` tables, in name order, and
// the environment the session needs for them (mesh-specs ADR 006: a
// secret is in the environment only, the config holds its name). An
// http server is its `url` and each header through `env_http_headers`,
// naming a variable MESH_CODEX_MCP_<n>_TOKEN that holds the value; a
// stdio server is its `command`, `args`, and each `env` entry set in
// the session's environment under its own name and passed through by
// `env_vars`. Every table approves its tools (tools.CodexApproval):
// `codex exec` refuses an unapproved MCP call under workspace-write. A
// server with neither url nor command is skipped.
//
// Codex expands no `${VAR}`: a value holding one (Claude Code's
// rendering of a console tool's secret, tools.ClaudeCode) is resolved
// from jobEnv, the job's own environment, and a reference jobEnv does
// not set is an error — never passed through for Codex to send as a
// literal header.
func CodexMCPConfig(servers map[string]any, jobEnv []string) (toml string, env []string, err error) {
	toml, env, _, err = codexMCP(servers, jobEnv)
	return toml, env, err
}

// codexMCP is CodexMCPConfig, and for each http server the variable
// each of its headers is read from (server → header → variable): what
// the preflight looks up in the session's environment.
func codexMCP(servers map[string]any, jobEnv []string) (toml string, env []string, headerVars map[string]map[string]string, err error) {
	headerVars = map[string]map[string]string{}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	vars := map[string]string{}
	for _, kv := range jobEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	var b strings.Builder
	for _, name := range names {
		srv, ok := servers[name].(map[string]any)
		if !ok {
			continue
		}
		resolve := func(what, v string) (string, error) {
			out, missing := expandRefs(v, vars)
			if missing != "" {
				return "", fmt.Errorf("mcp server %s: %s names ${%s}, which the session's environment does not set", name, what, missing)
			}
			return out, nil
		}
		url, _ := srv["url"].(string)
		cmd, _ := srv["command"].(string)
		if url == "" && cmd == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", tomlKey(name))
		if url != "" {
			if url, err = resolve("its url", url); err != nil {
				return "", nil, nil, err
			}
			fmt.Fprintf(&b, "url = %s\n", tomlString(url))
			headers := stringMap(srv["headers"])
			refs := make([]string, 0, len(headers))
			headerVars[name] = map[string]string{}
			for _, h := range sortedKeys(headers) {
				val, err := resolve("header "+h, headers[h])
				if err != nil {
					return "", nil, nil, err
				}
				v := fmt.Sprintf("MESH_CODEX_MCP_%d_TOKEN", len(env))
				env = append(env, v+"="+val)
				headerVars[name][h] = v
				refs = append(refs, tomlString(h)+" = "+tomlString(v))
			}
			if len(refs) > 0 {
				fmt.Fprintf(&b, "env_http_headers = { %s }\n", strings.Join(refs, ", "))
			}
			fmt.Fprintf(&b, "default_tools_approval_mode = %s\n", tomlString(tools.CodexApproval))
			continue
		}
		if cmd, err = resolve("its command", cmd); err != nil {
			return "", nil, nil, err
		}
		fmt.Fprintf(&b, "command = %s\n", tomlString(cmd))
		if list := stringList(srv["args"]); len(list) > 0 {
			args := make([]string, len(list))
			for i, a := range list {
				if args[i], err = resolve("an arg", a); err != nil {
					return "", nil, nil, err
				}
			}
			fmt.Fprintf(&b, "args = %s\n", tomlList(args))
		}
		if vs := stringMap(srv["env"]); len(vs) > 0 {
			keys := sortedKeys(vs)
			for _, k := range keys {
				val, err := resolve("env "+k, vs[k])
				if err != nil {
					return "", nil, nil, err
				}
				env = append(env, k+"="+val)
			}
			fmt.Fprintf(&b, "env_vars = %s\n", tomlList(keys))
		}
		fmt.Fprintf(&b, "default_tools_approval_mode = %s\n", tomlString(tools.CodexApproval))
	}
	return b.String(), env, headerVars, nil
}

// envRef is a `${NAME}` reference, as Claude Code expands them.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandRefs is s with every ${NAME} replaced by vars[NAME], and the
// first name vars does not hold ("" when every one resolved).
func expandRefs(s string, vars map[string]string) (out, missing string) {
	out = envRef.ReplaceAllStringFunc(s, func(ref string) string {
		name := ref[2 : len(ref)-1]
		v, ok := vars[name]
		if !ok && missing == "" {
			missing = name
		}
		return v
	})
	return out, missing
}

// codexOwnServers is job.MCPServers less its console tools (job.Tools):
// the servers CodexMCPConfig renders, the built-in among them.
func codexOwnServers(job Job) map[string]any {
	own := make(map[string]any, len(job.MCPServers))
	for name, srv := range job.MCPServers {
		own[name] = srv
	}
	for _, t := range job.Tools {
		delete(own, t.Name)
	}
	return own
}

// codexConfig is the session's config.toml and the environment it
// needs: the job's own servers (the built-in mesh-context) by
// CodexMCPConfig, then its console tools (job.Tools) by the Codex
// renderer, tools.CodexConfig — bearer_token_env_var and
// env_http_headers naming MESH_TOOL_* variables, never Claude Code's
// `${…}` rendering of them that job.MCPServers also carries.
func codexConfig(job Job) (string, []string, error) {
	cfg, env, err := CodexMCPConfig(codexOwnServers(job), job.Env)
	if err != nil {
		return "", nil, err
	}
	if len(job.Tools) == 0 {
		return cfg, env, nil
	}
	tcfg, tenv, err := tools.CodexConfig(job.Tools)
	if err != nil {
		return "", nil, err
	}
	if cfg != "" {
		cfg += "\n"
	}
	return cfg + tcfg, append(env, tenv...), nil
}

// tomlString quotes s as a TOML basic string: JSON's escapes are
// TOML's.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func tomlList(l []string) string {
	q := make([]string, len(l))
	for i, s := range l {
		q[i] = tomlString(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// tomlKey is a table-name segment: bare when TOML allows it, else
// quoted.
func tomlKey(s string) string {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return tomlString(s)
		}
	}
	if s == "" {
		return `""`
	}
	return s
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	switch m := v.(type) {
	case map[string]string:
		for k, s := range m {
			out[k] = s
		}
	case map[string]any:
		for k, x := range m {
			if s, ok := x.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// codexHome is one session's CODEX_HOME.
type codexHome struct {
	dir     string
	env     []string
	secrets []string
	// owned says the harness made dir and must remove it in close;
	// false for a home the job's own CODEX_HOME named, which its own
	// AccountSession owns and cleans up.
	owned bool
	// login is the plan account's auth.json the copy came from, and
	// orig what it held: Codex refreshes a plan's tokens in place, so a
	// changed copy is written back (the refresh token rotates). Empty
	// when the harness copied no login: an API-key account, or a home
	// the job named.
	login string
	orig  []byte
}

// prepareHome makes the session's CODEX_HOME. When job.Env carries
// CODEX_HOME=<dir> — a login the node already placed there, e.g. an
// issued credential's auth.json — that directory is the session's home
// as given: the harness writes only the MCP servers' config.toml into
// it, copies no login from Account, and sets no CODEX_HOME of its own
// (job.Env already has it); close neither removes the directory nor
// writes a login back, since the session that made it owns it.
//
// Otherwise the harness makes a fresh temporary CODEX_HOME: the same
// config.toml, and a copy of the plan account's auth.json, or for an
// API-key account the key in the environment only. codex-cli 0.157.0's
// exec reads the key from CODEX_API_KEY — with only OPENAI_API_KEY set
// it sends no credential and gets a 401 — so both are set.
func (c *Codex) prepareHome(job Job) (*codexHome, error) {
	if dir := jobCodexHome(job.Env); dir != "" {
		h := &codexHome{dir: dir}
		cfg, env, err := codexConfig(job)
		if err != nil {
			return nil, err
		}
		if cfg != "" {
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
				return nil, err
			}
			h.env = append(h.env, env...)
			for _, kv := range env {
				_, v, _ := strings.Cut(kv, "=")
				h.secrets = append(h.secrets, v)
			}
		}
		return h, nil
	}

	root := c.HomeRoot
	if root == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(cache, "mesh", "codex")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "home-")
	if err != nil {
		return nil, err
	}
	h := &codexHome{dir: dir, owned: true, env: []string{"CODEX_HOME=" + dir}}
	fail := func(err error) (*codexHome, error) {
		os.RemoveAll(dir)
		return nil, err
	}
	cfg, env, err := codexConfig(job)
	if err != nil {
		return fail(err)
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
			return fail(err)
		}
		h.env = append(h.env, env...)
		for _, kv := range env {
			_, v, _ := strings.Cut(kv, "=")
			h.secrets = append(h.secrets, v)
		}
	}
	if c.Account.IsAPIKey() {
		key, err := c.Account.apiKey()
		if err != nil {
			return fail(err)
		}
		h.env = append(h.env, "CODEX_API_KEY="+key, "OPENAI_API_KEY="+key)
		h.secrets = append(h.secrets, key)
		return h, nil
	}
	login, err := c.Account.loginHome()
	if err != nil {
		return fail(err)
	}
	h.login = filepath.Join(login, "auth.json")
	h.orig, err = os.ReadFile(h.login)
	if err != nil {
		return fail(fmt.Errorf("codex account %q: no login in %s (run `CODEX_HOME=%s codex login`): %w", c.Account.Name, login, login, err))
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), h.orig, 0o600); err != nil {
		return fail(err)
	}
	return h, nil
}

// jobCodexHome is the CODEX_HOME job.Env names, or "".
func jobCodexHome(env []string) string {
	for _, kv := range env {
		if name, v, ok := strings.Cut(kv, "="); ok && name == "CODEX_HOME" && v != "" {
			return v
		}
	}
	return ""
}

// close writes a refreshed plan login back to its home, and removes the
// directory the harness made — never one the job named (owned false):
// its own AccountSession removes that.
func (h *codexHome) close() error {
	if h.owned {
		defer os.RemoveAll(h.dir)
	}
	if h.login == "" {
		return nil
	}
	now, err := os.ReadFile(filepath.Join(h.dir, "auth.json"))
	if err != nil || len(now) == 0 || bytes.Equal(now, h.orig) {
		return nil
	}
	tmp := h.login + ".mesh-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, now, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.login)
}

// codexSession is what a Codex process runs in, as Run and Preflight
// both build it — one place, so the two cannot drift: the prepared home,
// the whole environment (the node's own less job.Unset and the operator's
// Codex variables, then job.Env and the home's), and every secret to
// redact.
type codexSession struct {
	c       *Codex
	home    *codexHome
	environ []string
	secrets []string
}

func (c *Codex) session(job Job) (*codexSession, error) {
	home, err := c.prepareHome(job)
	if err != nil {
		return nil, fmt.Errorf("harness codex: %s", Redact(err.Error(), job.Secrets...))
	}
	env := job
	env.Unset = append(append([]string{}, job.Unset...), codexInherited...)
	env.Env = append(append([]string{}, job.Env...), home.env...)
	return &codexSession{c: c, home: home, environ: SessionEnviron(os.Environ(), env),
		secrets: append(append([]string{}, job.Secrets...), home.secrets...)}, nil
}

func (s *codexSession) close() {
	if err := s.home.close(); err != nil && s.c.Stderr != nil {
		fmt.Fprintf(s.c.Stderr, "harness codex: write back the refreshed login of %q: %v\n", s.c.Account.Name, err)
	}
}

// lookup is the value the session's environment gives name: the last
// one, as exec keeps it.
func (s *codexSession) lookup(name string) (string, bool) {
	v, ok := "", false
	for _, kv := range s.environ {
		if k, val, found := strings.Cut(kv, "="); found && k == name {
			v, ok = val, true
		}
	}
	return v, ok
}

// Run implements Harness, as ClaudeCode.Run: the workspace is the
// working dir, job.Env is appended to the environment, the prompt goes
// on stdin, and a Timeout or a cancelled ctx kills the session and
// returns the context's error. The stream is read by ParseCodexStream
// at job.Model. A turn.failed is an error carrying Codex's message (an
// UnavailableError when it is a limit); a stream that ends without
// either turn event is a Result with Success false, as for Claude.
func (c *Codex) Run(ctx context.Context, job Job, onProgress func(Progress)) (Result, error) {
	bin := c.Binary
	if bin == "" {
		bin = "codex"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return Result{}, fmt.Errorf("harness codex: %w", err)
	}
	s, err := c.session(job)
	if err != nil {
		return Result{}, err
	}
	defer s.close()
	home, secrets := s.home, s.secrets

	var keptDir string
	var keptOut, keptErrFile *os.File
	if c.WorkRoot != "" {
		now := time.Now()
		if err := SweepKeptSessions(c.WorkRoot, now); err != nil && c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "harness codex: sweep kept sessions: %v\n", err)
		}
		var kErr error
		if keptDir, keptOut, keptErrFile, kErr = openKeptSession(c.WorkRoot, job, now); kErr != nil {
			if c.Stderr != nil {
				fmt.Fprintf(c.Stderr, "harness codex: kept session: %v\n", kErr)
			}
			keptDir, keptOut, keptErrFile = "", nil, nil
		} else {
			defer keptOut.Close()
			defer keptErrFile.Close()
		}
	}

	if job.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, c.Args(job)...)
	cmd.Dir = job.Workspace
	cmd.Env = s.environ
	cmd.Stdin = strings.NewReader(job.Prompt)
	cmd.WaitDelay = 5 * time.Second
	var errBuf bytes.Buffer
	stderrWriters := []io.Writer{&errBuf}
	if c.Stderr != nil {
		stderrWriters = append(stderrWriters, &redactWriter{w: c.Stderr, secrets: secrets})
	}
	if keptErrFile != nil {
		stderrWriters = append(stderrWriters, &redactWriter{w: keptErrFile, secrets: secrets})
	}
	cmd.Stderr = io.MultiWriter(stderrWriters...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("harness codex: start: %w", err)
	}
	rawOut := &capBuffer{max: 64 << 10}
	var stdoutTee io.Writer = rawOut
	if keptOut != nil {
		stdoutTee = io.MultiWriter(rawOut, &redactWriter{w: keptOut, secrets: secrets})
	}
	var sessionID, failure string
	res := ParseCodexStream(io.TeeReader(stdout, stdoutTee), job.Model, func(ev CodexEvent) {
		switch ev.Type {
		case "thread.started":
			sessionID = ev.ThreadID
			if onProgress != nil && sessionID != "" {
				onProgress(Progress{SessionID: sessionID})
			}
		case "item.completed":
			if onProgress != nil && ev.Item != nil && ev.Item.Type == "agent_message" {
				if t := strings.TrimSpace(ev.Item.Text); t != "" {
					onProgress(Progress{Text: t, SessionID: sessionID})
				}
			}
		case "turn.completed":
			if onProgress != nil && ev.Usage != nil {
				if p, ok := CodexPrices[job.Model]; ok {
					onProgress(Progress{CostUSD: p.Cost(*ev.Usage), SessionID: sessionID})
				}
			}
		case "turn.failed":
			if ev.Error != nil {
				failure = ev.Error.Message
			}
		}
	})
	res.KeptSessionDir = keptDir
	waitErr := cmd.Wait()
	// a tool's error text rides the usage report and the item's note
	for _, stat := range res.Tools {
		stat.FirstError = Redact(stat.FirstError, secrets...)
	}
	if res.RateLimit == nil {
		if rl, err := ParseCodexSessionLimits(home.dir); err == nil && rl != nil {
			res.RateLimit = rl
		}
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if failure != "" {
		msg := Redact(strings.TrimSpace(failure), secrets...)
		if msg == "" {
			msg = Redact(strings.TrimSpace(errBuf.String()), secrets...)
		}
		if isUnavailable(msg) {
			return res, &UnavailableError{Reason: msg}
		}
		return res, fmt.Errorf("harness codex: %s", msg)
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && res.Success && res.Text != "" {
			// the turn completed and said its piece; the exit is Codex's own
			res.Success = false
			return res, nil
		}
		msg := Redact(strings.TrimSpace(errBuf.String()), secrets...)
		if isUnavailable(msg) {
			return res, &UnavailableError{Reason: msg}
		}
		if out := Redact(strings.TrimSpace(rawOut.String()), secrets...); res.EmptyOutput && isUnavailable(out) {
			return res, &UnavailableError{Reason: out}
		}
		if msg == "" {
			msg = Redact(waitErr.Error(), secrets...)
		}
		return res, fmt.Errorf("harness codex: %s", msg)
	}
	return res, nil
}

// BuiltinMCP is the MCP server the node attaches to every session
// itself: the platform's context server (tools.Builtin).
const BuiltinMCP = "mesh-context"

// PreflightTimeout caps each step of a Codex preflight.
const PreflightTimeout = 10 * time.Second

// CodexPreflight is Codex{}.Preflight: `codex` on PATH, on the job's
// own CODEX_HOME or the operator's login.
func CodexPreflight(ctx context.Context, job Job) error {
	return (&Codex{}).Preflight(ctx, job)
}

// Preflight checks, before a Codex session starts, that the built-in
// context server (BuiltinMCP) will answer it: in the CODEX_HOME the
// session will use and with exactly the environment Run builds
// (codexSession), (1) `codex mcp get <builtin>` must succeed — Codex
// parses the config and sees the server — and (2) an MCP initialize
// and tools/list over streamable HTTP to the server's URL, with the
// header values the rendered config and that environment give Codex,
// must answer 2xx listing at least one tool, each step within
// PreflightTimeout. A job with no built-in has nothing to check. The
// error says why — `refuses auth: <status> <body>`, `unreachable:
// <error>`, a header variable missing or empty — and never carries a
// secret. It costs nothing and is no tool call of the session's.
func (c *Codex) Preflight(ctx context.Context, job Job) error {
	srv, ok := job.MCPServers[BuiltinMCP].(map[string]any)
	if !ok {
		return nil
	}
	url, _ := srv["url"].(string)
	if url == "" {
		return nil
	}
	bin := c.Binary
	if bin == "" {
		bin = "codex"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	s, err := c.session(job)
	if err != nil {
		return err
	}
	defer s.close()
	redact := func(msg string) string { return Redact(msg, s.secrets...) }

	// (1) Codex sees the server in the config it will read
	getCtx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(getCtx, bin, "mcp", "get", BuiltinMCP)
	cmd.Dir = job.Workspace
	cmd.Env = s.environ
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		why := strings.TrimSpace(string(out))
		if why == "" {
			why = err.Error()
		}
		return fmt.Errorf("codex mcp get %s: %s", BuiltinMCP, truncateRunes(redact(why), 200))
	}

	// (2) the server answers the headers Codex would send
	_, _, vars, err := codexMCP(codexOwnServers(job), job.Env)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	for _, h := range sortedKeys(vars[BuiltinMCP]) {
		name := vars[BuiltinMCP][h]
		v, found := s.lookup(name)
		switch {
		case !found:
			return fmt.Errorf("%s for %s's %s header is missing from the session's environment", name, BuiltinMCP, h)
		case strings.TrimSpace(v) == "":
			return fmt.Errorf("%s for %s's %s header is empty in the session's environment", name, BuiltinMCP, h)
		}
		headers[h] = v
	}
	listCtx, cancelList := context.WithTimeout(ctx, PreflightTimeout)
	defer cancelList()
	n, err := mcpListTools(listCtx, url, headers)
	if err != nil {
		return errors.New(redact(err.Error()))
	}
	if n == 0 {
		return fmt.Errorf("%s lists no tools", BuiltinMCP)
	}
	return nil
}

// mcpListTools is an MCP client's opening over streamable HTTP —
// initialize, notifications/initialized, tools/list — and the number of
// tools listed.
func mcpListTools(ctx context.Context, url string, headers map[string]string) (int, error) {
	client := &http.Client{}
	session := ""
	post := func(id int, method string, params any) (*mcpResponse, error) {
		msg := map[string]any{"jsonrpc": "2.0", "method": method}
		if id > 0 {
			msg["id"] = id
		}
		if params != nil {
			msg["params"] = params
		}
		body, _ := json.Marshal(msg)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("unreachable: %v", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("unreachable: %v", err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		snippet := truncateRunes(strings.TrimSpace(string(data)), 200)
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, fmt.Errorf("refuses auth: %s: %s", resp.Status, snippet)
		case resp.StatusCode < 200 || resp.StatusCode > 299:
			return nil, fmt.Errorf("%s: %s: %s", method, resp.Status, snippet)
		}
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			session = sid
		}
		if id == 0 {
			return nil, nil
		}
		r, err := readMCPResponse(data, id)
		if err != nil {
			return nil, fmt.Errorf("%s: %v: %s", method, err, snippet)
		}
		if r.Error != nil {
			return nil, fmt.Errorf("%s: error %d: %s", method, r.Error.Code, truncateRunes(r.Error.Message, 200))
		}
		return r, nil
	}
	if _, err := post(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mesh-preflight", "version": "1"},
	}); err != nil {
		return 0, err
	}
	if _, err := post(0, "notifications/initialized", nil); err != nil {
		return 0, err
	}
	r, err := post(2, "tools/list", map[string]any{})
	if err != nil {
		return 0, err
	}
	var result struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &result); err != nil {
		return 0, fmt.Errorf("tools/list: %v", err)
	}
	return len(result.Tools), nil
}

type mcpResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// readMCPResponse finds the response to id in a body that is one JSON
// message or an event stream of them.
func readMCPResponse(data []byte, id int) (*mcpResponse, error) {
	want := strconv.Itoa(id)
	try := func(b []byte) *mcpResponse {
		var r mcpResponse
		if json.Unmarshal(b, &r) == nil && string(r.ID) == want {
			return &r
		}
		return nil
	}
	if r := try(bytes.TrimSpace(data)); r != nil {
		return r, nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		if d, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			if r := try([]byte(strings.TrimSpace(d))); r != nil {
				return r, nil
			}
		}
	}
	return nil, errors.New("no response")
}

// truncateRunes is s cut to n runes.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
