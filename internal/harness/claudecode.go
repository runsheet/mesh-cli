package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
func (c *ClaudeCode) Args(job Job) []string {
	args := []string{"-p", job.Prompt, "--output-format", "stream-json", "--verbose"}
	if job.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(job.MaxBudgetUSD, 'f', 2, 64))
	}
	args = append(args, c.ExtraArgs...)
	return args
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
	cmd := exec.CommandContext(ctx, bin, c.Args(job)...)
	cmd.Dir = job.Workspace
	cmd.Env = append(os.Environ(), job.Env...)
	cmd.Stdin = nil
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
	res := ParseStream(stdout, func(ev Event) {
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
		if msg == "" {
			msg = waitErr.Error()
		}
		return res, fmt.Errorf("harness claude-code: %s", msg)
	}
	return res, nil
}
