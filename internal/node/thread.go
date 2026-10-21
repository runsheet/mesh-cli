package node

// A thread turn on the node (runsheet ADR 061 §7, runsheet/goal-chat): a
// person's message on a product's thread, raised as a job a node claims
// and answers. The node claims the turn (claimJob), opens its job's
// directory, its account and tools from the one exchange (jobAccess,
// gateway.Session's thread read), runs the role's session in the turn's
// pod with no code, and hands back what the session said — an answer,
// and a proposal when it drafted one (finishJob). A turn that cannot be
// run is still answered, saying why, so the thread is never left
// waiting on a node that gave up.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
)

// ThreadKinds are the thread kinds this node runs, and the prompt each
// turn of one is asked with.
var ThreadKinds = map[string]func(harness.ThreadInput) string{
	"runsheet/goal-chat": harness.GoalChatPrompt,
}

// ThreadGateway is what a thread turn needs of the platform.
type ThreadGateway interface {
	ClaimJob(ctx context.Context, in gateway.ClaimJobInput) (*gateway.JobBrief, error)
	FinishJob(ctx context.Context, nodeID, kind string, subject gateway.JobSubject, finish map[string]any) (map[string]any, error)
	Projects(ctx context.Context, orgID string) ([]gateway.Project, error)
}

// ThreadRunner runs thread turns.
type ThreadRunner struct {
	GW          ThreadGateway
	Harness     harness.Harness
	Credentials *Credentials
	Sandbox     *sandbox.Sandbox
	Layout      *Layout
	// Served is the orgs the loop serves now: a node serving two or more
	// runs no turn on the host (openScopePod).
	Served      func() Scopes
	Proxies     *SessionProxies
	MCP         map[string]any
	Runtimes    func() []string
	Model       string
	Timeout     time.Duration
	BudgetUSD   float64
	WorkRoot    string
	BuildCache  string
	Log         interface{ Printf(string, ...any) }
}

func (t *ThreadRunner) logf(format string, args ...any) {
	if t.Log != nil {
		t.Log.Printf(format, args...)
	}
}

// Run claims turn as the job of kind and answers it.
func (t *ThreadRunner) Run(ctx context.Context, turn gateway.ThreadTurn, kind string, sc Scope) error {
	prompter, ok := ThreadKinds[kind]
	if !ok {
		return fmt.Errorf("thread %s: this node runs no %s turns", turn.ThreadKey, kind)
	}
	subject := gateway.JobSubject{Type: "thread", ID: turn.ID}
	adapter := Adapter(t.Harness)
	brief, err := t.GW.ClaimJob(ctx, gateway.ClaimJobInput{NodeID: sc.NodeID, Kind: kind, Subject: subject, Harness: adapter})
	if err != nil {
		if gateway.IsRefusal(err) {
			return nil // another node took it, or it is no longer waiting
		}
		return fmt.Errorf("thread %s: claim: %w", turn.ThreadKey, err)
	}
	label := kind + " " + turn.ThreadKey
	t.logf("%s: answering %s", sc.Name(), label)
	reply, err := t.answer(ctx, turn, kind, sc, brief, prompter, label)
	if err != nil {
		t.logf("%s: %v; answering that", label, err)
		reply = harness.ThreadReply{Answer: "I couldn't answer this time: " + truncate(err.Error(), 400)}
	}
	finish := map[string]any{"answer": reply.Answer}
	if reply.Answer == "" {
		delete(finish, "answer")
	}
	if reply.Proposal != "" {
		finish["proposal"] = reply.Proposal
	}
	if _, err := t.GW.FinishJob(ctx, sc.NodeID, kind, subject, finish); err != nil {
		return fmt.Errorf("%s: finish: %w", label, err)
	}
	what := "answered"
	if reply.Proposal != "" {
		what = "answered with a proposal"
	}
	t.logf("%s: %s", label, what)
	return nil
}

// answer runs the turn's session and reads its reply.
func (t *ThreadRunner) answer(ctx context.Context, turn gateway.ThreadTurn, kind string, sc Scope, brief *gateway.JobBrief, prompter func(harness.ThreadInput) string, label string) (harness.ThreadReply, error) {
	if brief.Thread == nil {
		return harness.ThreadReply{}, errors.New("the brief carries no thread")
	}
	tr, err := harness.ParseThreadTranscript(brief.Thread.Body)
	if err != nil {
		return harness.ThreadReply{}, err
	}
	projectID, projectSlug := brief.Thread.ProjectID, ""
	if projectID != "" {
		if ps, err := t.GW.Projects(ctx, sc.ID); err == nil {
			for _, p := range ps {
				if p.ID == projectID {
					projectSlug = p.Slug
				}
			}
		}
	}
	workspace, jobID := filepath.Join(t.WorkRoot, "threads", turn.ID), turn.ID
	if t.Layout != nil {
		thread, _, _ := strings.Cut(brief.Thread.Ask, "#")
		j, err := openJob(t.Layout, Scopes{sc}, sc.ID, JobMeta{ID: jobID, Unit: JobThread, ThreadID: thread, Label: label})
		if err != nil {
			return harness.ThreadReply{}, err
		}
		workspace = filepath.Join(j.Dir, "work")
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return harness.ThreadReply{}, err
	}
	acct, err := t.Credentials.Open(withSubject(ctx, sc, projectID, projectSlug),
		gateway.SessionRef{Kind: string(harness.KindThread), ThreadID: turn.ID, JobKind: kind}, sc.NodeID, Adapter(t.Harness))
	if err != nil {
		return harness.ThreadReply{}, fmt.Errorf("no model account: %w", err)
	}
	acct.SetLabel(label, string(harness.KindThread))
	defer acct.Cleanup()
	var runtimes []string
	if t.Runtimes != nil {
		runtimes = t.Runtimes()
	}
	st := SelectTools(t.MCP, acct.Tools, tools.RoleFor(string(harness.KindThread)), runtimes)
	if len(st.Missing) > 0 {
		return harness.ThreadReply{}, errors.New("declined, " + st.DeclineReason())
	}
	prompt := prompter(harness.ThreadInput{Org: sc.Name(), Project: projectSlug, Transcript: tr, MeshContext: len(t.MCP) > 0, Tools: toolNames(st)})
	kept, err := keptRootFor(t.Layout, jobID)
	if err != nil {
		return harness.ThreadReply{}, err
	}
	budget := t.BudgetUSD
	if budget <= 0 {
		budget = 2
	}
	job := harness.Job{Kind: harness.KindThread, Workspace: workspace, Prompt: prompt, MaxBudgetUSD: budget, Label: label,
		Timeout: t.Timeout, MCPServers: t.MCP, Model: t.Model, KeptRoot: kept}
	scratchRoot, err := scratchRootFor(t.Layout, jobID, t.WorkRoot)
	if err != nil {
		return harness.ThreadReply{}, fmt.Errorf("session scratch: %w", err)
	}
	pod, err := openScopePod(ctx, t.Sandbox, t.Layout, servedOf(t.Served), sc, workspace, label, harness.KindThread, t.logf)
	defer pod.Close()
	if err != nil {
		if errors.Is(err, ErrHostServesSeveralOrgs) {
			noteHostFallback(err, t.logf)
		}
		return harness.ThreadReply{}, err
	}
	pod.Apply(&job)
	scratch, err := sessionScratch(t.Sandbox, scratchRoot, t.BuildCache, harness.KindThread, jobID, &job)
	if err != nil {
		return harness.ThreadReply{}, fmt.Errorf("session scratch: %w", err)
	}
	defer RemoveScratch(scratch)
	acct.Apply(&job)
	closeProxy, err := st.UseVia(t.Proxies, mcpRootFor(t.Layout, jobID), &job, t.MCP, acct)
	if err != nil {
		return harness.ThreadReply{}, fmt.Errorf("tools: %w", err)
	}
	defer closeProxy(ctx)
	job.Meta = harness.SessionMeta{NodeID: sc.NodeID, TempDirs: sessionTempDirs(job, acct)}
	result, err := t.Harness.Run(ctx, job, nil)
	defer acct.Finish(ctx, result)
	defer harness.RemoveSession(workspace)
	if err != nil {
		return harness.ThreadReply{}, fmt.Errorf("session: %w", err)
	}
	if !result.Success {
		return harness.ThreadReply{}, fmt.Errorf("the session ended unsuccessfully: %s", truncate(result.Text, 300))
	}
	return harness.ParseThreadReply(result.Text)
}

// toolNames are the session's served tools by name, for its prompt.
func toolNames(st SessionTools) []string {
	names := make([]string, len(st.Use))
	for i, t := range st.Use {
		names[i] = t.Name
	}
	return names
}

// threadLister is the platform's read of raised turns and of the job
// kinds (*gateway.Client).
type threadLister interface {
	RaisedThreadTurns(ctx context.Context, orgID, product string) ([]gateway.ThreadTurn, error)
	JobKinds(ctx context.Context) (map[string]gateway.JobKind, error)
}

// threadKindsEvery is how often the loop reads the job-kind catalogue
// again.
const threadKindsEvery = 10 * time.Minute

// jobKinds is the catalogue, read again past threadKindsEvery; a failed
// read keeps the last one.
func (l *Loop) jobKinds(ctx context.Context, tl threadLister) map[string]gateway.JobKind {
	l.mu.Lock()
	kinds, at := l.threadKinds, l.threadKindsAt
	l.mu.Unlock()
	if kinds != nil && time.Since(at) < threadKindsEvery {
		return kinds
	}
	got, err := tl.JobKinds(ctx)
	if err != nil {
		l.logf("job kinds: %v", err)
		return kinds
	}
	l.mu.Lock()
	l.threadKinds, l.threadKindsAt = got, time.Now()
	l.mu.Unlock()
	return got
}

// threadsDue takes every raised turn of the served orgs whose kind this
// node runs (ThreadKinds), each in its own session: a turn is a person
// waiting, so it is taken on the tick that sees it, under the
// connection's cap and an account with headroom like any session.
func (l *Loop) threadsDue(ctx context.Context) {
	if l.Thread == nil || !l.takesKind(harness.KindThread) {
		return
	}
	tl, ok := l.GW.(threadLister)
	if !ok {
		return
	}
	kinds := l.jobKinds(ctx, tl)
	for _, sc := range l.Scopes {
		turns, err := tl.RaisedThreadTurns(ctx, sc.ID, "")
		if err != nil {
			l.logf("list thread turns of %s: %v", sc.Name(), err)
			continue
		}
		for _, turn := range turns {
			kind := turn.KindIn(kinds)
			if _, runs := ThreadKinds[kind]; !runs {
				continue
			}
			if why, at := atConnectionCap(sc, l.heldByOrg()); at {
				l.passOver(turn.ID, "thread", "", why, time.Time{})
				continue
			}
			actx, ok := l.accountFor(ctx, "", false, "")
			if !ok {
				continue
			}
			l.mu.Lock()
			if l.threading == nil {
				l.threading = map[string]bool{}
			}
			if l.threading[turn.ID] {
				l.mu.Unlock()
				continue
			}
			l.threading[turn.ID] = true
			l.mu.Unlock()
			l.countFor(turn.ID, sc.ID)
			l.wg.Add(1)
			go func(ctx context.Context, turn gateway.ThreadTurn, kind string, sc Scope) {
				defer crash.Guard()
				defer l.wg.Done()
				defer func() {
					l.mu.Lock()
					delete(l.threading, turn.ID)
					delete(l.sessionOrg, turn.ID)
					l.mu.Unlock()
					l.notify()
				}()
				l.Thread(ctx, turn, kind, sc)
			}(actx, turn, kind, sc)
		}
	}
}
