package node

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/tools"
)

// DeclineHold is how long a decline keeps the node off an item while its
// runtimes are unchanged: the item's tools may change in the console
// meanwhile, and the next claim reads them afresh.
const DeclineHold = time.Hour

// SessionTools is what a session of one role gets of its claim-time
// tools on this node (trd §3 Tools): Use are rendered, Omitted run
// without (each noted on the item), Missing are required tools whose
// runtime the node lacks — any makes the node decline the session.
type SessionTools struct {
	Use     []tools.Tool
	Omitted []tools.Omission
	Missing []tools.Omission
}

// SelectTools is tools.Select for the session's role and the node's
// runtimes, and a tool the renderer refuses — a bad or repeated name, a
// clash with a built-in such as mesh-context — is omitted with the refusal
// as its note, not rendered (trd §3 Tools, mesh-context is built in).
func SelectTools(builtin map[string]any, ts []tools.Tool, role string, runtimes []string) SessionTools {
	use, omitted, missing := tools.Select(ts, role, runtimes)
	st := SessionTools{Omitted: omitted, Missing: missing}
	for _, t := range use {
		if _, _, err := tools.ClaudeCode(builtin, append(slices.Clone(st.Use), t)); err != nil {
			st.Omitted = append(st.Omitted, tools.Omission{Tool: t.Name, Runtime: t.Runtime, Note: "tool refused: " + err.Error()})
			continue
		}
		st.Use = append(st.Use, t)
	}
	return st
}

// DeclineReason is the ReleaseWorkItem reason for a session declined
// for its missing required tools: each tool and the runtime it needs.
func (st SessionTools) DeclineReason() string {
	notes := make([]string, len(st.Missing))
	for i, o := range st.Missing {
		notes[i] = o.Note
	}
	return "tools: " + strings.Join(notes, "; ")
}

// OmittedNote is the item note for the nice-to-have tools the session
// runs without ("" when none).
func (st SessionTools) OmittedNote() string {
	if len(st.Omitted) == 0 {
		return ""
	}
	notes := make([]string, len(st.Omitted))
	for i, o := range st.Omitted {
		notes[i] = o.Note
	}
	return "tools: " + strings.Join(notes, "; ")
}

// UseNote is the item note for how the session used the tools it was
// served (goal tools-show-their-use): every tool in Use it never called
// ("tools: brave served, never called") and every one whose calls
// errored, by its first error ("tools: perplexity: first call errored:
// …"), in Use's order; then every other server the result names with
// an error — the built-in context server (tools.Builtin) above all,
// never in Use, and Codex's own — by name ("mesh-context: first call
// errored: …", or "mesh-context served, never called: …" for one that
// failed to start); "" when every served tool was called cleanly and
// no other server errored. The error text goes through redact (the
// account's Redact) and has every value of every served tool's
// secrets, headers and env taken out too, so no key or header can
// leak, and is capped at 200 characters.
func (st SessionTools) UseNote(res harness.Result, redact func(string) string) string {
	var notes []string
	errored := func(name, msg string, calls int, redactValues func(string) string) {
		if redact != nil {
			msg = redact(msg)
		}
		msg = truncate(redactValues(msg), 200)
		if calls == 0 {
			notes = append(notes, fmt.Sprintf("%s served, never called: %s", name, msg))
			return
		}
		notes = append(notes, fmt.Sprintf("%s: first call errored: %s", name, msg))
	}
	used := map[string]bool{}
	for _, t := range st.Use {
		used[t.Name] = true
		stat := res.Tools[t.Name]
		calls := res.ToolCalls[t.Name]
		switch {
		case stat != nil && stat.FirstError != "":
			errored(t.Name, stat.FirstError, calls, func(m string) string { return redactToolValues(m, t) })
		case calls == 0:
			notes = append(notes, t.Name+" served, never called")
		}
	}
	var others []string
	for name, stat := range res.Tools {
		if !used[name] && stat != nil && stat.FirstError != "" {
			others = append(others, name)
		}
	}
	sort.Slice(others, func(i, j int) bool {
		// the built-in first: it is what the session reads the project by
		if (others[i] == tools.Builtin) != (others[j] == tools.Builtin) {
			return others[i] == tools.Builtin
		}
		return others[i] < others[j]
	})
	for _, name := range others {
		errored(name, res.Tools[name].FirstError, res.ToolCalls[name], st.redactAllValues)
	}
	if len(notes) == 0 {
		return ""
	}
	return "tools: " + strings.Join(notes, "; ")
}

// redactAllValues is s with every served tool's values taken out
// (redactToolValues for each in Use).
func (st SessionTools) redactAllValues(s string) string {
	for _, t := range st.Use {
		s = redactToolValues(s, t)
	}
	return s
}

// redactToolValues is s with every value of t's secrets and headers
// replaced, longest first, and a bearer header's token as well; a
// header value under four characters is no key and is left.
func redactToolValues(s string, t tools.Tool) string {
	var vals []string
	add := func(v string, min int) {
		if v = strings.TrimSpace(v); len(v) < min {
			return
		}
		vals = append(vals, v)
		if tok, ok := strings.CutPrefix(v, "Bearer "); ok && strings.TrimSpace(tok) != "" {
			vals = append(vals, strings.TrimSpace(tok))
		}
	}
	for _, v := range t.Secrets {
		add(v, 1)
	}
	for _, v := range t.Headers {
		add(v, 4)
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

// Names are the rendered tools' names, for a log line: names only, never
// a config or a value.
func (st SessionTools) Names() string {
	names := make([]string, len(st.Use))
	for i, t := range st.Use {
		names[i] = t.Name
	}
	return strings.Join(names, " ")
}

// Apply renders the tools in use for Claude Code beside builtin (the
// mesh-context attachment) onto the job: MCPServers holds the built-in and
// every tool, Env gains the secret values (the config names them only),
// Secrets gains them so the harness redacts them, and Tools carries the
// tools for an adapter that renders its own: Codex's config.toml takes
// them by tools.CodexConfig, since Codex expands no ${…}. It
// returns the secret env, for the caller's own redaction.
func (st SessionTools) Apply(job *harness.Job, builtin map[string]any) ([]string, error) {
	if len(st.Use) == 0 {
		job.MCPServers = builtin
		return nil, nil
	}
	servers, env, err := tools.ClaudeCode(builtin, st.Use)
	if err != nil {
		return nil, err
	}
	job.MCPServers = servers
	job.Env = append(job.Env, env...)
	for _, kv := range env {
		if _, v, ok := strings.Cut(kv, "="); ok && v != "" {
			job.Secrets = append(job.Secrets, v)
		}
	}
	job.Tools = st.Use
	return env, nil
}

// UseTools is Apply for acct's session: the tools go on the job, their
// secrets on acct for Redact, and acct remembers the tools so Finish
// reports their priced calls.
func (st SessionTools) UseTools(job *harness.Job, builtin map[string]any, acct *SessionAccount) error {
	env, err := st.Apply(job, builtin)
	if err != nil {
		return err
	}
	acct.toolEnv, acct.used = env, st.Use
	return nil
}

// Declines are the items this node declined for a required tool whose
// runtime it lacks (trd §3 Tools): the loop does not claim one again
// while the node's runtimes are those it declined on, for DeclineHold at
// most. Safe for concurrent use; the zero value is ready.
type Declines struct {
	mu sync.Mutex
	m  map[string]decline
}

type decline struct {
	// key is the item, the missing tools and the runtimes it was
	// declined on.
	key      string
	runtimes string
	at       time.Time
}

func runtimeKey(runtimes []string) string {
	r := slices.Clone(runtimes)
	sort.Strings(r)
	return strings.Join(r, ",")
}

// Record remembers that itemID was declined for missing on runtimes.
func (d *Declines) Record(itemID string, missing []tools.Omission, runtimes []string, now time.Time) {
	if d == nil {
		return
	}
	var ts []string
	for _, o := range missing {
		ts = append(ts, o.Tool+"@"+o.Runtime)
	}
	sort.Strings(ts)
	rk := runtimeKey(runtimes)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[string]decline{}
	}
	d.m[itemID] = decline{key: fmt.Sprintf("%s|%s|%s", itemID, strings.Join(ts, ","), rk), runtimes: rk, at: now}
}

// Holds reports whether itemID stays declined on a node with runtimes
// at now; a decline whose runtimes changed or whose hold ran out is
// forgotten.
func (d *Declines) Holds(itemID string, runtimes []string, now time.Time) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dc, ok := d.m[itemID]
	if !ok {
		return false
	}
	if dc.runtimes != runtimeKey(runtimes) || now.Sub(dc.at) >= DeclineHold {
		delete(d.m, itemID)
		return false
	}
	return true
}

// ToolTrimSettings are the Claude Code settings beside a session's MCP
// config that bound its Bash output to maxKB (trd §3 Tool results): a
// PreToolUse hook on Bash, `<bin> tool-trim --max-kb N --hook`, which
// rewrites the command to pipe through `<bin> tool-trim --max-kb N`
// (harness.TrimHook). nil for a cap of 0.
func ToolTrimSettings(bin string, maxKB int) map[string]any {
	if maxKB <= 0 {
		return nil
	}
	return map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{map[string]any{
			"matcher": "Bash",
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": harness.ToolTrimCommand(bin, maxKB) + " --hook",
				"timeout": 10,
			}},
		}},
	}}
}

// ToolResultsEnv is what the session's environment carries for a cap
// of maxKB, so what the hook does not see is bounded by the CLI too:
// BASH_MAX_OUTPUT_LENGTH in characters (the cap), MAX_MCP_OUTPUT_TOKENS
// at four bytes a token. None for a cap of 0.
func ToolResultsEnv(maxKB int) []string {
	if maxKB <= 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("MAX_MCP_OUTPUT_TOKENS=%d", maxKB*1024/4),
		fmt.Sprintf("BASH_MAX_OUTPUT_LENGTH=%d", maxKB*1024),
	}
}
