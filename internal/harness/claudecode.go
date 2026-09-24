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
	"sort"
	"strconv"
	"strings"
	"time"
)

// ClaudeCode runs `claude -p` in the workspace, stream-json out.
type ClaudeCode struct {
	// Binary is the claude executable ("claude" on PATH by default).
	Binary string
	// ExtraArgs are appended to every invocation (e.g.
	// --dangerously-skip-permissions for an unattended node).
	ExtraArgs []string
	// Stderr receives the process's stderr (nil discards it).
	Stderr io.Writer
}

// Name implements Harness.
func (c *ClaudeCode) Name() string { return "claude-code" }

// Args builds the command line for a job (exposed for tests).
//
// The prompt is *not* here: it goes on stdin. Linux caps a single
// argument at 128KB (MAX_ARG_STRLEN) and an evaluator's prompt carries
// the branch's whole diff — 159KB of it on 2026-09-23, which turned
// every attempt into `fork/exec: argument list too long` and left the
// item retrying every thirty seconds for four hours. `claude -p` with
// no prompt argument reads the prompt from stdin, which has no such
// limit.
func (c *ClaudeCode) Args(job Job) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if job.Model != "" {
		args = append(args, "--model", job.Model)
	}
	if job.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(job.MaxBudgetUSD, 'f', 2, 64))
	}
	args = append(args, MCPArgs(job.MCPServers)...)
	args = append(args, c.ExtraArgs...)
	return args
}

// MCPArgs is the command line for a set of MCP servers: the config
// inline, and every server's tools allowed (a print-mode session cannot
// be asked).
func MCPArgs(servers map[string]any) []string {
	if len(servers) == 0 {
		return nil
	}
	cfg, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	allowed := make([]string, 0, len(names))
	for _, n := range names {
		allowed = append(allowed, "mcp__"+n+"__*")
	}
	return []string{"--mcp-config", string(cfg), "--allowedTools", strings.Join(allowed, ",")}
}

// Run implements Harness. The process is killed when ctx is cancelled.
func (c *ClaudeCode) Run(ctx context.Context, job Job, onProgress func(Progress)) (Result, error) {
	bin := c.Binary
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return Result{}, fmt.Errorf("harness claude-code: %w", err)
	}
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, c.Args(job)...)
	cmd.Dir = job.Workspace
	cmd.Env = append(os.Environ(), job.Env...)
	cmd.Stdin = strings.NewReader(job.Prompt)
	cmd.WaitDelay = 5 * time.Second
	var errBuf bytes.Buffer
	if c.Stderr != nil {
		cmd.Stderr = io.MultiWriter(c.Stderr, &errBuf)
	} else {
		cmd.Stderr = &errBuf
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("harness claude-code: start: %w", err)
	}
	// The raw stdout, capped, in case the harness said why it could not
	// run there rather than on stderr.
	rawOut := &capBuffer{max: 64 << 10}
	res := ParseStream(io.TeeReader(stdout, rawOut), func(ev Event) {
		if onProgress == nil {
			return
		}
		switch ev.Type {
		case "assistant":
			if t := ev.Text(); t != "" {
				onProgress(Progress{Text: t, SessionID: ev.SessionID})
			}
		case "system":
			if ev.Subtype == "init" && ev.SessionID != "" {
				onProgress(Progress{SessionID: ev.SessionID})
			}
		case "result":
			onProgress(Progress{CostUSD: ev.CostUSD, SessionID: ev.SessionID, Text: strings.TrimSpace(ev.Result)})
		}
	})
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && res.SessionID != "" && res.Text != "" {
			// claude exits non-zero on a budget stop or an error result but
			// still wrote its result event; the Result carries the story.
			res.Success = false
			return res, nil
		}
		msg := strings.TrimSpace(errBuf.String())
		if isUnavailable(msg) {
			return res, &UnavailableError{Reason: msg}
		}
		// Stdout is only the harness's own when it held no events; a
		// session's text may mention a rate limit for its own reasons.
		if out := strings.TrimSpace(rawOut.String()); res.EmptyOutput && isUnavailable(out) {
			return res, &UnavailableError{Reason: out}
		}
		if msg == "" {
			msg = waitErr.Error()
		}
		return res, fmt.Errorf("harness claude-code: %s", msg)
	}
	return res, nil
}

// capBuffer keeps the first max bytes written to it and drops the rest.
type capBuffer struct {
	bytes.Buffer
	max int
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}
