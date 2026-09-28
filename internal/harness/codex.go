package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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
// (CodexMCPConfig), as ADR 006 has it.
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
// `env_vars`. A server with neither url nor command is skipped.
func CodexMCPConfig(servers map[string]any) (toml string, env []string) {
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		srv, ok := servers[name].(map[string]any)
		if !ok {
			continue
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
			fmt.Fprintf(&b, "url = %s\n", tomlString(url))
			headers := stringMap(srv["headers"])
			if len(headers) == 0 {
				continue
			}
			refs := make([]string, 0, len(headers))
			for _, h := range sortedKeys(headers) {
				v := fmt.Sprintf("MESH_CODEX_MCP_%d_TOKEN", len(env))
				env = append(env, v+"="+headers[h])
				refs = append(refs, tomlString(h)+" = "+tomlString(v))
			}
			fmt.Fprintf(&b, "env_http_headers = { %s }\n", strings.Join(refs, ", "))
			continue
		}
		fmt.Fprintf(&b, "command = %s\n", tomlString(cmd))
		if list := stringList(srv["args"]); len(list) > 0 {
			fmt.Fprintf(&b, "args = %s\n", tomlList(list))
		}
		if vars := stringMap(srv["env"]); len(vars) > 0 {
			keys := sortedKeys(vars)
			for _, k := range keys {
				env = append(env, k+"="+vars[k])
			}
			fmt.Fprintf(&b, "env_vars = %s\n", tomlList(keys))
		}
	}
	return b.String(), env
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
		if cfg, env := CodexMCPConfig(job.MCPServers); cfg != "" {
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
	if cfg, env := CodexMCPConfig(job.MCPServers); cfg != "" {
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
	home, err := c.prepareHome(job)
	if err != nil {
		return Result{}, fmt.Errorf("harness codex: %s", Redact(err.Error(), job.Secrets...))
	}
	defer func() {
		if err := home.close(); err != nil && c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "harness codex: write back the refreshed login of %q: %v\n", c.Account.Name, err)
		}
	}()
	secrets := append(append([]string{}, job.Secrets...), home.secrets...)
	env := job
	env.Unset = append(append([]string{}, job.Unset...), codexInherited...)
	env.Env = append(append([]string{}, job.Env...), home.env...)

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
	cmd.Env = SessionEnviron(os.Environ(), env)
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
