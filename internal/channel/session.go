package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/claudeconf"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/node"
)

// Session is the one persistent `claude -p` the node keeps open with the
// mesh channel attached. A print-mode session ends with its turn, and
// channel events only arrive while a session is open — so it runs in
// streaming-input mode (`--input-format stream-json`) with stdin held
// open until the node stops: the session waits for user messages, and
// events reach it both as channel notifications and as user messages on
// stdin. It is restarted only when the process exits.
type Session struct {
	Cfg *node.Config
	// Orgs are the logins the node serves, for the system prompt.
	Orgs []string
	// MCP are MCP servers the session gets beside the mesh channel (Prologue).
	MCP map[string]any
	// MCPFunc, when set, is asked for them at every session start
	// instead: a credential that refreshes (the node's machine token)
	// gives each restarted process a live bearer.
	MCPFunc func() map[string]any
	// WorkDir is ~/.mesh/work/<node-slug>, the session's cwd.
	WorkDir string
	// Exe is this binary, registered as the channel server.
	Exe string
	// SockPath is the hub's socket the server dials.
	SockPath string
	// Unattended adds --dangerously-skip-permissions.
	Unattended bool
	// Binary is the claude executable ("claude").
	Binary string
	// Model is what the channel session runs on ("" the CLI's default).
	Model string
	// Stderr receives claude's stderr.
	Stderr io.Writer
	Log    *log.Logger
	// OnSessionID hears the claude session id when the stream announces it.
	OnSessionID func(id string)
	// OnText hears what the session says.
	OnText func(text string)
	// OnRateLimit hears the account's plan standing each time the
	// stream reports it (a rate_limit_event).
	OnRateLimit func(rl *harness.RateLimit)
	// OnStart runs after each (re)start of the process, once it is alive.
	OnStart func()
	// OnTurn hears each turn's end: the item the message that started it
	// was for ("" for the idle prompt and goal notices) and the session's
	// cumulative cost as claude reports it.
	OnTurn func(itemID string, costUSD float64)

	mu     sync.Mutex
	stdin  io.WriteCloser
	proc   *os.Process
	starts int
	// pending lists the items of the user messages sent whose turn has
	// not ended, oldest first; the session is idle when it is empty.
	// idleSince is when it last became so.
	pending   []string
	idleSince time.Time
}

func (s *Session) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

// IdlePrompt is the first user message: nothing to do until an event arrives.
const IdlePrompt = "You are a Mesh node session. There is no work yet: wait for mesh events (channel notifications or messages like this one) and act on them as your instructions say. Reply with one short line."

// SystemPrompt is appended to the session's system prompt.
func SystemPrompt(cfg *node.Config, orgs []string, workRoot string) string {
	who := "org " + strings.Join(orgs, ", ")
	if len(orgs) != 1 {
		who = "orgs " + strings.Join(orgs, ", ")
	}
	return strings.TrimSpace(fmt.Sprintf(`
You are a Mesh node (%s) for %s, running work items of goals on this machine. Events arrive from the "mesh" channel — and, as a fallback, as user messages that start with [mesh event …]; treat both the same. The channel's tools report back to the platform.

On a work_item.ready event (attributes: item_id, seq, goal_slug, repo, base_ref, branch, workspace):
1. Call mesh_context(goal_slug) and read it: instructions and ADRs are binding, the PRD/TRD are the design; it ends with the project's whole index — fetch anything else with the prologue_document tool (search with prologue_search; other projects through prologue_projects) rather than guessing.
2. Work in the workspace at the given path (under %s/<goal-slug>/<seq>). The node has already cloned it, checked out <branch> (a retry continues the last attempt; the event says what its evaluator found) and given it a git credential for <repo> alone; never clone, re-clone or re-credential it, and never run gh auth. If that path does not exist or is not a git checkout, call mesh_fail(item_id, error) saying so rather than preparing one yourself.
3. Do the item as a fresh subagent with clean context (use the Task tool with the item's brief, acceptance and the context; one subagent per item; never carry one item's context into another). Work on that branch only.
4. Run the item's acceptance check and make it pass. Commit with clear, conventional messages.
5. Call mesh_record(item_id, summary) after each milestone and before finishing: it reports the summary to the platform and writes no file — never write or commit a goal status table yourself.
6. Push the branch: "git push -u origin <branch>" ("--force-with-lease" too when you rebased it for a rework); then call mesh_finish(item_id, branch, head_sha) with "git rev-parse HEAD". If the item cannot be done, call mesh_fail(item_id, error) with the reason.
Report mesh_progress(item_id, note, cost_cents) as you go — at least once when you start and once per milestone. An event for an item you already finished or failed needs nothing but a one-line reply.

On goal.paused: finish the current step, do not push, stop. On goal.cancelled: stop and drop the goal's items. On goal.resumed: continue. A checkpoint notice needs nothing from you.

Keep every reply to one or two lines; the log is the platform's, not the terminal's. Never open pull requests; never touch anything outside the workspace.`, cfg.Slug, who, workRoot))
}

// Args builds the claude command line: print mode with streaming input,
// so the session lives as long as stdin does.
func (s *Session) Args(workRoot string) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--dangerously-load-development-channels", "server:" + ServerName,
		"--append-system-prompt", SystemPrompt(s.Cfg, s.Orgs, workRoot)}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	args = append(args, harness.MCPArgs(s.mcp())...)
	if s.Unattended {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

// mcp is the session's MCP servers, asked for afresh at each start when
// MCPFunc is set.
func (s *Session) mcp() map[string]any {
	if s.MCPFunc != nil {
		return s.MCPFunc()
	}
	return s.MCP
}

// UserMessage renders an event as a stream-json user message for stdin:
// the meta as a [mesh event …] header line, then the content.
func UserMessage(content string, meta map[string]string) string {
	var b strings.Builder
	b.WriteString("[mesh event")
	for _, k := range []string{"kind", "item_id", "seq", "goal_slug", "repo", "base_ref", "branch", "workspace", "status"} {
		if v := meta[k]; v != "" {
			fmt.Fprintf(&b, " %s=%s", k, v)
		}
	}
	b.WriteString("]\n")
	b.WriteString(content)
	return b.String()
}

// Send writes one user message to the session's stdin.
func (s *Session) Send(text string) error { return s.SendFor("", text) }

// SendFor writes one user message about an item, so the turn it starts
// is attributed to that item when it ends.
func (s *Session) SendFor(itemID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin == nil {
		return fmt.Errorf("session: not running")
	}
	line, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(line, '\n'))
	if err == nil {
		s.pending = append(s.pending, itemID)
	}
	return err
}

// Kill ends the process; Run restarts it and OnStart re-pushes what it
// held. For a turn that has gone quiet past the node's patience.
func (s *Session) Kill(why string) {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p == nil {
		return
	}
	s.logf("session killed: %s", why)
	_ = p.Kill()
}

// Idle reports whether the session is up with no turn in flight, and for
// how long. A held item on an idle session is one nobody is working.
func (s *Session) Idle() (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin == nil || len(s.pending) > 0 {
		return false, 0
	}
	return true, time.Since(s.idleSince)
}

// turnEnded is one result event: the turn of the oldest pending message.
func (s *Session) turnEnded(costUSD float64) {
	s.mu.Lock()
	item := ""
	if len(s.pending) > 0 {
		item = s.pending[0]
		s.pending = s.pending[1:]
	}
	if len(s.pending) == 0 {
		s.idleSince = time.Now()
	}
	on := s.OnTurn
	s.mu.Unlock()
	if on != nil {
		on(item, costUSD)
	}
}

// Alive reports whether a process is up with stdin open.
func (s *Session) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdin != nil
}

// Starts counts process starts.
func (s *Session) Starts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// Run keeps one session up until ctx is done, restarting on exit with a
// backoff from 5 s to 60 s. A live session is never restarted.
func (s *Session) Run(ctx context.Context, workRoot string) error {
	if err := os.MkdirAll(s.WorkDir, 0o755); err != nil {
		return err
	}
	claudeconf.EnsureMeshServer(s.WorkDir, s.Exe, []string{"channel", "serve", "--sock", s.SockPath})
	bin := s.Binary
	if bin == "" {
		bin = "claude"
	}
	backoff := 5 * time.Second
	for {
		started := time.Now()
		err := s.once(ctx, bin, workRoot)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logf("session exited after %s: %v; restarting in %s", time.Since(started).Round(time.Second), err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if time.Since(started) > 5*time.Minute {
			backoff = 5 * time.Second
		} else if backoff < time.Minute {
			backoff *= 2
		}
	}
}

// operatorGitHubEnv is what the node daemon's environment may carry of
// the operator's own GitHub access, none of which the session inherits.
var operatorGitHubEnv = map[string]bool{"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true, "GH_CONFIG_DIR": true}

// SessionEnv is the shared session's environment: the daemon's own
// (environ) less every GitHub token and gh config dir it carries, with
// GH_CONFIG_DIR a fresh, empty directory for the process's life. The
// session runs many items, so no one item's GH_TOKEN goes here either;
// each item's git reaches GitHub through its own workspace's credential
// helper, and gh reaches nothing.
func SessionEnv(environ []string, ghConfigDir string) []string {
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); !operatorGitHubEnv[k] {
			env = append(env, kv)
		}
	}
	return append(env, "GH_CONFIG_DIR="+ghConfigDir)
}

func (s *Session) once(ctx context.Context, bin, workRoot string) error {
	cmd := exec.CommandContext(ctx, bin, s.Args(workRoot)...)
	cmd.Dir = s.WorkDir
	ghDir, err := node.NewGHConfigDir()
	if err != nil {
		return fmt.Errorf("gh config dir: %w", err)
	}
	defer os.RemoveAll(ghDir)
	cmd.Env = SessionEnv(os.Environ(), ghDir)
	cmd.WaitDelay = 5 * time.Second
	if s.Stderr != nil {
		cmd.Stderr = s.Stderr
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	s.mu.Lock()
	s.stdin = stdin
	s.proc = cmd.Process
	s.starts++
	s.pending = nil
	s.idleSince = time.Now()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.stdin = nil
		s.proc = nil
		s.pending = nil
		s.mu.Unlock()
		_ = stdin.Close()
	}()
	s.logf("session started (pid %d) in %s", cmd.Process.Pid, s.WorkDir)
	if err := s.Send(IdlePrompt); err != nil {
		s.logf("session: idle prompt: %v", err)
	}
	if s.OnStart != nil {
		go s.OnStart()
	}
	var last string
	harness.ParseStream(stdout, func(ev harness.Event) {
		if ev.SessionID != "" && s.OnSessionID != nil {
			s.OnSessionID(ev.SessionID)
		}
		if rl := ev.RateLimit(time.Now()); rl != nil && s.OnRateLimit != nil {
			s.OnRateLimit(rl)
		}
		switch ev.Type {
		case "assistant":
			if t := ev.Text(); t != "" {
				last = t
				if s.OnText != nil {
					s.OnText(t)
				}
			}
		case "result":
			// a turn ended; the session stays open for the next message
			s.logf("session turn: $%.4f, %d turns, %s", ev.CostUSD, ev.NumTurns, ev.Subtype)
			s.turnEnded(ev.CostUSD)
		}
	})
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("claude: %w (last: %s)", waitErr, last)
	}
	return fmt.Errorf("claude ended (last: %s)", last)
}

// WatchGoals polls the org's goals every interval and pushes a notice
// when one changes status: goal.paused, goal.cancelled, goal.resumed,
// checkpoint (awaiting_approval).
func WatchGoals(ctx context.Context, gw node.Gateway, orgID string, interval time.Duration, push func(kind string, goal gateway.Goal)) {
	last := map[string]string{}
	first := true
	tick := func() {
		goals, err := gw.Goals(ctx, orgID)
		if err != nil {
			return
		}
		for _, g := range goals {
			prev, seen := last[g.ID]
			last[g.ID] = g.Status
			if first || !seen || prev == g.Status {
				continue
			}
			switch g.Status {
			case "paused", "blocked":
				push("goal.paused", g)
			case "cancelled":
				push("goal.cancelled", g)
			case "running":
				if prev == "paused" || prev == "blocked" {
					push("goal.resumed", g)
				}
			case "awaiting_approval":
				push("checkpoint", g)
			}
		}
		first = false
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
