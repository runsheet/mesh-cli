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
// same reason as ClaudeCode.Args: MAX_ARG_STRLEN. MCP servers go in as
// `-c mcp_servers.<name>.…` overrides (CodexMCPArgs), never a header's
// value.
func (c *Codex) Args(job Job) []string {
	args := []string{"exec", "--json", "--sandbox", "workspace-write"}
	if job.Model != "" {
		args = append(args, "--model", job.Model)
	}
	mcp, _ := CodexMCPArgs(job.MCPServers)
	args = append(args, mcp...)
	args = append(args, c.ExtraArgs...)
	return append(args, "-")
}

// CodexMCPArgs turns MCP servers (the mcp.json shape Job.MCPServers
// holds) into Codex config overrides, in name order: an http server's
// `url`, and each of its headers through `env_http_headers`, which
// names a variable holding the value; a stdio server's `command`,
// `args` and `env`. env is those variables (KEY=VALUE), for the
// session's environment only, so a bearer token never reaches argv
// where `ps` shows it. A server Codex cannot take — neither shape, or
// a name that is not [A-Za-z0-9_-]+ — is skipped.
func CodexMCPArgs(servers map[string]any) (args, env []string) {
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		srv, ok := servers[name].(map[string]any)
		if !ok || !bareKey(name) {
			continue
		}
		key := "mcp_servers." + name
		if url, _ := srv["url"].(string); url != "" {
			args = append(args, "-c", key+".url="+tomlString(url))
			headers := stringMap(srv["headers"])
			if len(headers) == 0 {
				continue
			}
			hnames := sortedKeys(headers)
			refs := make([]string, 0, len(hnames))
			for _, h := range hnames {
				v := fmt.Sprintf("MESH_CODEX_MCP_%d_TOKEN", len(env))
				env = append(env, v+"="+headers[h])
				refs = append(refs, tomlString(h)+"="+tomlString(v))
			}
			args = append(args, "-c", key+".env_http_headers={"+strings.Join(refs, ",")+"}")
			continue
		}
		if cmd, _ := srv["command"].(string); cmd != "" {
			args = append(args, "-c", key+".command="+tomlString(cmd))
			if list := stringList(srv["args"]); len(list) > 0 {
				quoted := make([]string, len(list))
				for i, a := range list {
					quoted[i] = tomlString(a)
				}
				args = append(args, "-c", key+".args=["+strings.Join(quoted, ",")+"]")
			}
			if vars := stringMap(srv["env"]); len(vars) > 0 {
				kv := make([]string, 0, len(vars))
				for _, k := range sortedKeys(vars) {
					kv = append(kv, tomlString(k)+"="+tomlString(vars[k]))
				}
				args = append(args, "-c", key+".env={"+strings.Join(kv, ",")+"}")
			}
		}
	}
	return args, env
}

// tomlString quotes s as a TOML basic string: JSON's escapes are
// TOML's.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// bareKey says name can be a segment of a `-c` dotted path: codex-cli
// 0.157.0 splits the path on every dot and takes no quoted segment.
func bareKey(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return s != ""
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

// codexHome is one session's temporary CODEX_HOME.
type codexHome struct {
	dir     string
	env     []string
	secrets []string
	// login is the plan account's auth.json the copy came from, and
	// orig what it held: Codex refreshes a plan's tokens in place, so a
	// changed copy is written back (the refresh token rotates).
	login string
	orig  []byte
}

// prepareHome makes the session's CODEX_HOME: a copy of the plan
// account's auth.json, or for an API-key account nothing on disk and
// the key in the environment only. codex-cli 0.157.0's exec reads the
// key from CODEX_API_KEY — with only OPENAI_API_KEY set it sends no
// credential and gets a 401 — so both are set.
func (c *Codex) prepareHome() (*codexHome, error) {
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
	h := &codexHome{dir: dir, env: []string{"CODEX_HOME=" + dir}}
	fail := func(err error) (*codexHome, error) {
		os.RemoveAll(dir)
		return nil, err
	}
	if c.Account.IsAPIKey() {
		key, err := c.Account.apiKey()
		if err != nil {
			return fail(err)
		}
		h.env = append(h.env, "CODEX_API_KEY="+key, "OPENAI_API_KEY="+key)
		h.secrets = []string{key}
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

// close writes a refreshed plan login back to its home and removes the
// temporary one.
func (h *codexHome) close() error {
	defer os.RemoveAll(h.dir)
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
	home, err := c.prepareHome()
	if err != nil {
		return Result{}, fmt.Errorf("harness codex: %s", Redact(err.Error(), job.Secrets...))
	}
	defer func() {
		if err := home.close(); err != nil && c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "harness codex: write back the refreshed login of %q: %v\n", c.Account.Name, err)
		}
	}()
	_, mcpEnv := CodexMCPArgs(job.MCPServers)
	secrets := append(append([]string{}, job.Secrets...), home.secrets...)
	for _, kv := range mcpEnv {
		_, v, _ := strings.Cut(kv, "=")
		secrets = append(secrets, v)
	}
	env := job
	env.Unset = append(append([]string{}, job.Unset...), codexInherited...)
	env.Env = append(append(append([]string{}, job.Env...), home.env...), mcpEnv...)

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
	if c.Stderr != nil {
		cmd.Stderr = io.MultiWriter(&redactWriter{w: c.Stderr, secrets: secrets}, &errBuf)
	} else {
		cmd.Stderr = &errBuf
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("harness codex: start: %w", err)
	}
	rawOut := &capBuffer{max: 64 << 10}
	var sessionID, failure string
	res := ParseCodexStream(io.TeeReader(stdout, rawOut), job.Model, func(ev CodexEvent) {
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
	waitErr := cmd.Wait()
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
