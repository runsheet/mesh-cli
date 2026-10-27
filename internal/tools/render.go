package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeCode returns the mcpServers of a Claude Code session — builtin
// (the runsheet server) and one entry per tool — and the environment the
// session needs for them. A remote tool is {type: http, url, headers}, a
// local one {command, args, env} for its runtime. Every secret is written
// as ${MESH_TOOL_<NAME>_<KEY>}, which Claude Code expands from its own
// environment, and its value is returned in env only. harness.MCPArgs of
// servers allows mcp__<name>__* for each.
func ClaudeCode(builtin map[string]any, tools []Tool) (servers map[string]any, env []string, err error) {
	reserved := map[string]bool{Builtin: true}
	for n := range builtin {
		reserved[n] = true
	}
	if err := check(tools, reserved); err != nil {
		return nil, nil, err
	}
	servers = make(map[string]any, len(builtin)+len(tools))
	for n, s := range builtin {
		servers[n] = s
	}
	for _, t := range tools {
		switch t.Transport {
		case Remote:
			headers := map[string]string{}
			for k, v := range t.Headers {
				headers[k] = v
			}
			for _, k := range sortedKeys(t.Secrets) {
				name := EnvName(t.Name, k)
				v := t.Secrets[k]
				if tok, ok := bearer(k, v); ok {
					headers[k] = "Bearer ${" + name + "}"
					env = append(env, name+"="+tok)
					continue
				}
				headers[k] = "${" + name + "}"
				env = append(env, name+"="+v)
			}
			s := map[string]any{"type": "http", "url": t.URL}
			if len(headers) > 0 {
				s["headers"] = headers
			}
			servers[t.Name] = s
		case Local:
			vars := map[string]string{}
			for k, v := range t.Env {
				vars[k] = v
			}
			for _, k := range sortedKeys(t.Secrets) {
				name := EnvName(t.Name, k)
				vars[k] = "${" + name + "}"
				env = append(env, name+"="+t.Secrets[k])
			}
			cmd, args := localArgs(t, envKeys(t))
			s := map[string]any{"command": cmd, "args": args}
			if len(vars) > 0 {
				s["env"] = vars
			}
			servers[t.Name] = s
		}
	}
	return servers, env, nil
}

// UpstreamSpec is what sessionproxy reaches one server on — a remote
// tool's URL and every header (plain and secret alike, since the proxy,
// never the agent, reads them), a local tool's launch by its runtime
// (npx -y <package> …, uvx, docker run), its env and
// secret env, or the built-in's URL alone (Transport ""). The node turns
// one into a sessionproxy.Upstream (Remote, Local or Runsheet); tools
// does not import that package, to stay the leaf its own doc comment
// says it is.
type UpstreamSpec struct {
	Transport string
	URL       string
	Headers   map[string]string
	Command   string
	Args      []string
	Env       map[string]string
	Secrets   map[string]string
}

// ProxiedServers is a session's mcpServers when it reaches every server
// through sessionproxy instead of directly (mesh-cli goal
// a-session-reaches-every-mcp-server-through-one-proxy): the built-in
// and every tool become one shim entry each, from shim(name) —
// harness.ProxiedServer in a pod, `mesh mcp-relay --socket/--server` on
// a host — which names a socket and a server, nothing else; alongside,
// the UpstreamSpec each is reached on, which does carry every secret.
// No MESH_TOOL_* and no secret value is in servers: there is nothing
// for the session's environment to hold, since a shim's argv is the
// socket and the name alone. Existing non-proxied rendering (ClaudeCode,
// CodexConfig) is unchanged; this is additive until the node switches
// over (item 4 of the same goal).
func ProxiedServers(builtinURL string, toolsIn []Tool, shim func(name string) map[string]any) (servers map[string]any, upstreams map[string]UpstreamSpec, err error) {
	if err := check(toolsIn, map[string]bool{Builtin: true}); err != nil {
		return nil, nil, err
	}
	servers = map[string]any{Builtin: shim(Builtin)}
	upstreams = map[string]UpstreamSpec{Builtin: {URL: builtinURL}}
	for _, t := range toolsIn {
		servers[t.Name] = shim(t.Name)
		spec := UpstreamSpec{Transport: t.Transport, URL: t.URL}
		switch t.Transport {
		case Remote:
			headers := make(map[string]string, len(t.Headers)+len(t.Secrets))
			for k, v := range t.Headers {
				headers[k] = v
			}
			for k, v := range t.Secrets {
				headers[k] = v
			}
			spec.Headers = headers
		case Local:
			env := make(map[string]string, len(t.Env))
			for k, v := range t.Env {
				env[k] = v
			}
			secrets := make(map[string]string, len(t.Secrets))
			for k, v := range t.Secrets {
				secrets[k] = v
			}
			spec.Env, spec.Secrets = env, secrets
			// the runtime's own launch, as ClaudeCode renders it: npx -y
			// <package>, uvx <package>, docker run -i --rm -e …
			spec.Command, spec.Args = localArgs(t, envKeys(t))
		}
		upstreams[t.Name] = spec
	}
	return servers, upstreams, nil
}

// WithMesh adds the Mesh server (Mesh) to a session's proxied servers
// and upstreams (runsheet ADR 069, step 4): the work tools a session
// calls about its own job — mesh_progress and the node's health and
// pause tools — answer there, on /mesh/mcp beside builtinURL's /mcp,
// reached with the same session bearer as the built-in. It is added
// only for a session whose bearer is its own mesh session's (DPoP-bound):
// /mesh/mcp serves apps and sessions alone and refuses the node's
// operator bearer. A tool of that name is left as it is (no server
// replaced); servers and upstreams are changed in place.
func WithMesh(servers map[string]any, upstreams map[string]UpstreamSpec, builtinURL string, shim func(name string) map[string]any) {
	u := MeshURL(builtinURL)
	if u == "" {
		return
	}
	if _, taken := upstreams[Mesh]; taken {
		return
	}
	if servers != nil {
		servers[Mesh] = shim(Mesh)
	}
	if upstreams != nil {
		upstreams[Mesh] = UpstreamSpec{URL: u}
	}
}

// CodexApproval is every rendered server's default_tools_approval_mode.
// `codex exec` runs with approval policy never, and under `--sandbox
// workspace-write` it refuses an MCP call that would need approval
// before it leaves the process ("MCP tool call requires approval, but
// approval policy is never", codex-cli 0.157.0): the node chose the
// session's servers, so their tools are approved up front
// (docs/codex-events.md, "Why the context tool failed in Codex").
const CodexApproval = "approve"

// CodexConfig is the config.toml WriteCodexConfig writes — one
// [mcp_servers.<name>] table per tool, each approved (CodexApproval) —
// and the environment the session needs for them. The text names
// variables, never values: a remote tool's bearer Authorization goes by
// bearer_token_env_var, any other secret header by env_http_headers, both
// under MESH_TOOL_<NAME>_<KEY>; a local tool's secret env goes by
// env_vars, which Codex forwards under the key itself, so the variable is
// the key — two tools giving one key different values is an error.
func CodexConfig(tools []Tool) (config string, env []string, err error) {
	if err := check(tools, map[string]bool{Builtin: true}); err != nil {
		return "", nil, err
	}
	var b bytes.Buffer
	local := map[string]string{}
	for i, t := range tools {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", t.Name)
		switch t.Transport {
		case Remote:
			fmt.Fprintf(&b, "url = %s\n", tomlString(t.URL))
			plain := map[string]string{}
			for k, v := range t.Headers {
				if _, secret := t.Secrets[k]; !secret {
					plain[k] = v
				}
			}
			byName := map[string]string{}
			for _, k := range sortedKeys(t.Secrets) {
				name := EnvName(t.Name, k)
				v := t.Secrets[k]
				if tok, ok := bearer(k, v); ok {
					fmt.Fprintf(&b, "bearer_token_env_var = %s\n", tomlString(name))
					env = append(env, name+"="+tok)
					continue
				}
				byName[k] = name
				env = append(env, name+"="+v)
			}
			if len(plain) > 0 {
				fmt.Fprintf(&b, "http_headers = %s\n", tomlTable(plain))
			}
			if len(byName) > 0 {
				fmt.Fprintf(&b, "env_http_headers = %s\n", tomlTable(byName))
			}
		case Local:
			cmd, args := localArgs(t, envKeys(t))
			fmt.Fprintf(&b, "command = %s\n", tomlString(cmd))
			fmt.Fprintf(&b, "args = %s\n", tomlArray(args))
			plain := map[string]string{}
			for k, v := range t.Env {
				if _, secret := t.Secrets[k]; !secret {
					plain[k] = v
				}
			}
			if len(plain) > 0 {
				fmt.Fprintf(&b, "env = %s\n", tomlTable(plain))
			}
			var names []string
			for _, k := range sortedKeys(t.Secrets) {
				v := t.Secrets[k]
				if prev, ok := local[k]; ok && prev != v {
					return "", nil, fmt.Errorf("tool %s: secret env %s differs from another tool's", t.Name, k)
				} else if !ok {
					local[k] = v
					env = append(env, k+"="+v)
				}
				names = append(names, k)
			}
			if len(names) > 0 {
				fmt.Fprintf(&b, "env_vars = %s\n", tomlArray(names))
			}
		}
		fmt.Fprintf(&b, "default_tools_approval_mode = %s\n", tomlString(CodexApproval))
	}
	return b.String(), env, nil
}

// WriteCodexConfig writes CodexConfig of tools as codexHome/config.toml
// (mode 0600) and returns the environment the session needs for them.
func WriteCodexConfig(codexHome string, tools []Tool) (env []string, err error) {
	config, env, err := CodexConfig(tools)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return nil, fmt.Errorf("codex home: %w", err)
	}
	path := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return nil, fmt.Errorf("codex config: %w", err)
	}
	// WriteFile keeps an existing file's mode.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("codex config: %w", err)
	}
	return env, nil
}

// tomlString quotes s as a TOML basic string; JSON's escapes are TOML's.
func tomlString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func tomlArray(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = tomlString(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func tomlTable(m map[string]string) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, tomlString(k)+" = "+tomlString(m[k]))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}
