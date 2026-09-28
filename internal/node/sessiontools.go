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
// clash with a built-in such as prologue — is omitted with the refusal as
// its note, not rendered (trd §3 Tools, Prologue is built in).
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
// …"), in Use's order; "" when every served tool was called cleanly.
// The built-in Prologue is never in Use, so never named. The error
// text goes through redact (the account's Redact) and has every value
// of the tool's own secrets, headers and env taken out too, so no key
// or header can leak, and is capped at 200 characters.
func (st SessionTools) UseNote(res harness.Result, redact func(string) string) string {
	var notes []string
	for _, t := range st.Use {
		stat := res.Tools[t.Name]
		calls := res.ToolCalls[t.Name]
		switch {
		case stat != nil && stat.FirstError != "":
			msg := stat.FirstError
			if redact != nil {
				msg = redact(msg)
			}
			msg = truncate(redactToolValues(msg, t), 200)
			if calls == 0 {
				notes = append(notes, fmt.Sprintf("%s served, never called: %s", t.Name, msg))
				continue
			}
			notes = append(notes, fmt.Sprintf("%s: first call errored: %s", t.Name, msg))
		case calls == 0:
			notes = append(notes, t.Name+" served, never called")
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return "tools: " + strings.Join(notes, "; ")
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
// Prologue attachment) onto the job: MCPServers holds the built-in and
// every tool, Env gains the secret values (the config names them only),
// Secrets gains them so the harness redacts them, and Tools carries the
// tools for an adapter that renders its own (Codex's config.toml). It
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
