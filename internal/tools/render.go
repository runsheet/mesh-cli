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
// (the Prologue server) and one entry per tool — and the environment the
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

// WriteCodexConfig writes codexHome/config.toml (mode 0600) with one
// [mcp_servers.<name>] table per tool and returns the environment the
// session needs for them. The file names variables, never values: a
// remote tool's bearer Authorization goes by bearer_token_env_var, any
// other secret header by env_http_headers, both under
// MESH_TOOL_<NAME>_<KEY>; a local tool's secret env goes by env_vars,
// which Codex forwards under the key itself, so the variable is the key —
// two tools giving one key different values is an error.
func WriteCodexConfig(codexHome string, tools []Tool) (env []string, err error) {
	if err := check(tools, map[string]bool{Builtin: true}); err != nil {
		return nil, err
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
					return nil, fmt.Errorf("tool %s: secret env %s differs from another tool's", t.Name, k)
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
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return nil, fmt.Errorf("codex home: %w", err)
	}
	path := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
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
