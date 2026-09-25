package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Rebaser is a node's session of kind rebase (runsheet ADR 008 step 2,
// trd §6a): an item whose pull request conflicts with its base in a way
// the integrator could not resolve itself, claimed with
// ClaimWorkItemRebase. The session checks out the branch in the item's
// own checkout, rebases it onto the base keeping both sides' intent,
// re-runs the acceptance and pushes with --force-with-lease; the node
// reads the pushed head back and records it with FinishWorkItemRebase —
// or gives the rebase up with FailWorkItemRebase and the reason, which
// hands the item to its builder. Every way out ends the claim: a claim
// left held waits out the platform's 15-minute stall timer.
type Rebaser struct {
	GW       Gateway
	Harness  harness.Harness
	WS       Workspacer
	Scopes   Scopes
	WorkRoot string
	// MCP are the MCP servers the session gets (Prologue).
	MCP map[string]any
	// MaxBudgetUSD caps the session (5 by default).
	MaxBudgetUSD float64
	// Model is what a rebase session runs on ("" the CLI's default).
	Model string
	// Timeout caps its wall clock (0 none).
	Timeout time.Duration
	// ProgressEvery is how often the rebase's heartbeat is sent (60 s):
	// ReportWorkItemProgress while the rebase is claimed, with the
	// session's cents since the last one.
	ProgressEvery time.Duration
	// Fallback is the node's one old-platform notice (nil: every item).
	Fallback *TokenFallback
	Log      *log.Logger
}

func (r *Rebaser) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

// Rebase runs one claimed rebase to its end. The error is what went
// wrong, already handed in as the rebase's failure when it could be.
func (r *Rebaser) Rebase(ctx context.Context, item gateway.WorkItem) error {
	cost := &costReporter{}
	label := item.ID // the goal's slug and the seq once resolved
	// the session's cents go on the heartbeat: finish and fail carry none
	report := func(ctx context.Context, note, sessionID string) {
		if err := r.GW.ReportWorkItemProgress(ctx, item.ID, truncate(note, 2000), cost.delta(), sessionID); err != nil {
			r.logf("item %s: rebase progress: %v", label, err)
		}
	}
	fail := func(cause error) error {
		// a node stopping mid-session still hands the rebase back,
		// rather than holding it until the platform's timer does
		fctx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			fctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
		}
		reason := truncate(cause.Error(), 4000)
		r.logf("item %s: rebase failed: %s", label, reason)
		report(fctx, "rebase failed", "")
		if err := r.GW.FailWorkItemRebase(fctx, item.ID, reason); err != nil {
			r.logf("item %s: fail rebase: %v", label, err)
		}
		return cause
	}

	res, err := Resolve(ctx, r.GW, item)
	if err != nil {
		return fail(fmt.Errorf("resolve: %w", err))
	}
	label = fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	branch := item.Branch
	if branch == "" {
		branch = res.Branch()
	}
	spec := WorkspaceSpec{Root: r.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: branch, BaseRef: res.BaseRef()}
	// the item's own GitHub token: the push is the session's whole point
	cred, err := NewItemCred(ctx, r.GW, r.WS, r.Scopes, item, spec, r.Fallback, r.logf)
	if err != nil {
		return fail(err)
	}
	dir, err := r.WS.Prepare(ctx, spec)
	if err != nil {
		return fail(fmt.Errorf("workspace: %w", err))
	}
	orgLogin := r.Scopes.LoginOf(item.OrgID)
	cx, err := FetchContext(ctx, r.GW, orgLogin, res.Project.Slug)
	if err != nil {
		r.logf("item %s: context unavailable: %v", label, err)
		cx = &Context{}
	}
	files, err := WriteContext(dir, cx)
	if err != nil {
		return fail(fmt.Errorf("write context: %w", err))
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 {
		contextDir = ContextDir
	}
	prompt := harness.RebasePrompt(harness.RebaseInput{
		GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, Seq: item.Seq, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: branch, BaseRef: spec.BaseRef, Conflict: item.RebaseConflict, Files: item.RebaseFiles,
		ContextDir: contextDir, ContextFiles: files, Project: orgLogin + "/" + res.Project.Slug, Prologue: len(r.MCP) > 0,
	})
	budget := r.MaxBudgetUSD
	if budget <= 0 {
		budget = 5
	}
	job := harness.Job{Kind: harness.KindRebase, Workspace: dir, Prompt: prompt, MaxBudgetUSD: budget, Label: label + " rebase", Timeout: r.Timeout, MCPServers: r.MCP, Model: r.Model}
	if tok := cred.Token(); tok != nil {
		ghDir, err := NewGHConfigDir()
		if err != nil {
			return fail(fmt.Errorf("gh config dir: %w", err))
		}
		defer os.RemoveAll(ghDir)
		job.Env = SessionEnv(tok, ghDir)
	}

	var mu sync.Mutex
	sessionID := ""
	sid := func() string {
		mu.Lock()
		defer mu.Unlock()
		return sessionID
	}
	r.logf("item %s: rebasing %s onto %s", label, branch, spec.BaseRef)
	report(ctx, "rebase session started: "+branch+" onto "+spec.BaseRef, "")
	every := r.ProgressEvery
	if every <= 0 {
		every = 60 * time.Second
	}
	tickCtx, stopTicker := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-t.C:
				report(tickCtx, "rebasing "+branch, sid())
				if err := cred.Refresh(tickCtx); err != nil && tickCtx.Err() == nil {
					r.logf("item %s: %v", label, err)
				}
			}
		}
	}()
	result, runErr := r.Harness.Run(ctx, job, func(p harness.Progress) {
		if p.SessionID != "" {
			mu.Lock()
			sessionID = p.SessionID
			mu.Unlock()
		}
		if p.CostUSD > 0 {
			cost.observe(p.CostUSD)
		}
	})
	stopTicker()
	cost.observe(result.CostUSD)
	if result.SessionID != "" {
		mu.Lock()
		sessionID = result.SessionID
		mu.Unlock()
	}
	switch {
	case ctx.Err() != nil:
		return fail(errors.New("the node stopped while the rebase session ran"))
	case errors.Is(runErr, context.DeadlineExceeded):
		return fail(fmt.Errorf("the rebase session ran out of time (%s)", r.Timeout))
	case runErr != nil:
		return fail(fmt.Errorf("rebase session: %w", runErr))
	}
	out, ok := harness.ParseRebase(result.Text)
	if !ok {
		return fail(fmt.Errorf("the rebase session reached no result (%s): %s", result.SessionID, truncate(result.Text, 1000)))
	}
	if !out.Rebased {
		return fail(errors.New(out.Reason))
	}
	// what origin has is the head, not what the session says it pushed
	head, err := r.WS.PushedHead(ctx, dir, branch, spec.BaseRef)
	if err != nil {
		return fail(fmt.Errorf("the rebase session said it pushed %s, but: %w", short(out.Head), err))
	}
	if !strings.HasPrefix(strings.ToLower(head), out.Head) {
		return fail(fmt.Errorf("the rebase session said it pushed %s, but origin/%s is at %s: the push did not land", short(out.Head), branch, short(head)))
	}
	report(ctx, fmt.Sprintf("rebased %s onto %s at %s", branch, spec.BaseRef, short(head)), sid())
	if err := r.GW.FinishWorkItemRebase(ctx, item.ID, head, out.Files); err != nil {
		// a finish the platform did not take still ends the claim
		return fail(fmt.Errorf("the rebase landed at %s, but finishing it failed: %w", short(head), err))
	}
	r.logf("item %s: rebased %s@%s, resolved %d file(s) ($%.2f)", label, branch, short(head), len(out.Files), result.CostUSD)
	return nil
}
