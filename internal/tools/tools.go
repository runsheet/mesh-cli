// Package tools is the node's half of a session's tools (ADR-006, trd §3
// Tools): the tool the claim-time read hands out, which of them a session
// gets on this node, and each agent's own rendering of them — Claude
// Code's --mcp-config servers and Codex's config.toml — with every secret
// value in the session's environment only.
//
// It is a leaf: it imports nothing from node or gateway.
package tools

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Transports and runtimes a Tool names.
const (
	Remote = "remote"
	Local  = "local"

	Npx    = "npx"
	Uvx    = "uvx"
	Docker = "docker"
)

// Runtimes are the local runtimes a node can report, in the heartbeat's
// order; Adapters the agents, by the names the heartbeat reports them
// under (trd §1 Node, `adapters`).
var (
	Runtimes = []string{Npx, Uvx, Docker}
	Adapters = []string{"claude-code", "codex"}
)

// adapterBinary is the CLI each adapter runs.
var adapterBinary = map[string]string{"claude-code": "claude", "codex": "codex"}

// Builtin is the server the node attaches to every session itself; a
// console tool of that name is a clash (trd §3 Tools, mesh-context is
// built in).
const Builtin = "mesh-context"

// Tool is one tool of the claim-time read (trd §3 Tools). Secrets holds a
// header (remote) or env (local) key and its value; it wins over the same
// key in Headers or Env and its value is never written anywhere but the
// session's environment.
type Tool struct {
	Name              string            `json:"name"`
	Transport         string            `json:"transport"`
	URL               string            `json:"url,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	Runtime           string            `json:"runtime,omitempty"`
	Command           string            `json:"command,omitempty"`
	Args              []string          `json:"args,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	Secrets           map[string]string `json:"secrets,omitempty"`
	Required          bool              `json:"required"`
	Roles             []string          `json:"roles,omitempty"`
	PricePerCallCents int               `json:"pricePerCallCents,omitempty"`
}

// Omission is a tool a session does not get on this node, and why.
type Omission struct {
	Tool    string
	Runtime string
	Note    string
}

// ProbeRuntimes returns which of npx, uvx and docker lookPath finds.
func ProbeRuntimes(lookPath func(string) (string, error)) []string {
	return probe(lookPath, Runtimes)
}

// ProbeAdapters returns the adapters (claude-code, codex) whose CLI
// (claude, codex) lookPath finds.
func ProbeAdapters(lookPath func(string) (string, error)) []string {
	var found []string
	for _, a := range Adapters {
		if _, err := lookPath(adapterBinary[a]); err == nil {
			found = append(found, a)
		}
	}
	return found
}

func probe(lookPath func(string) (string, error), names []string) []string {
	var found []string
	for _, n := range names {
		if _, err := lookPath(n); err == nil {
			found = append(found, n)
		}
	}
	return found
}

// Select returns the tools a session of role gets on a node with
// runtimes: a tool whose roles leave role out is dropped silently; a local
// tool whose runtime is missing is omitted with a note when nice-to-have,
// and reported in missingRequired when required (the node then declines
// the claim).
func Select(tools []Tool, role string, runtimes []string) (use []Tool, omitted []Omission, missingRequired []Omission) {
	have := map[string]bool{}
	for _, r := range runtimes {
		have[r] = true
	}
	for _, t := range tools {
		if !forRole(t, role) {
			continue
		}
		if t.Transport == Local && !have[t.Runtime] {
			o := Omission{Tool: t.Name, Runtime: t.Runtime}
			if t.Required {
				o.Note = fmt.Sprintf("required tool %s needs %s, which this node lacks", t.Name, runtimeName(t.Runtime))
				missingRequired = append(missingRequired, o)
			} else {
				o.Note = fmt.Sprintf("tool %s omitted: this node lacks %s", t.Name, runtimeName(t.Runtime))
				omitted = append(omitted, o)
			}
			continue
		}
		use = append(use, t)
	}
	return use, omitted, missingRequired
}

func runtimeName(r string) string {
	if r == "" {
		return "a runtime"
	}
	return r
}

func forRole(t Tool, role string) bool {
	if len(t.Roles) == 0 {
		return true
	}
	for _, r := range t.Roles {
		if r == role {
			return true
		}
	}
	return false
}

var validName = regexp.MustCompile(`^[a-z0-9_-]+$`)

// check refuses what no renderer may write: a bad or repeated name, a
// clash with a built-in, an unknown transport or runtime.
func check(tools []Tool, builtin map[string]bool) error {
	seen := map[string]bool{}
	for _, t := range tools {
		if !validName.MatchString(t.Name) {
			return fmt.Errorf("tool %q: name must be [a-z0-9_-]", t.Name)
		}
		if builtin[t.Name] {
			return fmt.Errorf("tool %s: clashes with the built-in server of that name", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("tool %s: named twice", t.Name)
		}
		seen[t.Name] = true
		switch t.Transport {
		case Remote:
			if t.URL == "" {
				return fmt.Errorf("tool %s: remote with no url", t.Name)
			}
		case Local:
			switch t.Runtime {
			case Npx, Uvx, Docker:
			default:
				return fmt.Errorf("tool %s: unknown runtime %q", t.Name, t.Runtime)
			}
			if t.Command == "" {
				return fmt.Errorf("tool %s: local with no command", t.Name)
			}
		default:
			return fmt.Errorf("tool %s: unknown transport %q", t.Name, t.Transport)
		}
	}
	return nil
}

// EnvName is the variable a tool's secret travels in:
// MESH_TOOL_<NAME>_<KEY>, upper-cased, anything not [A-Z0-9] as '_'.
func EnvName(tool, key string) string {
	return "MESH_TOOL_" + envPart(tool) + "_" + envPart(key)
}

func envPart(s string) string {
	b := []byte(strings.ToUpper(s))
	for i, c := range b {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '_'
		}
	}
	return string(b)
}

// bearer splits a secret Authorization header into its token when it is
// a bearer one, so both agents carry the token alone in the variable.
func bearer(key, value string) (string, bool) {
	if !strings.EqualFold(key, "Authorization") {
		return "", false
	}
	if len(value) > 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:]), true
	}
	return "", false
}

// localArgs is the command and argv a runtime starts a local tool with;
// docker passes each env key through with -e so the container sees it.
func localArgs(t Tool, envKeys []string) (string, []string) {
	switch t.Runtime {
	case Npx:
		return Npx, append([]string{"-y", t.Command}, t.Args...)
	case Uvx:
		return Uvx, append([]string{t.Command}, t.Args...)
	default:
		args := []string{"run", "-i", "--rm"}
		for _, k := range envKeys {
			args = append(args, "-e", k)
		}
		return Docker, append(append(args, t.Command), t.Args...)
	}
}

// envKeys lists a local tool's env and secret keys, sorted, once each.
func envKeys(t Tool) []string {
	set := map[string]bool{}
	for k := range t.Env {
		set[k] = true
	}
	for k := range t.Secrets {
		set[k] = true
	}
	return sortedKeys(set)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Redact masks every value of env (KEY=value) found in s, the longest
// first so one secret inside another is masked whole.
func Redact(s string, env []string) string {
	var vals []string
	for _, kv := range env {
		if _, v, ok := strings.Cut(kv, "="); ok && v != "" {
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}
