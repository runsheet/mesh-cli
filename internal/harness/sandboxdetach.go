package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// runDetached is runSandboxed for a job that outlives the node (ADR 009
// Letting go, in a pod): the agent's container is started detached
// (Session.StartDetached) with its prompt, output and stderr on the
// session directory's files, the state file names its pod, container
// and HOME, and the output is followed as a host session's is. A
// cancelled ctx — the node shutting down — returns ctx.Err() at once
// with the container, pod, HOME and session directory left running
// (Session.Detach); only the job's own Timeout, or ErrStopSession,
// stops the container. Any other end leaves the pod and HOME to
// whoever opened sess, and the session directory to RemoveSession.
func (c *ClaudeCode) runDetached(ctx context.Context, job Job, sess *sandbox.Session, mcpPath string, onProgress func(Progress)) (Result, error) {
	parent := ctx
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}
	dir, files, err := openSessionDir(job)
	if err != nil {
		return Result{}, fmt.Errorf("harness claude-code: session dir: %w", err)
	}
	files.close()
	// the container reads its prompt from a file, never a pipe the
	// node's exit would break; it goes once the agent has spoken
	prompt := filepath.Join(dir, sessionunit.PromptFile)
	if err := os.WriteFile(prompt, []byte(job.Prompt), 0o600); err != nil {
		os.RemoveAll(dir)
		return Result{}, fmt.Errorf("harness claude-code: prompt: %w", err)
	}
	bin := c.SandboxBinary
	if bin == "" {
		bin = "claude"
	}
	agent, err := sess.StartDetached(parent, sandbox.DetachedSpec{
		Workspace:  job.Workspace,
		Env:        job.Env,
		Command:    append([]string{bin}, c.SandboxArgs(job, mcpPath)...),
		Timeout:    job.Timeout,
		StdinFile:  prompt,
		StdoutFile: filepath.Join(dir, sessionunit.OutputFile),
		StderrFile: filepath.Join(dir, sessionunit.StderrFile),
	})
	if err != nil {
		os.RemoveAll(dir)
		return Result{}, fmt.Errorf("harness claude-code: %w", err)
	}
	st := job.state(sessionunit.SandboxRunner, sessionunit.Handle{UnitName: agent.Name}, c.Name())
	st.Pod, st.Container, st.Home = agent.Pod.Name, agent.Name, sess.Home
	end := &unitEnd{h: sessionunit.Handle{UnitName: agent.Name}, agent: &podAgent{
		rt: sess.Runtime(), sess: sess, name: agent.Name, started: st.StartedAt,
		timeout: job.Timeout, stderr: filepath.Join(dir, sessionunit.StderrFile),
	}}
	if err := sessionunit.WriteState(dir, st); err != nil {
		// never followed: the container stops and the pod goes with sess
		end.kill()
		os.RemoveAll(dir)
		return Result{}, fmt.Errorf("harness claude-code: state: %w", err)
	}
	var spoke sync.Once
	res, detached, waitErr, errBuf, rawOut := c.follow(ctx, parent, true, end, dir, &st, 0, job.Secrets, onProgress, func(Event) {
		spoke.Do(func() { os.Remove(prompt) })
	})
	if detached {
		sess.Detach()
		return res, parent.Err()
	}
	os.Remove(prompt)
	// the pod and HOME go with sess, closed by whoever opened it: the
	// state no longer names them for a later node to remove
	st.Pod, st.Container, st.Home = "", "", ""
	_ = sessionunit.WriteState(dir, st)
	if le := (*sandbox.LimitError)(nil); errors.As(waitErr, &le) && ctx.Err() == nil {
		return res, le
	}
	return finishClaude(ctx, job, res, waitErr, errBuf, rawOut)
}

// attachSandboxed is Attach for a session whose agent runs detached in
// its pod: alive while the container runs (Runtime.AgentState), the
// file followed from the start with progress past the saved offset,
// the container stopped at the state's own timeout from startedAt, and
// at the end — not a detach — the pod and HOME removed through
// Sandbox.Reattach, as a host session's end kills its unit.
func (c *ClaudeCode) attachSandboxed(ctx context.Context, dir string, st sessionunit.State, onProgress func(Progress)) (Result, error) {
	if c.Sandbox == nil {
		return Result{}, fmt.Errorf("harness claude-code: attach: session %s runs in pod %s, and this node has no sandbox", st.Label, st.Pod)
	}
	sess := c.Sandbox.Reattach(sandbox.Pod{Name: st.Pod}, st.Home, st.Label)
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
	res, detached, waitErr, errBuf, rawOut := c.follow(ctx, parent, true, end, dir, &st, st.Offset, nil, onProgress, nil)
	if detached {
		return res, parent.Err()
	}
	os.Remove(filepath.Join(dir, sessionunit.PromptFile))
	if st.Pod != "" || st.Home != "" {
		sess.CloseAndLog()
		st.Pod, st.Container, st.Home = "", "", ""
		_ = sessionunit.WriteState(dir, st)
	}
	if le := (*sandbox.LimitError)(nil); errors.As(waitErr, &le) && ctx.Err() == nil {
		return res, le
	}
	return finishClaude(ctx, Job{Timeout: timeout}, res, waitErr, errBuf, rawOut)
}

// RemoveSandboxedSession is RemoveSession for a session that may run in
// a pod: the container its state names is stopped, its pod and HOME
// removed through Sandbox.Reattach, then the session directory and its
// temp dirs go. A state naming no pod, or sb nil, is RemoveSession.
func RemoveSandboxedSession(sb *sandbox.Sandbox, workdir string) error {
	if workdir == "" {
		return nil
	}
	var errs []error
	if st, err := sessionunit.ReadState(sessionunit.Dir(workdir)); err == nil && sb != nil && st.Runner == sessionunit.SandboxRunner && (st.Pod != "" || st.Home != "") {
		a := &podAgent{rt: sb.Runtime, name: st.Container}
		if err := a.stop(); err != nil {
			errs = append(errs, err)
		}
		if err := sb.Reattach(sandbox.Pod{Name: st.Pod}, st.Home, st.Label).Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := RemoveSession(workdir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// podAgent is a detached agent's container read by name: the unit of a
// sandboxed session.
type podAgent struct {
	rt sandbox.Runtime
	// sess classifies its exit as an attached run's (Session.Ended)
	sess    *sandbox.Session
	name    string
	started time.Time
	timeout time.Duration
	// stderr is the session's stderr.log, whose tail names a pids kill
	stderr string
}

// podAgentCall bounds one runtime call on the agent.
const podAgentCall = 30 * time.Second

func (a *podAgent) state() (sandbox.AgentState, error) {
	if a.name == "" {
		return sandbox.AgentState{}, sandbox.ErrNoAgent
	}
	ctx, cancel := context.WithTimeout(context.Background(), podAgentCall)
	defer cancel()
	return a.rt.AgentState(ctx, a.name)
}

// alive is whether the container still runs (or is about to). A
// runtime that cannot answer is taken as running: only the container
// gone, or exited, ends the reading.
func (a *podAgent) alive() bool {
	st, err := a.state()
	if err != nil {
		return !errors.Is(err, sandbox.ErrNoAgent)
	}
	return st.Running || st.Status == "created"
}

// wait polls the container to its end and classifies its exit as Run
// does. A container already removed has no exit to read: its end is
// what the output file says.
func (a *podAgent) wait() error {
	if a.name == "" {
		return nil
	}
	exit, err := sandbox.WaitAgent(context.Background(), a.rt, a.name, sessionunit.PollInterval)
	if errors.Is(err, sandbox.ErrNoAgent) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.sess == nil {
		return nil
	}
	return a.sess.Ended(exit, time.Since(a.started), a.timeout, tailOf(a.stderr, 8<<10))
}

// stop stops the container while it runs; one already ended or gone is
// left for its pod's removal.
func (a *podAgent) stop() error {
	if !a.alive() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), podAgentCall)
	defer cancel()
	return a.rt.StopAgent(ctx, a.name)
}

// tailOf is the last max bytes of the file at path ("" when unreadable).
func tailOf(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		_, _ = f.Seek(fi.Size()-max, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}
