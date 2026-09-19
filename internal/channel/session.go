package channel

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/claudeconf"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/node"
)

// Session is the one persistent `claude -p` the node keeps open with the
// mesh channel attached. It is restarted when it exits.
type Session struct {
	Cfg *node.Config
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
	// Stderr receives claude's stderr.
	Stderr io.Writer
	Log    *log.Logger
	// OnSessionID hears the claude session id when the stream announces it.
	OnSessionID func(id string)
	// OnText hears what the session says.
	OnText func(text string)
}

func (s *Session) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

// IdlePrompt is the -p prompt: nothing to do until an event arrives.
const IdlePrompt = "You are a Mesh node session. There is no work yet: wait for <channel source=\"mesh\"> events and act on them as your instructions say. Reply with one short line."

// SystemPrompt is appended to the session's system prompt.
func SystemPrompt(cfg *node.Config, workRoot string) string {
	return strings.TrimSpace(fmt.Sprintf(`
You are a Mesh node (%s) for org %s, running work items of goals on this machine. Events arrive from the "mesh" channel; its tools report back to the platform.

On a work_item.ready event (attributes: item_id, seq, goal_slug, repo, base_ref, branch, workspace):
1. Call mesh_context(goal_slug) and read it: instructions and ADRs are binding, the PRD/TRD are the design.
2. Prepare the workspace at the given path (under %s/<goal-slug>/<seq>): "gh repo clone <repo> <workspace>" if it does not exist, else "git fetch --prune origin" there; then "git checkout -B <branch> origin/<base_ref>".
3. Do the item as a fresh subagent with clean context (use the Task tool with the item's brief, acceptance and the context; one subagent per item; never carry one item's context into another). Work on that branch only.
4. Run the item's acceptance check and make it pass. Commit with clear, conventional messages.
5. Call mesh_record(item_id, summary) after each milestone and before finishing.
6. Push the branch: "git push -u origin <branch>"; then call mesh_finish(item_id, branch, head_sha) with "git rev-parse HEAD". If the item cannot be done, call mesh_fail(item_id, error) with the reason.
Report mesh_progress(item_id, note, cost_cents) as you go — at least once when you start and once per milestone.

On goal.paused: finish the current step, do not push, stop. On goal.cancelled: stop and drop the goal's items. On goal.resumed: continue. A checkpoint notice needs nothing from you.

Keep every reply to one or two lines; the log is the platform's, not the terminal's. Never open pull requests; never touch anything outside the workspace.`, cfg.Slug, cfg.OrgLogin, workRoot))
}

// Args builds the claude command line.
func (s *Session) Args(workRoot string) []string {
	args := []string{"-p", IdlePrompt, "--output-format", "stream-json", "--verbose",
		"--dangerously-load-development-channels", "server:" + ServerName,
		"--append-system-prompt", SystemPrompt(s.Cfg, workRoot)}
	if s.Unattended {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

// Run keeps one session up until ctx is done, restarting on exit with a
// backoff from 5 s to 60 s.
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

func (s *Session) once(ctx context.Context, bin, workRoot string) error {
	cmd := exec.CommandContext(ctx, bin, s.Args(workRoot)...)
	cmd.Dir = s.WorkDir
	cmd.Env = os.Environ()
	cmd.WaitDelay = 5 * time.Second
	if s.Stderr != nil {
		cmd.Stderr = s.Stderr
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	s.logf("session started (pid %d) in %s", cmd.Process.Pid, s.WorkDir)
	res := harness.ParseStream(stdout, func(ev harness.Event) {
		if ev.SessionID != "" && s.OnSessionID != nil {
			s.OnSessionID(ev.SessionID)
		}
		if ev.Type == "assistant" && s.OnText != nil {
			if t := ev.Text(); t != "" {
				s.OnText(t)
			}
		}
	})
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("claude: %w (last: %s)", waitErr, res.Text)
	}
	return fmt.Errorf("claude ended (%s)", res.Text)
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
