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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// ClaudeCode runs `claude -p` in the workspace, stream-json out.
type ClaudeCode struct {
	// Binary is the claude executable ("claude" on PATH by default); a
	// host path, never used in a sandbox.
	Binary string
	// SandboxBinary is the agent's executable in the pod: empty is
	// `claude` on the node image's PATH; else a path inside the pod, such
	// as one in the workspace, which is mounted at its own path (the
	// e2e's stub agent).
	SandboxBinary string
	// ExtraArgs are appended to every invocation (e.g.
	// --dangerously-skip-permissions for an unattended node).
	ExtraArgs []string
	// Stderr receives the process's stderr (nil discards it).
	Stderr io.Writer
	// Sandbox, when set, runs every session in its own pod (ADR 007):
	// `claude` from the node image, the workspace and a temporary HOME
	// the only host paths, the environment the job's allow-listed
	// variables. Nil is `sandbox: off`: the process on the host, in Unit.
	Sandbox *sandbox.Sandbox
	// Unit runs a host session outside the node process (ADR 008); nil is
	// sessionunit.Default() — a systemd user scope, else detached.
	Unit sessionunit.Unit
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
	if len(job.Settings) > 0 {
		if cfg, err := json.Marshal(job.Settings); err == nil {
			args = append(args, "--settings", string(cfg))
		}
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

// Run implements Harness. The session runs in its own unit
// (sessionunit), not as the node's child: its stream-json goes to
// `<workdir>.session/output.jsonl`, read back as it grows, and its
// state to `state.json` beside it, so the node's exit does not end it.
// A cancelled ctx or the job's Timeout stops the unit.
func (c *ClaudeCode) Run(ctx context.Context, job Job, onProgress func(Progress)) (Result, error) {
	if c.Sandbox != nil || job.Session != nil {
		// the pod is the unit once sandboxed-sessions adopts it (ADR 008)
		return c.runSandboxed(ctx, job, onProgress)
	}
	bin := c.Binary
	if bin == "" {
		bin = "claude"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return Result{}, fmt.Errorf("harness claude-code: %w", err)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs // the unit may search another PATH
	}
	parent := ctx
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}
	unit := c.Unit
	if unit == nil {
		unit = sessionunit.Default()
	}
	dir, files, err := openSessionDir(job)
	if err != nil {
		return Result{}, fmt.Errorf("harness claude-code: session dir: %w", err)
	}
	// the session is handled when Run returns: its files go with it —
	// unless it outlives the node, when the node removes them once it has
	// handled the end, or the next node does
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	h, err := unit.Start(sessionunit.Spec{
		Kind: string(job.Kind), Subject: job.Meta.subject(job.Label),
		Binary: path, Args: c.Args(job), Dir: job.Workspace,
		Env:   SessionEnviron(os.Environ(), job),
		Stdin: files.prompt, Stdout: files.out, Stderr: files.err,
	})
	files.close()
	if err != nil {
		return Result{}, fmt.Errorf("harness claude-code: start: %w", err)
	}
	st := job.state(unit, h, c.Name())
	if err := sessionunit.WriteState(dir, st); err != nil {
		unit.Stop(h)
		return Result{}, fmt.Errorf("harness claude-code: state: %w", err)
	}
	keep = job.Outlive && job.Workspace != ""
	res, detached, waitErr, errBuf, rawOut := c.follow(ctx, parent, job.Outlive, unit, h, dir, &st, 0, job.Secrets, onProgress)
	if detached {
		return res, parent.Err()
	}
	return finishClaude(ctx, job, res, waitErr, errBuf, rawOut)
}

// Attach implements Attacher.
func (c *ClaudeCode) Attach(ctx context.Context, dir string, onProgress func(Progress)) (Result, error) {
	st, err := sessionunit.ReadState(dir)
	if err != nil {
		return Result{}, fmt.Errorf("harness claude-code: attach: %w", err)
	}
	var unit sessionunit.Unit = sessionunit.Detached{}
	if st.Runner == (sessionunit.SystemdScope{}).Name() {
		unit = sessionunit.SystemdScope{}
	}
	if c.Unit != nil && c.Unit.Name() == st.Runner {
		unit = c.Unit
	}
	h := sessionunit.Handle{PID: st.PID, UnitName: st.Unit}
	parent := ctx
	if st.TimeoutSeconds > 0 {
		// the session's own wall clock, from when it started
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, st.StartedAt.Add(time.Duration(st.TimeoutSeconds)*time.Second))
		defer cancel()
	}
	res, detached, waitErr, errBuf, rawOut := c.follow(ctx, parent, true, unit, h, dir, &st, st.Offset, nil, onProgress)
	if detached {
		return res, parent.Err()
	}
	return finishClaude(ctx, Job{Timeout: time.Duration(st.TimeoutSeconds) * time.Second}, res, waitErr, errBuf, rawOut)
}

// RemoveSession removes a work dir's session directory, and the
// temporary dirs its state names: the node calls it once it has handled
// the end of a session that outlives it.
func RemoveSession(workdir string) error {
	if workdir == "" {
		return nil
	}
	dir := sessionunit.Dir(workdir)
	if st, err := sessionunit.ReadState(dir); err == nil {
		for _, d := range st.TempDirs {
			os.RemoveAll(d)
		}
	}
	return os.RemoveAll(dir)
}

// follow reads a running (or ended) session in dir to its end: stderr
// into a buffer (and c.Stderr), the output through the stream parser,
// progress passed on only for the events past from, and the offset
// saved into st as it advances. ctx done stops the unit — unless
// outlive and parent is done too (the node shutting down), when the
// reading stops and the session is left running: detached.
func (c *ClaudeCode) follow(ctx, parent context.Context, outlive bool, unit sessionunit.Unit, h sessionunit.Handle, dir string, st *sessionunit.State, from int64, secrets []string, onProgress func(Progress)) (res Result, detached bool, waitErr error, errBuf *bytes.Buffer, rawOut *capBuffer) {
	errBuf, rawOut = &bytes.Buffer{}, &capBuffer{max: 64 << 10}
	readCtx, stopReading := context.WithCancel(context.Background())
	defer stopReading()
	var detach atomic.Bool
	// a cancelled ctx or the timeout stops the unit; the reading below
	// then ends with the process, as it did with a pipe
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		select {
		case <-ctx.Done():
			if outlive && parent.Err() != nil {
				detach.Store(true)
				stopReading()
				return
			}
			unit.Stop(h)
		case <-ended:
		}
	}()
	alive := func() bool { return unit.Alive(h) }

	var stderr io.Writer = errBuf
	if c.Stderr != nil {
		stderr = io.MultiWriter(&redactWriter{w: c.Stderr, secrets: secrets}, errBuf)
	}
	errDone := make(chan struct{})
	if errFollow, err := sessionunit.Follow(readCtx, filepath.Join(dir, sessionunit.StderrFile), 0, alive); err == nil {
		defer errFollow.Close()
		go func() {
			_, _ = io.Copy(stderr, errFollow)
			close(errDone)
		}()
	} else {
		close(errDone)
	}
	out, err := sessionunit.Follow(readCtx, filepath.Join(dir, sessionunit.OutputFile), 0, alive)
	if err != nil {
		unit.Stop(h)
		<-errDone
		return Result{}, false, fmt.Errorf("output: %w", err), errBuf, rawOut
	}
	defer out.Close()
	progress := onProgress
	if onProgress != nil && from > 0 {
		// what lies before the saved offset was reported by the node
		// that started the session
		progress = func(p Progress) {
			if out.Offset() > from {
				onProgress(p)
			}
		}
	}
	res = parseClaude(io.TeeReader(out, rawOut), progress, func(ev Event) {
		if out.Offset() <= st.Offset {
			return
		}
		// the offset read and reported so far, after every event, so a
		// node that adopts reports nothing twice
		if ev.SessionID != "" && st.SessionID == "" {
			st.SessionID = ev.SessionID
		}
		st.Offset = out.Offset()
		_ = sessionunit.WriteState(dir, *st)
	})
	if detach.Load() {
		<-errDone
		return res, true, nil, errBuf, rawOut
	}
	waitErr = h.Wait(context.Background())
	<-errDone
	// the whole output is read: a node that adopts after this reports
	// nothing twice
	if st.Offset < out.Offset() {
		st.Offset = out.Offset()
		_ = sessionunit.WriteState(dir, *st)
	}
	return res, false, waitErr, errBuf, rawOut
}

// sessionFiles are the files a session is started on: the prompt (on
// stdin, already unlinked) and the output and stderr it writes.
type sessionFiles struct{ prompt, out, err *os.File }

func (f sessionFiles) close() {
	for _, fh := range []*os.File{f.prompt, f.out, f.err} {
		if fh != nil {
			fh.Close()
		}
	}
}

// openSessionDir makes the job's session directory, `<workdir>.session`
// (a temporary one for a job with no work dir), fresh: the output and
// stderr files empty, and the prompt in a file unlinked at once, which
// the session reads on stdin — never on a pipe the node's exit would
// break, and never left on disk.
func openSessionDir(job Job) (string, sessionFiles, error) {
	var f sessionFiles
	var dir string
	if job.Workspace == "" {
		d, err := os.MkdirTemp("", "mesh-session-*")
		if err != nil {
			return "", f, err
		}
		dir = d
	} else {
		dir = sessionunit.Dir(job.Workspace)
		// a session before this one on the same work dir is done with
		if err := os.RemoveAll(dir); err != nil {
			return "", f, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", f, err
		}
	}
	fail := func(err error) (string, sessionFiles, error) {
		f.close()
		os.RemoveAll(dir)
		return "", sessionFiles{}, err
	}
	var err error
	if f.prompt, err = os.CreateTemp(dir, ".prompt-*"); err != nil {
		return fail(err)
	}
	os.Remove(f.prompt.Name())
	if _, err = io.WriteString(f.prompt, job.Prompt); err != nil {
		return fail(err)
	}
	if _, err = f.prompt.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if f.out, err = os.OpenFile(filepath.Join(dir, sessionunit.OutputFile), os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		return fail(err)
	}
	if f.err, err = os.OpenFile(filepath.Join(dir, sessionunit.StderrFile), os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		return fail(err)
	}
	return dir, f, nil
}

// subject is what a session is for, in its unit's name.
func (m SessionMeta) subject(label string) string {
	for _, s := range []string{m.ItemID, m.GoalID, m.DirectionID, label} {
		if s != "" {
			return s
		}
	}
	return ""
}

// state is the job's state file on its start: identity, unit and
// limits, and nothing of its prompt, environment or secrets.
func (job Job) state(unit sessionunit.Unit, h sessionunit.Handle, harness string) sessionunit.State {
	acct := job.Meta.Account
	if acct == "" {
		for _, kv := range job.Env {
			if v, ok := strings.CutPrefix(kv, "MESH_ACCOUNT="); ok {
				acct = v
			}
		}
	}
	return sessionunit.State{
		NodeID: job.Meta.NodeID, ItemID: job.Meta.ItemID, GoalID: job.Meta.GoalID, DirectionID: job.Meta.DirectionID,
		Attempt: job.Meta.Attempt, Kind: string(job.Kind), Label: job.Label, Model: job.Model, Account: acct,
		Harness: harness, Runner: unit.Name(), PID: h.PID, Unit: h.UnitName, Workspace: job.Workspace,
		StartedAt: time.Now().UTC(), TimeoutSeconds: int(job.Timeout / time.Second), MaxBudgetUSD: job.MaxBudgetUSD,
		TempDirs: job.Meta.TempDirs,
	}
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
	bin := c.SandboxBinary
	if bin == "" {
		bin = "claude"
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := sess.Run(ctx, sandbox.RunSpec{
			Workspace: job.Workspace,
			Env:       job.Env,
			Command:   append([]string{bin}, c.SandboxArgs(job, mcpPath)...),
			Timeout:   job.Timeout,
			Stdin:     strings.NewReader(job.Prompt),
			Stdout:    pw,
			Stderr:    stderr,
		})
		pw.Close()
		done <- err
	}()
	rawOut := &capBuffer{max: 64 << 10}
	res := parseClaude(io.TeeReader(pr, rawOut), onProgress, nil)
	_, _ = io.Copy(io.Discard, pr) // a stream the parser gave up on still drains
	waitErr := <-done
	if le := (*sandbox.LimitError)(nil); errors.As(waitErr, &le) && ctx.Err() == nil {
		return res, le
	}
	return finishClaude(ctx, job, res, waitErr, &errBuf, rawOut)
}

// parseClaude reads a session's stream-json, passing what it says on;
// after, when set, sees every event once it has been passed on.
func parseClaude(stdout io.Reader, onProgress func(Progress), after func(Event)) Result {
	return ParseStream(stdout, func(ev Event) {
		if after != nil {
			defer after(ev)
		}
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
