package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
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
	// Account is the OpenAI account the session runs on when the job
	// places no CODEX_HOME of its own. The zero account is no account: a
	// job with neither is refused (ErrNoIssuedCredential) — the
	// operator's own login, $CODEX_HOME or ~/.codex, is never read.
	Account CodexAccount
	// HomeRoot is where the temporary CODEX_HOMEs are made; empty is
	// <user cache dir>/mesh/codex. Not /tmp: Codex refuses to put its
	// helper binaries under /tmp.
	HomeRoot string
	// WorkRoot is the node's work root (node.WorkRoot()); when set, Run
	// keeps the session's whole event stream and stderr under
	// WorkRoot/kept-sessions (KeepSessionOutput, openKeptSession) — or
	// the job's own KeptRoot, its org's, when it names one — and sweeps
	// directories older than that there on every run
	// (SweepKeptSessions). Both empty keeps nothing: the stream lives
	// only in the 64KB in-memory buffer, as before.
	WorkRoot string
	// Probe is the command the preflight's sandbox probe runs inside
	// Codex's sandbox (SandboxProbe); nil is this executable's
	// `mesh sandbox-probe`, which binds a loopback socket.
	Probe []string
	// Tokens keeps the session's runsheet token file, in its
	// CODEX_HOME, fresh when the job's runsheet is the relay
	// (MeshContextRelay, mesh-cli ADR-029).
	Tokens TokenKeeper
	// Sandbox, when set, runs every session in its own pod (ADR 007), as
	// ClaudeCode.Sandbox does: the node image's codex, the workspace and
	// a temporary HOME the only host paths, the session's CODEX_HOME
	// inside that HOME (Session.CodexHome), the environment the job's
	// allow-listed variables by name. Nil is `sandbox: off`: codex on
	// the host, Binary.
	Sandbox *sandbox.Sandbox
	// PodProbe is the command the preflight's sandbox probe runs inside
	// Codex's sandbox in the pod; nil is PodProbeLoopback, node binding
	// a loopback socket (the image has no mesh).
	PodProbe []string
	// SandboxBinary is the agent's executable in the pod: empty is
	// `codex` on the node image's PATH; else a path inside the pod, such
	// as one in the workspace (an e2e's stub agent).
	SandboxBinary string
	// DetachSandboxed runs a sandboxed Outlive job's codex detached in
	// its pod (runDetached), as ClaudeCode.DetachSandboxed does: the
	// node's restart leaves it running and the next process adopts it
	// (Attach, mesh-cli ADR-009). Codex on the host never outlives the
	// node.
	DetachSandboxed bool
}

// CodexProvider is an OpenAI-compatible endpoint a Codex session talks
// to instead of OpenAI's own: a vLLM or SGLang server in front of a
// model the operator runs (Kimi, Qwen, a local Llama), or any hosted
// service that speaks the same wire. It is named in node.yaml's
// `codex.providers` and chosen by an account, so one node may run real
// Codex and a local model side by side (adr-015).
//
// The key travels as OPENAI_API_KEY, which the harness already exports
// for an api-key account, so `env_key` is always that and a provider
// needs no variable of its own. A server that checks no key still wants
// one set: give the account an api_key_env holding any non-empty value.
type CodexProvider struct {
	// Name is the provider's name in node.yaml, and the name Codex
	// knows it by; empty is OpenAI itself.
	Name string
	// BaseURL is the endpoint including the path a client expects —
	// vLLM and SGLang serve OpenAI's API under /v1.
	BaseURL string
	// WireAPI is "chat" (OpenAI's chat completions, what vLLM and
	// SGLang serve) or "responses" (OpenAI's own); empty is "chat".
	WireAPI string
	// Prices are the list prices per million tokens of the models this
	// provider serves, by model name. A model with no price records no
	// cost, which is right for one that costs nothing to run and wrong
	// for a hosted one — so name the price when there is a bill.
	Prices map[string]CodexPrice
}

// IsZero says the session runs on OpenAI itself.
func (p CodexProvider) IsZero() bool { return p.Name == "" || p.BaseURL == "" }

// TOML is the provider as the session's config.toml declares it, and
// empty for OpenAI itself.
func (p CodexProvider) TOML() string {
	if p.IsZero() {
		return ""
	}
	wire := p.WireAPI
	if wire == "" {
		wire = "chat"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "model_provider = %s\n\n", tomlString(p.Name))
	fmt.Fprintf(&b, "[model_providers.%s]\n", tomlKey(p.Name))
	fmt.Fprintf(&b, "name = %s\n", tomlString(p.Name))
	fmt.Fprintf(&b, "base_url = %s\n", tomlString(p.BaseURL))
	fmt.Fprintf(&b, "wire_api = %s\n", tomlString(wire))
	fmt.Fprintf(&b, "env_key = %s\n", tomlString("OPENAI_API_KEY"))
	return b.String()
}

// Price is a model's list price: a provider's own table alone, never
// OpenAI's, because a name it happens to share ("gpt-6-sol" served
// locally) does not cost what OpenAI charges for it. The zero provider
// is OpenAI, priced at CodexPrices.
func (p CodexProvider) Price(model string) (CodexPrice, bool) {
	if !p.IsZero() {
		pr, ok := p.Prices[model]
		return pr, ok
	}
	pr, ok := CodexPrices[model]
	return pr, ok
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
	// Provider is the OpenAI-compatible endpoint the session talks to;
	// the zero provider is OpenAI itself. Only an api-key account names
	// one: a plan login's auth.json would never reach the endpoint.
	Provider CodexProvider
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

// loginHome is where the account's auth.json lives: Home. There is no
// default: the operator's own CODEX_HOME (~/.codex) is never a
// session's login (ErrNoIssuedCredential).
func (a CodexAccount) loginHome() (string, error) {
	if a.Home == "" {
		return "", ErrNoIssuedCredential
	}
	return expandTilde(a.Home)
}

// ErrNoIssuedCredential is a Codex job with no login to run on: no
// CODEX_HOME the node placed for an issued credential, and no account
// of the harness's own. The operator's own login is never the fallback
// (goal a-session-never-runs-on-the-machines-own-login).
var ErrNoIssuedCredential = errors.New("codex: no issued credential on the session; refusing the operator's own login")

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
//
// The sandboxed commands get the network (SandboxNetwork,
// `sandbox_workspace_write.network_access=true`): without it Codex's
// seccomp filter refuses socket(), so no test that binds loopback
// (httptest, a local server for npm test) and no `gh` can run, and an
// evaluation could only guess (ADR 011, amendment 2026-09-29). Every
// Codex session today is an evaluation.
//
// Job.Resume (a retry turn, goal
// a-failed-check-returns-to-the-session-that-built-it) is `codex exec
// [OPTIONS] resume <id> -`: exec's own flags (--json, --sandbox, -c,
// --model, ExtraArgs) precede the subcommand, as its
// `override_usage` has it, `codex exec [OPTIONS] <COMMAND> [ARGS]`,
// and resume's own PROMPT positional takes `-` for stdin exactly as
// exec's top-level one does (codex-rs/exec/src/cli.rs's Command enum,
// ResumeArgsRaw; verified via context7, /openai/codex, 2026-10-13). A
// job with no Resume is unchanged.
func (c *Codex) Args(job Job) []string {
	args := []string{"exec", "--json", "--sandbox", "workspace-write"}
	args = append(args, SandboxNetwork...)
	if job.Model != "" {
		args = append(args, "--model", job.Model)
	}
	args = append(args, c.ExtraArgs...)
	if job.Resume != "" {
		args = append(args, "resume", job.Resume)
	}
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
// needs: the job's own servers (the built-in runsheet) by
// CodexMCPConfig, then its console tools (job.Tools) by the Codex
// renderer, tools.CodexConfig — bearer_token_env_var and
// env_http_headers naming MESH_TOOL_* variables, never Claude Code's
// `${…}` rendering of them that job.MCPServers also carries. The
// provider, when it is not OpenAI itself, is declared first, and on its
// own makes the file worth writing: a session with no MCP server still
// needs its endpoint (adr-015).
func codexConfig(job Job, prov CodexProvider) (string, []string, error) {
	cfg, env, err := CodexMCPConfig(codexOwnServers(job), job.Env)
	if err != nil {
		return "", nil, err
	}
	if head := prov.TOML(); head != "" {
		if cfg != "" {
			head += "\n"
		}
		cfg = head + cfg
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
	// tokenFile is the runsheet relay's token file in dir, and
	// releaseToken stops keeping it (placeMeshContext); close removes it.
	tokenFile    string
	releaseToken func(remove bool)
}

// prepareHome makes the session's CODEX_HOME. When job.Env carries
// CODEX_HOME=<dir> — a login the node already placed there, e.g. an
// issued credential's auth.json — that directory is the session's home
// as given: the harness writes only the MCP servers' config.toml into
// it — with no provider, since an issued credential is OpenAI's own and
// Account's provider is not its endpoint — copies no login from Account, and sets no CODEX_HOME of its own
// (job.Env already has it); close neither removes the directory nor
// writes a login back, since the session that made it owns it.
//
// Otherwise the harness makes a fresh temporary CODEX_HOME: the same
// config.toml with Account's provider declared first (written even with
// no MCP server, for the endpoint), and a copy of the plan account's auth.json, or for an
// API-key account the key in the environment only. codex-cli 0.157.0's
// exec reads the key from CODEX_API_KEY — with only OPENAI_API_KEY set
// it sends no credential and gets a 401 — so both are set.
func (c *Codex) prepareHome(job Job) (*codexHome, error) {
	if dir := jobCodexHome(job.Env); dir != "" {
		h := &codexHome{dir: dir}
		var err error
		if job, err = h.placeToken(job, c.Tokens); err != nil {
			return nil, err
		}
		// the job's home is an issued credential's (PrepareCodexAccount):
		// an OpenAI account that names no provider, so it gets the zero
		// one, never this harness's own account's endpoint (adr-015)
		cfg, env, err := codexConfig(job, CodexProvider{})
		if err != nil {
			h.release(true)
			return nil, err
		}
		if cfg != "" {
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
				h.release(true)
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

	if !c.Account.IsAPIKey() && c.Account.Home == "" {
		// no placed home and no account: never the operator's own login
		return nil, ErrNoIssuedCredential
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
		h.release(true)
		os.RemoveAll(dir)
		return nil, err
	}
	if job, err = h.placeToken(job, c.Tokens); err != nil {
		return fail(err)
	}
	cfg, env, err := codexConfig(job, c.Account.Provider)
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
// placeToken puts the job's runsheet relay on a token file in the
// home, kept by keeper, and returns the job as config.toml renders it.
func (h *codexHome) placeToken(job Job, keeper TokenKeeper) (Job, error) {
	job, path, release, err := placeMeshContext(job, keeper, h.dir, false, "")
	if err != nil {
		return job, fmt.Errorf("runsheet token: %w", err)
	}
	h.tokenFile, h.releaseToken = path, release
	return job, nil
}

func (h *codexHome) release(remove bool) {
	if h.releaseToken != nil {
		h.releaseToken(remove)
		h.releaseToken = nil
	}
}

func (h *codexHome) close() error {
	// the session has ended: its token file goes with it (a detached
	// one's is released, kept, by runDetached instead)
	h.release(true)
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
// at job.Model, priced by the account's provider (CodexProvider.Price:
// its own table alone, OpenAI's CodexPrices only for OpenAI itself). A turn.failed is an error carrying Codex's message (an
// UnavailableError when it is a limit); a stream that ends without
// either turn event is a Result with Success false, as for Claude.
func (c *Codex) Run(ctx context.Context, job Job, onProgress func(Progress)) (Result, error) {
	if c.Sandbox != nil || job.Session != nil {
		// the image's codex in the session's pod: no host binary looked up
		return c.runSandboxed(ctx, job, onProgress)
	}
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

	kept := c.keepSession(job, secrets)
	defer kept.close()

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
	cmd.Stderr = kept.stderr(&errBuf, c.Stderr)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("harness codex: start: %w", err)
	}
	rawOut := &capBuffer{max: 64 << 10}
	res, failure := c.readStream(io.TeeReader(stdout, kept.stdout(rawOut)), job, onProgress)
	res.KeptSessionDir = kept.dir
	waitErr := cmd.Wait()
	kept.flush()
	return codexEnd(ctx, res, failure, waitErr, &errBuf, rawOut, secrets, home.dir)
}

// keptCodex is a session's kept stream and stderr (KeepSessionOutput),
// or nothing when the harness keeps none.
type keptCodex struct {
	dir     string
	out     *os.File
	errFile *os.File
	secrets []string
	// redactors hold back a partial secret until the stream ends
	redactors []*streamRedactor
}

// keepSession opens the session's kept files under the job's kept dir
// (keptDir: its KeptRoot, else WorkRoot/kept-sessions), sweeping older
// ones there first; a failure keeps nothing and says so.
func (c *Codex) keepSession(job Job, secrets []string) *keptCodex {
	k := &keptCodex{secrets: secrets}
	kept := keptDir(c.WorkRoot, job)
	if kept == "" {
		return k
	}
	now := time.Now()
	if err := SweepKeptSessions(kept, now); err != nil && c.Stderr != nil {
		fmt.Fprintf(c.Stderr, "harness codex: sweep kept sessions: %v\n", err)
	}
	var err error
	if k.dir, k.out, k.errFile, err = openKeptSession(c.WorkRoot, job, now); err != nil {
		if c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "harness codex: kept session: %v\n", err)
		}
		return &keptCodex{secrets: secrets}
	}
	return k
}

func (k *keptCodex) close() {
	k.flush()
	if k.out != nil {
		k.out.Close()
	}
	if k.errFile != nil {
		k.errFile.Close()
	}
}

// flush writes what the redactors held back once the process is done.
func (k *keptCodex) flush() {
	for _, r := range k.redactors {
		_ = r.Flush()
	}
	k.redactors = nil
}

func (k *keptCodex) redacting(w io.Writer) io.Writer {
	r := newStreamRedactor(w, k.secrets)
	k.redactors = append(k.redactors, r)
	return r
}

// stderr is where the process's stderr goes: buf, and log and the kept
// file with the secrets redacted, a secret split across writes too.
func (k *keptCodex) stderr(buf *bytes.Buffer, log io.Writer) io.Writer {
	ws := []io.Writer{buf}
	if log != nil {
		ws = append(ws, k.redacting(log))
	}
	if k.errFile != nil {
		ws = append(ws, k.redacting(k.errFile))
	}
	return io.MultiWriter(ws...)
}

// stdout is where the stream is copied as it is read: raw, and the
// kept file redacted.
func (k *keptCodex) stdout(raw *capBuffer) io.Writer {
	if k.out == nil {
		return raw
	}
	return io.MultiWriter(raw, k.redacting(k.out))
}

// readStream parses a session's stream, passing what it says on, and
// returns the turn.failed message, if any.
func (c *Codex) readStream(stdout io.Reader, job Job, onProgress func(Progress)) (Result, string) {
	return c.readStreamWatch(stdout, job, onProgress, nil)
}

// readStreamWatch is readStream with after, when set, called after
// every event: a detached session's follower saves its offset there.
func (c *Codex) readStreamWatch(stdout io.Reader, job Job, onProgress func(Progress), after func(CodexEvent)) (Result, string) {
	var sessionID, failure string
	price := c.Account.Provider.Price
	res := parseCodexStream(stdout, job.Model, price, func(ev CodexEvent) {
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
				if p, ok := price(job.Model); ok {
					onProgress(Progress{CostUSD: p.Cost(*ev.Usage), SessionID: sessionID})
				}
			}
		case "turn.failed":
			if ev.Error != nil {
				failure = ev.Error.Message
			}
		}
		if after != nil {
			after(ev)
		}
	})
	return res, failure
}

// codexEnd is how a session ended, on the host or in a pod: its Result,
// or the error its turn, its exit, its stderr or its stdout said, every
// secret redacted. homeDir is the CODEX_HOME its session limits are
// read from when the stream carried none.
func codexEnd(ctx context.Context, res Result, failure string, waitErr error, errBuf *bytes.Buffer, rawOut *capBuffer, secrets []string, homeDir string) (Result, error) {
	// a tool's error text rides the usage report and the item's note
	for _, stat := range res.Tools {
		stat.FirstError = Redact(stat.FirstError, secrets...)
	}
	if res.RateLimit == nil {
		if rl, err := ParseCodexSessionLimits(homeDir); err == nil && rl != nil {
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
		var sbExit *sandbox.ExitError
		if (errors.As(waitErr, &exitErr) || errors.As(waitErr, &sbExit)) && res.Success && res.Text != "" {
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
const BuiltinMCP = tools.Builtin

// PreflightTimeout caps each step of a Codex preflight.
const PreflightTimeout = 10 * time.Second

// CodexPreflight is Codex{}.Preflight: `codex` on PATH, on the job's
// own CODEX_HOME (none is ErrNoIssuedCredential).
func CodexPreflight(ctx context.Context, job Job) error {
	return (&Codex{}).Preflight(ctx, job)
}

// SandboxNetwork is the config override that gives a workspace-write
// sandbox's commands the network, loopback included (codex-cli 0.157:
// `sandbox_workspace_write.network_access`, default false). Args and
// SandboxProbe both carry it, so the probe cannot drift from the run.
var SandboxNetwork = []string{"-c", "sandbox_workspace_write.network_access=true"}

// SandboxProbe is the command line of the preflight's sandbox probe
// (codex-cli 0.157: `codex sandbox [COMMAND]...`, the platform's own
// sandbox — bubblewrap on Linux, Seatbelt on macOS) under the same
// policy `codex exec` runs the session in (Args: `--sandbox
// workspace-write` and SandboxNetwork), running probe. Left alone,
// `codex sandbox` defaults to read-only
// (codex-rs/cli/src/debug_sandbox.rs, rust-v0.157.0); the legacy
// `sandbox_mode` override is the one it honours instead. probe must
// bind a loopback socket (`mesh sandbox-probe`, ProbeLoopback): `true`
// passes a sandbox whose seccomp filter refuses socket(), where every
// test that listens fails.
func SandboxProbe(probe []string) []string {
	args := []string{"sandbox", "-c", `sandbox_mode="workspace-write"`}
	args = append(args, SandboxNetwork...)
	args = append(args, "--")
	return append(args, probe...)
}

// ProbeLoopback is `mesh sandbox-probe`: it listens on 127.0.0.1:0,
// writes the address it got to w and closes it. The error is the
// listen's, e.g. `listen tcp 127.0.0.1:0: socket: operation not
// permitted` in a sandbox without the network.
func ProbeLoopback(w io.Writer) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer l.Close()
	_, err = fmt.Fprintln(w, l.Addr().String())
	return err
}

// probeCommand is c.Probe, else this executable's `sandbox-probe`.
func (c *Codex) probeCommand() []string {
	if len(c.Probe) > 0 {
		return c.Probe
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "mesh"
	} else if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return []string{exe, "sandbox-probe"}
}

// Preflight checks, before a Codex session starts, that the built-in
// context server (BuiltinMCP) will answer it and that Codex's own
// sandbox starts (sandboxProbe): in the CODEX_HOME the
// session will use and with exactly the environment Run builds
// (codexSession), (1) `codex mcp get <builtin>` must succeed — Codex
// parses the config and sees the server — and (2) an MCP initialize
// and tools/list over streamable HTTP to the server's URL, with the
// header values the rendered config and that environment give Codex,
// must answer 2xx listing at least one tool, each step within
// PreflightTimeout. A job with no built-in skips (1) and (2). Then (3)
// `codex sandbox -c sandbox_mode="workspace-write" -c
// sandbox_workspace_write.network_access=true -- mesh sandbox-probe`
// (SandboxProbe, the session's own policy, on a command that binds
// 127.0.0.1:0) in the workspace, in the same home and environment, must
// exit 0 with no `bwrap:` on its stderr within PreflightTimeout — else
// `sandbox: <the bwrap line, else the socket error, else its output,
// 200 chars>`: every command the session ran would fail the same way
// (`bwrap: loopback: Failed RTM_NEWADDR` on an Ubuntu that restricts
// unprivileged user namespaces; README, Codex on a node), or every test
// that listens would (`socket: operation not permitted`). The error says why — `refuses auth: <status> <body>`,
// `unreachable: <error>`, a header variable missing or empty,
// `sandbox: …` — and never carries a secret. It costs nothing and is no
// tool call of the session's.
//
// With a Sandbox, or on a job carrying its Session, every step runs in
// the session's pod instead (preflightSandboxed): the image's codex, the
// CODEX_HOME inside the session's HOME, the env the session gets.
func (c *Codex) Preflight(ctx context.Context, job Job) error {
	if c.Sandbox != nil || job.Session != nil {
		// the image's codex in the session's pod: no host binary looked up
		return c.preflightSandboxed(ctx, job)
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
	if err := c.preflightContext(ctx, bin, job, s); err != nil {
		return err
	}
	return c.sandboxProbe(ctx, bin, job, s)
}

// sandboxProbe is the preflight's (3): Codex's sandbox on a command
// that binds a loopback socket.
func (c *Codex) sandboxProbe(ctx context.Context, bin string, job Job, s *codexSession) error {
	probeCtx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, bin, SandboxProbe(c.probeCommand())...)
	cmd.Dir = job.Workspace
	cmd.Env = s.environ
	cmd.WaitDelay = time.Second
	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	err := cmd.Run()
	why := strings.TrimSpace(stderr.String())
	if err == nil && !strings.Contains(why, "bwrap:") {
		return nil
	}
	if line := bwrapLine(why); line != "" {
		why = line
	} else if line := socketLine(why + "\n" + stdout.String()); line != "" {
		why = line
	} else if why == "" {
		why = strings.TrimSpace(stdout.String())
	}
	if why == "" {
		why = err.Error()
	}
	return fmt.Errorf("sandbox: %s", truncateRunes(Redact(why, s.secrets...), 200))
}

// preflightContext is the preflight's (1) and (2): the built-in context
// server, when the job has one.
func (c *Codex) preflightContext(ctx context.Context, bin string, job Job, s *codexSession) error {
	srv, ok := job.MCPServers[BuiltinMCP].(map[string]any)
	if !ok {
		return nil
	}
	url, _ := srv["url"].(string)
	relayURL, relay := meshContextRelay(job)
	if relay {
		url = relayURL
	}
	if url == "" {
		return nil
	}
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

	// (2) the server answers the headers Codex would send — through the
	// relay, the bearer its token file holds
	if relay {
		if s.home.tokenFile == "" {
			return fmt.Errorf("%s relays on no token file", BuiltinMCP)
		}
		tok, err := readTokenFile(s.home.tokenFile)
		if err != nil || tok == "" {
			return fmt.Errorf("%s's token file %s is missing or empty", BuiltinMCP, s.home.tokenFile)
		}
		listCtx, cancelList := context.WithTimeout(ctx, PreflightTimeout)
		defer cancelList()
		n, err := mcpListTools(listCtx, url, map[string]string{"Authorization": "Bearer " + tok})
		if err != nil {
			return errors.New(Redact(err.Error(), append(s.secrets, tok)...))
		}
		if n == 0 {
			return fmt.Errorf("%s lists no tools", BuiltinMCP)
		}
		return nil
	}
	return c.preflightHTTP(ctx, job, s, url)
}

// preflightHTTP is the preflight's (2) for an http built-in: an MCP
// client's opening to url with the header values the rendered config
// and the session's environment give Codex.
func (c *Codex) preflightHTTP(ctx context.Context, job Job, s *codexSession, url string) error {
	redact := func(msg string) string { return Redact(msg, s.secrets...) }
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
