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

	"projectmesh.io/mesh-cli/internal/sandbox"
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
	// Sandbox, when set, runs every session in its own pod (ADR 007):
	// `claude` from the node image, the workspace and a temporary HOME
	// the only host paths, the environment the job's allow-listed
	// variables. Nil is `sandbox: off`: the process on the host, as ever.
	Sandbox *sandbox.Sandbox
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
	cfg, allowed, ok := mcpConfig(servers)
	if !ok {
		return nil
	}
	return []string{"--mcp-config", string(cfg), "--allowedTools", allowed}
}

// mcpConfig is the servers' mcp.json and the --allowedTools that lets
// every one of their tools run; ok is false when there are none.
func mcpConfig(servers map[string]any) ([]byte, string, bool) {
	if len(servers) == 0 {
		return nil, "", false
	}
	cfg, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return nil, "", false
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
	return cfg, strings.Join(allowed, ","), true
}

// SandboxArgs is Args for a session in a pod: the same, but the MCP
// config is the file at mcpPath (under the session's HOME) rather than
// inline, so the Prologue bearer is on no command line.
func (c *ClaudeCode) SandboxArgs(job Job, mcpPath string) []string {
	servers := job.MCPServers
	job.MCPServers = nil
	args := c.Args(job)
	if _, allowed, ok := mcpConfig(servers); ok {
		// the MCP flags go where Args puts them, before ExtraArgs
		at := len(args) - len(c.ExtraArgs)
		mcp := []string{"--mcp-config", mcpPath, "--allowedTools", allowed}
		args = append(args[:at:at], append(mcp, args[at:]...)...)
	}
	return args
}

// Run implements Harness. The process is killed when ctx is cancelled.
func (c *ClaudeCode) Run(ctx context.Context, job Job, onProgress func(Progress)) (Result, error) {
	if c.Sandbox != nil || job.Session != nil {
		return c.runSandboxed(ctx, job, onProgress)
	}
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
	cmd.Env = SessionEnviron(os.Environ(), job)
	cmd.Stdin = strings.NewReader(job.Prompt)
	cmd.WaitDelay = 5 * time.Second
	var errBuf bytes.Buffer
	if c.Stderr != nil {
		cmd.Stderr = io.MultiWriter(&redactWriter{w: c.Stderr, secrets: job.Secrets}, &errBuf)
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
	res := parseClaude(io.TeeReader(stdout, rawOut), onProgress)
	waitErr := cmd.Wait()
	return finishClaude(ctx, job, res, waitErr, &errBuf, rawOut)
}

// runSandboxed is Run in the session's own pod: the MCP config written
// under its temporary HOME, `claude` run from the node image with the
// prompt on stdin, the stream read as on the host. A kill by the pod's
// limits is returned as the *sandbox.LimitError it is. The job's own
// Session (its services started by the node) is used and left open;
// without one the harness opens and closes its own.
func (c *ClaudeCode) runSandboxed(ctx context.Context, job Job, onProgress func(Progress)) (Result, error) {
	sess := job.Session
	if sess == nil {
		var err error
		if sess, err = c.Sandbox.Open(job.Label, string(job.Kind)); err != nil {
			return Result{}, fmt.Errorf("harness claude-code: %w", err)
		}
		defer sess.Close()
	}
	var err error
	mcpPath := ""
	if cfg, _, ok := mcpConfig(job.MCPServers); ok {
		if mcpPath, err = sess.WriteFile("mcp.json", cfg); err != nil {
			return Result{}, fmt.Errorf("harness claude-code: mcp config: %w", err)
		}
	}
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}
	var errBuf bytes.Buffer
	var stderr io.Writer = &errBuf
	if c.Stderr != nil {
		stderr = io.MultiWriter(&redactWriter{w: c.Stderr, secrets: job.Secrets}, &errBuf)
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := sess.Run(ctx, sandbox.RunSpec{
			Workspace: job.Workspace,
			Env:       job.Env,
			Command:   append([]string{"claude"}, c.SandboxArgs(job, mcpPath)...),
			Timeout:   job.Timeout,
			Stdin:     strings.NewReader(job.Prompt),
			Stdout:    pw,
			Stderr:    stderr,
		})
		pw.Close()
		done <- err
	}()
	rawOut := &capBuffer{max: 64 << 10}
	res := parseClaude(io.TeeReader(pr, rawOut), onProgress)
	_, _ = io.Copy(io.Discard, pr) // a stream the parser gave up on still drains
	waitErr := <-done
	if le := (*sandbox.LimitError)(nil); errors.As(waitErr, &le) && ctx.Err() == nil {
		return res, le
	}
	return finishClaude(ctx, job, res, waitErr, &errBuf, rawOut)
}

// parseClaude reads a session's stream-json, passing what it says on.
func parseClaude(stdout io.Reader, onProgress func(Progress)) Result {
	return ParseStream(stdout, func(ev Event) {
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
		case "rate_limit_event":
			if rl := ev.RateLimit(time.Now()); rl != nil {
				onProgress(Progress{SessionID: ev.SessionID, RateLimit: rl})
			}
		case "result":
			onProgress(Progress{CostUSD: ev.CostUSD, SessionID: ev.SessionID, Text: strings.TrimSpace(ev.Result)})
		}
	})
}

// finishClaude is how a session ended: its Result, or the error its
// exit, its stderr or its stdout said.
func finishClaude(ctx context.Context, job Job, res Result, waitErr error, errBuf *bytes.Buffer, rawOut *capBuffer) (Result, error) {
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		var sbExit *sandbox.ExitError
		if (errors.As(waitErr, &exitErr) || errors.As(waitErr, &sbExit)) && res.SessionID != "" && res.Text != "" {
			// claude exits non-zero on a budget stop or an error result but
			// still wrote its result event; the Result carries the story.
			res.Success = false
			return res, nil
		}
		msg := Redact(strings.TrimSpace(errBuf.String()), job.Secrets...)
		if isUnavailable(msg) {
			return res, &UnavailableError{Reason: msg}
		}
		// Stdout is only the harness's own when it held no events; a
		// session's text may mention a rate limit for its own reasons.
		if out := Redact(strings.TrimSpace(rawOut.String()), job.Secrets...); res.EmptyOutput && isUnavailable(out) {
			return res, &UnavailableError{Reason: out}
		}
		if msg == "" {
			msg = Redact(waitErr.Error(), job.Secrets...)
		}
		return res, fmt.Errorf("harness claude-code: %s", msg)
	}
	return res, nil
}

// SessionEnviron is the environment a job's process runs in: environ
// without the variables the job unsets, then the job's own Env.
func SessionEnviron(environ []string, job Job) []string {
	out := make([]string, 0, len(environ)+len(job.Env))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, u := range job.Unset {
			if name == u {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(out, job.Env...)
}

// Redacted is what a secret is replaced with wherever it would be shown.
const Redacted = "[redacted]"

// Redact replaces every secret in s with Redacted. Empty secrets are
// ignored.
func Redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	return s
}

// redactWriter passes writes on with the secrets redacted. A secret
// split across two writes is not caught; claude writes its stderr a
// line at a time.
type redactWriter struct {
	w       io.Writer
	secrets []string
}

func (r *redactWriter) Write(p []byte) (int, error) {
	if len(r.secrets) == 0 {
		return r.w.Write(p)
	}
	if _, err := io.WriteString(r.w, Redact(string(p), r.secrets...)); err != nil {
		return 0, err
	}
	return len(p), nil
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
