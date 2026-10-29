package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// SessionSpec is one harnessed session of a lease.
type SessionSpec struct {
	// Label names the session in logs: the lease.
	Label string
	// Workspace is the directory the session runs in.
	Workspace string
	// Prompt goes on stdin.
	Prompt string
	// Model is the kind's default model; empty leaves the CLI's own.
	Model string
	// BudgetUSD caps the session's spend; 0 is none.
	BudgetUSD float64
	// Servers are the session's MCP servers (an mcp.json's mcpServers):
	// every one an entry of the lease's sessionproxy (harness.ProxyRelay),
	// so no credential is on the command line or in the config.
	Servers map[string]any
	// Env is added to the node's environment.
	Env []string
	// Unset are the node's variables the session must not see.
	Unset []string
	// Secrets are masked in anything the session's error carries.
	Secrets []string
}

// SessionResult is how a session ended.
type SessionResult struct {
	SessionID string
	Text      string
	CostUSD   float64
	Turns     int
	Usage     *harness.Usage
	Duration  time.Duration
}

// Session runs one harnessed session to its end. A cancelled ctx kills
// it and answers ctx's error.
type Session interface {
	Run(ctx context.Context, spec SessionSpec) (SessionResult, error)
}

// ClaudeSession is a `claude -p` session, stream-json out — the minimal
// v1 runner; the streaming session with its mailbox (internal/harness/
// session) replaces it in wave 3.
type ClaudeSession struct {
	// Binary is the claude executable; "claude" on PATH when empty.
	Binary string
	// ExtraArgs are added to every session (--dangerously-skip-permissions
	// on an unattended node).
	ExtraArgs []string
	// Stderr hears the process's stderr; discarded when nil.
	Stderr io.Writer
}

// Args is spec's command line; the prompt goes on stdin.
func (c *ClaudeSession) Args(spec SessionSpec) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(spec.BudgetUSD, 'f', 2, 64))
	}
	if cfg, err := json.Marshal(harness.SessionSettings(nil)); err == nil {
		args = append(args, "--settings", string(cfg))
	}
	args = append(args, harness.MCPArgs(spec.Servers)...)
	return append(args, c.ExtraArgs...)
}

// Run implements Session.
func (c *ClaudeSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	bin := c.Binary
	if bin == "" {
		bin = "claude"
	}
	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, c.Args(spec)...)
	cmd.Dir = spec.Workspace
	cmd.Env = harness.SessionEnviron(os.Environ(), harness.Job{Env: spec.Env, Unset: spec.Unset})
	cmd.Stdin = strings.NewReader(spec.Prompt)
	var tail bytes.Buffer
	stderr := io.Writer(&limited{b: &tail, n: 8 << 10})
	if c.Stderr != nil {
		stderr = io.MultiWriter(stderr, c.Stderr)
	}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return SessionResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return SessionResult{}, fmt.Errorf("claude: %s", harness.Redact(err.Error(), spec.Secrets...))
	}
	res := harness.ParseStream(out, nil)
	waitErr := cmd.Wait()
	sr := SessionResult{SessionID: res.SessionID, Text: res.Text, CostUSD: res.CostUSD, Turns: res.Turns, Usage: res.Usage, Duration: time.Since(start)}
	if ctx.Err() != nil {
		return sr, ctx.Err()
	}
	if waitErr != nil {
		msg := strings.TrimSpace(tail.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return sr, errors.New("claude: " + harness.Redact(lastLines(msg, 5), spec.Secrets...))
	}
	if !res.Success {
		return sr, errors.New("claude: the session ended with an error: " + harness.Redact(lastLines(res.Text, 5), spec.Secrets...))
	}
	return sr, nil
}

// limited keeps the first n bytes written.
type limited struct {
	b *bytes.Buffer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
