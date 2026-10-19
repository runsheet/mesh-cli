package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// runDetached is runSandboxed for a job that outlives the node (ADR 009
// Letting go, in a pod), as ClaudeCode.runDetached: `codex exec` is
// started detached in the session's pod (Session.StartDetached) with
// its prompt, output and stderr on the session directory's files, and
// state.json names the adapter (codex), the pod, the container and the
// HOME — and the auth.json the login was copied from and the cache a
// refreshed one is kept in (LoginFile, LoginCache), paths only, never a
// secret. A cancelled ctx — the node shutting down — returns ctx.Err()
// at once with the container, pod, HOME and session directory left
// running (Session.Detach), and left true: the home's token file and
// login are the adopting node's (Attach). Any other end leaves the pod
// and HOME to whoever opened sess, the login written back by the
// caller's home.close.
func (c *Codex) runDetached(ctx context.Context, job Job, sess *sandbox.Session, home *codexHome, env, secrets []string, onProgress func(Progress)) (res Result, err error, left bool) {
	parent := ctx
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}
	dir, files, err := openSessionDir(job, "")
	if err != nil {
		return Result{}, fmt.Errorf("harness codex: session dir: %w", err), false
	}
	files.close()
	// the container reads its prompt from a file, never a pipe the
	// node's exit would break; it goes once the agent has spoken
	prompt := filepath.Join(dir, sessionunit.PromptFile)
	if err := os.WriteFile(prompt, []byte(job.Prompt), 0o600); err != nil {
		os.RemoveAll(dir)
		return Result{}, fmt.Errorf("harness codex: prompt: %w", err), false
	}
	bin := c.SandboxBinary
	if bin == "" {
		bin = "codex"
	}
	agent, err := sess.StartDetached(parent, sandbox.DetachedSpec{
		Workspace:  job.Workspace,
		Env:        env,
		Command:    append([]string{bin}, c.Args(job)...),
		Timeout:    job.Timeout,
		StdinFile:  prompt,
		StdoutFile: filepath.Join(dir, sessionunit.OutputFile),
		StderrFile: filepath.Join(dir, sessionunit.StderrFile),
	})
	if err != nil {
		os.RemoveAll(dir)
		return Result{}, fmt.Errorf("harness codex: %s", Redact(err.Error(), secrets...)), false
	}
	st := job.state(sessionunit.SandboxRunner, sessionunit.Handle{UnitName: agent.Name}, c.Name())
	st.Pod, st.Container, st.Home = agent.Pod.Name, agent.Name, sess.Home
	st.TokenFile, st.LoginFile = home.tokenFile, home.login
	st.RelaySocket, st.RelayURL = home.relaySocket, home.relayURL
	end := &unitEnd{h: sessionunit.Handle{UnitName: agent.Name}, agent: &podAgent{
		rt: sess.Runtime(), sess: sess, name: agent.Name, started: st.StartedAt,
		timeout: job.Timeout, stderr: filepath.Join(dir, sessionunit.StderrFile),
	}}
	if err := sessionunit.WriteState(dir, st); err != nil {
		// never followed: the container stops and the pod goes with sess
		end.kill()
		os.RemoveAll(dir)
		return Result{}, fmt.Errorf("harness codex: state: %w", err), false
	}
	var spoke sync.Once
	res, detached, waitErr, errBuf, rawOut, failure := c.follow(ctx, parent, end, dir, &st, 0, job, secrets, onProgress, func() {
		spoke.Do(func() { os.Remove(prompt) })
	})
	if detached {
		sess.Detach()
		return res, parent.Err(), true
	}
	os.Remove(prompt)
	// the pod and HOME go with sess, closed by whoever opened it: the
	// state no longer names them for a later node to remove
	st.Pod, st.Container, st.Home = "", "", ""
	_ = sessionunit.WriteState(dir, st)
	if le := (*sandbox.LimitError)(nil); errors.As(waitErr, &le) && ctx.Err() == nil {
		return res, le, false
	}
	res, err = codexEnd(ctx, res, failure, waitErr, errBuf, rawOut, secrets, home.dir)
	return res, err, false
}

// Attach implements Attacher for a Codex session an earlier node
// process left running in its pod (runDetached): alive while its
// container runs, output.jsonl followed from the start and parsed as
// Codex's stream with progress past the saved offset, the container
// stopped at the state's own timeout from startedAt. At the end — not
// a detach — a login the session refreshed is written back to the
// state's LoginFile and LoginCache, then the pod and HOME are removed
// through Sandbox.Reattach (unless WithPodKept). A host Codex session
// never outlives its node, so a state naming no pod is refused.
func (c *Codex) Attach(ctx context.Context, dir string, onProgress func(Progress)) (Result, error) {
	st, err := sessionunit.ReadState(dir)
	if err != nil {
		return Result{}, fmt.Errorf("harness codex: attach: %w", err)
	}
	if st.Runner != sessionunit.SandboxRunner {
		return Result{}, fmt.Errorf("harness codex: attach: session %s ran on the host (%s); a host Codex session ends with its node", st.Label, st.Runner)
	}
	if c.Sandbox == nil {
		return Result{}, fmt.Errorf("harness codex: attach: session %s runs in pod %s, and this node has no sandbox", st.Label, st.Pod)
	}
	sess := c.Sandbox.Reattach(sandbox.Pod{Name: st.Pod}, st.Home, st.Label)
	codexHome := sess.CodexHome()
	// what the session's login holds is redacted from what it says
	var secrets []string
	for _, p := range []string{st.LoginFile, filepath.Join(codexHome, "auth.json")} {
		if p == "" {
			continue
		}
		if b, err := os.ReadFile(p); err == nil {
			secrets = append(secrets, loginSecrets(b)...)
		}
	}
	release := keepAdoptedToken(c.Tokens, c.Stderr, c.Name(), st)
	timeout := time.Duration(st.TimeoutSeconds) * time.Second
	parent := ctx
	if timeout > 0 {
		// the session's own wall clock, from when it started
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, st.StartedAt.Add(timeout))
		defer cancel()
	}
	end := &unitEnd{h: sessionunit.Handle{UnitName: st.Container}, agent: &podAgent{
		rt: c.Sandbox.Runtime, sess: sess, name: st.Container, started: st.StartedAt,
		timeout: timeout, stderr: filepath.Join(dir, sessionunit.StderrFile),
	}}
	job := Job{Model: st.Model, Timeout: timeout}
	res, detached, waitErr, errBuf, rawOut, failure := c.follow(ctx, parent, end, dir, &st, st.Offset, job, secrets, onProgress, nil)
	release(!detached)
	if detached {
		return res, parent.Err()
	}
	os.Remove(filepath.Join(dir, sessionunit.PromptFile))
	if err := writeBackLogin(codexHome, st.LoginFile, st.LoginCache); err != nil && c.Stderr != nil {
		fmt.Fprintf(c.Stderr, "harness codex: attach: write back the refreshed login: %v\n", err)
	}
	// the session limits are read from the HOME before it goes
	res, err = codexEnd(ctx, res, failure, waitErr, errBuf, rawOut, secrets, codexHome)
	if (st.Pod != "" || st.Home != "") && !podKept(ctx) {
		sess.CloseAndLog()
		st.Pod, st.Container, st.Home = "", "", ""
		_ = sessionunit.WriteState(dir, st)
	}
	if le := (*sandbox.LimitError)(nil); errors.As(waitErr, &le) && ctx.Err() == nil {
		return res, le
	}
	return res, err
}

// follow reads a detached Codex session in dir to its end, as
// ClaudeCode.follow reads Claude's (followUnit): the stream parsed by
// readStreamWatch at job's model, spoke called after every event, and
// the turn.failed message returned beside it.
func (c *Codex) follow(ctx, parent context.Context, end *unitEnd, dir string, st *sessionunit.State, from int64, job Job, secrets []string, onProgress func(Progress), spoke func()) (res Result, detached bool, waitErr error, errBuf *bytes.Buffer, rawOut *capBuffer, failure string) {
	var stderr io.Writer
	if c.Stderr != nil {
		stderr = &redactWriter{w: c.Stderr, secrets: secrets}
	}
	parse := func(r io.Reader, progress func(Progress), each func(sessionID string)) Result {
		var res Result
		res, failure = c.readStreamWatch(r, job, progress, func(ev CodexEvent) {
			if spoke != nil {
				spoke()
			}
			each(ev.ThreadID)
		})
		return res
	}
	res, detached, waitErr, errBuf, rawOut = followUnit(ctx, parent, true, end, dir, st, from, stderr, onProgress, parse, c.ended)
	return res, detached, waitErr, errBuf, rawOut, failure
}

// ended puts the container's stop on res, as ClaudeCode.ended: a stop
// that failed is never the session's failure.
func (c *Codex) ended(e *unitEnd, res Result) Result {
	e.kill()
	res.Killed = e.killed
	if e.err != nil {
		msg := fmt.Sprintf("stop container %s: %v", e.h.UnitName, e.err)
		if c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "harness codex: %s\n", msg)
		} else {
			res.KillError = msg
		}
	}
	return res
}

// writeBackLogin writes the auth.json in codexHome back to loginFile and
// loginCache when it is a JSON object other than what loginFile holds:
// the login a session refreshed, kept for the next session on its
// credential by a node that did not start this one. Either path empty
// is skipped; a login unchanged writes nothing.
func writeBackLogin(codexHome, loginFile, loginCache string) error {
	if loginFile == "" && loginCache == "" {
		return nil
	}
	now, err := os.ReadFile(filepath.Join(codexHome, "auth.json"))
	if err != nil || len(now) == 0 || len(loginSecrets(now)) == 0 {
		return nil
	}
	if orig, err := os.ReadFile(loginFile); err == nil && bytes.Equal(now, orig) {
		return nil
	}
	var errs []error
	for _, p := range []string{loginFile, loginCache} {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			errs = append(errs, err)
			continue
		}
		tmp := p + ".mesh-" + strconv.Itoa(os.Getpid())
		if err := os.WriteFile(tmp, now, 0o600); err != nil {
			os.Remove(tmp)
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(tmp, p); err != nil {
			os.Remove(tmp)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
