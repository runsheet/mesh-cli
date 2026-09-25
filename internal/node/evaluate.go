package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Evaluator is the second fresh session an item gets on the node
// (mesh-specs trd §3.4): the finished branch checked out, its diff from
// the base, the acceptance, an adversarial review — and its verdict
// recorded with EvaluateWorkItem. The platform opens the pull request on
// a pass and readies the item again on a fail (with the notes for the
// next builder), so the evaluator changes nothing itself.
type Evaluator struct {
	GW       Gateway
	Harness  harness.Harness
	WS       Workspacer
	Scopes   Scopes
	WorkRoot string
	// MCP are the MCP servers the session gets (Prologue).
	MCP map[string]any
	// MaxBudgetUSD caps the review session (3 by default).
	MaxBudgetUSD float64
	// Model is what an evaluator session runs on ("" the CLI's default).
	Model string
	// Accounts are the node's Claude accounts (node.yaml); a session runs
	// on the first, or on the operator's own login when there are none.
	Accounts []AccountConfig
	// Timeout caps its wall clock (0 none).
	Timeout time.Duration
	// Fallback is the node's one old-platform notice (nil: every item).
	Fallback *TokenFallback
	Log      *log.Logger
}

func (e *Evaluator) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Printf(format, args...)
	}
}

// Evaluate reviews one item in evaluating and records the verdict. An
// error means no verdict was recorded: the item stays in evaluating and
// the next tick tries again. That holds when GitHub refused the item's
// token through every retry too: the item is neither failed nor released
// (the platform releases only a claimed or running item), the node logs
// the credential reason and a later tick evaluates it on a fresh token.
func (e *Evaluator) Evaluate(ctx context.Context, item gateway.WorkItem) error {
	res, err := Resolve(ctx, e.GW, item)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	branch := item.Branch
	if branch == "" {
		branch = res.Branch()
	}
	spec := WorkspaceSpec{Root: e.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: branch, BaseRef: res.BaseRef()}
	// the item's own GitHub token, as its builder had
	cred, err := NewItemCred(ctx, e.GW, e.WS, e.Scopes, item, spec, e.Fallback, e.logf)
	if err != nil {
		return err
	}
	dir, err := PrepareWithRetry(ctx, e.WS, spec, cred, e.logf)
	if ce := (*CredentialError)(nil); errors.As(err, &ce) {
		e.logf("item %s: not evaluated, the node's condition: %s", label, ce.Reason())
		return fmt.Errorf("workspace: %w", ce)
	}
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	diff, err := e.WS.Diff(ctx, dir, spec.BaseRef)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}
	// the same context the builder had: the ADRs it was bound by
	orgLogin := e.Scopes.LoginOf(item.OrgID)
	cx, err := FetchContext(ctx, e.GW, orgLogin, res.Project.Slug)
	if err != nil {
		e.logf("item %s: context unavailable: %v", label, err)
		cx = &Context{}
	}
	AddLive(ctx, e.GW, cx, item.OrgID, res.Project.ID, e.logf)
	files, err := WriteContext(dir, cx)
	if err != nil {
		return fmt.Errorf("write context: %w", err)
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || len(cx.Live) > 0 {
		contextDir = ContextDir
	}
	prompt := harness.EvaluatePrompt(harness.EvaluateInput{
		GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, GoalAcceptance: res.Goal.Acceptance,
		Seq: item.Seq, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: branch, BaseRef: spec.BaseRef, Diff: diff,
		ContextDir: contextDir, ContextFiles: files, Project: orgLogin + "/" + res.Project.Slug, Prologue: len(e.MCP) > 0,
	})
	budget := e.MaxBudgetUSD
	if budget <= 0 {
		budget = 3
	}
	job := harness.Job{Kind: harness.KindEvaluate, Workspace: dir, Prompt: prompt, MaxBudgetUSD: budget, Label: label + " evaluate", Timeout: e.Timeout, MCPServers: e.MCP, Model: e.Model}
	if tok := cred.Token(); tok != nil {
		ghDir, err := NewGHConfigDir()
		if err != nil {
			return fmt.Errorf("gh config dir: %w", err)
		}
		defer os.RemoveAll(ghDir)
		job.Env = SessionEnv(tok, ghDir)
	}
	acct, err := OpenAccount(ctx, e.Accounts)
	if err != nil {
		return err
	}
	defer acct.Cleanup()
	acct.Apply(&job)
	e.logf("item %s: evaluating %s (%d bytes of diff)", label, branch, len(diff))
	result, err := e.Harness.Run(ctx, job, nil)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("evaluator session ran out of time (%s)", e.Timeout)
	}
	if err != nil {
		return fmt.Errorf("evaluator session: %w", acct.RedactErr(err))
	}
	result.Text = acct.Redact(result.Text)
	passed, notes, ok := harness.ParseVerdict(result.Text)
	if !ok {
		return fmt.Errorf("evaluator session reached no verdict (%s): %s", result.SessionID, truncate(result.Text, 300))
	}
	verdict := "failed"
	if passed {
		verdict = "passed"
	}
	notes = strings.TrimSpace(notes)
	if notes == "" {
		notes = "(no notes)"
	}
	if err := e.GW.EvaluateWorkItem(ctx, item.ID, passed, truncate(notes, 4000), Cents(result.CostUSD)); err != nil {
		return fmt.Errorf("record verdict: %w", err)
	}
	e.logf("item %s: evaluation %s ($%.2f)", label, verdict, result.CostUSD)
	return nil
}

// Cents rounds a dollar cost to whole cents.
func Cents(usd float64) int {
	if usd <= 0 {
		return 0
	}
	return int(math.Round(usd * 100))
}
