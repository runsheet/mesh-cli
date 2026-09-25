package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Director is the session that acts on one direction of a project
// (runsheet trd §6c): the project's repos checked out read-only, its
// Prologue context, the words of the direction and the goals the
// project has already had; a fresh `claude -p` that reads and then
// writes its proposals as JSON; the proposals handed in against the
// direction with RecordDirectionResearch, the session's cost with them.
//
// The hours, the gap and the ceiling are the platform's: this asks for
// the claim on every tick and is told to wait, the way the loop asks
// for work items. So a refusal here is the normal answer, not an error.
type Director struct {
	GW       Gateway
	Harness  harness.Harness
	WS       Workspacer
	Cfg      *Config
	WorkRoot string
	// MCP are the MCP servers the session gets (Prologue).
	MCP map[string]any
	// MaxBudgetUSD caps the research session (5 by default).
	MaxBudgetUSD float64
	// Model is what a research session runs on ("" the CLI's default).
	Model string
	// Accounts are the node's Claude accounts (node.yaml); a session runs
	// on the first, or on the operator's own login when there are none.
	// They are the fallback to the credentials the platform issues.
	Accounts []AccountConfig
	// Credentials asks the platform for the session's credentials and
	// reports its usage record (nil: node.yaml's accounts, no record).
	Credentials *Credentials
	// Timeout caps its wall clock (0 none).
	Timeout time.Duration
	Log     *log.Logger
}

func (d *Director) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log.Printf(format, args...)
	}
}

// ResearchDir is where one direction's research session works:
// <root>/<project-slug>/research/<direction-slug>. The checkout is the
// direction's, not the project's: two directions of one project may be
// inside their hours at once, and a second session fetching over the
// first one's working tree would pull the ground from under it.
func ResearchDir(root, projectSlug, directionSlug string) string {
	return filepath.Join(root, projectSlug, "research", directionSlug)
}

// Research runs one research session on a direction whose claim this
// node holds, and hands in what it came to against that direction. An
// error means nothing was handed in — the claim goes stale and another
// tick, or another node, tries again.
func (d *Director) Research(ctx context.Context, direction gateway.Direction, project gateway.Project, scope Scope) error {
	all, err := d.GW.Repos(ctx, scope.AccountID, scope.Login)
	if err != nil {
		return fmt.Errorf("repos: %w", err)
	}
	inProject := map[string]bool{}
	for _, id := range project.RepoIDs {
		inProject[id] = true
	}
	dir := ResearchDir(d.WorkRoot, project.Slug, direction.Slug)
	dirs := map[string]string{}
	for _, r := range all {
		if !inProject[r.ID] {
			continue
		}
		sub := filepath.Join("repos", r.Name)
		if err := d.WS.Fetch(ctx, r.FullName(), filepath.Join(dir, sub)); err != nil {
			return fmt.Errorf("fetch %s: %w", r.FullName(), err)
		}
		dirs[r.FullName()] = sub
	}
	if len(dirs) == 0 {
		return fmt.Errorf("project %s has no repos enrolled; nothing to research", project.Slug)
	}
	cx, err := FetchContext(ctx, d.GW, scope.Login, project.Slug)
	if err != nil {
		d.logf("project %s: context unavailable: %v", project.Slug, err)
		cx = &Context{}
	}
	AddLive(ctx, d.GW, cx, scope.ID, project.ID, d.logf)
	files, err := WriteContext(dir, cx)
	if err != nil {
		return fmt.Errorf("write context: %w", err)
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || len(cx.Live) > 0 {
		contextDir = ContextDir
	}
	// what the project has already taken on, and the room left: the
	// platform decides the room for real when the proposals land, but a
	// session told there is room for one proposes its best one rather
	// than five that are thrown away
	goals, err := d.GW.Goals(ctx, scope.ID)
	if err != nil {
		d.logf("project %s: goals so far unavailable: %v", project.Slug, err)
	}
	var existing []harness.ExistingGoal
	for _, g := range goals {
		if g.ProjectID != project.ID {
			continue
		}
		existing = append(existing, harness.ExistingGoal{
			Slug: g.Slug, Title: g.Title, Status: g.Status, Note: truncate(g.PausedReason, 200),
		})
	}
	room := RoomFor(direction, project, goals)
	prompt := harness.ResearchPrompt(harness.ResearchInput{
		Org: scope.Login, Project: project.Slug, Direction: direction.Direction,
		Repos: dirs, ContextDir: contextDir, ContextFiles: files, Prologue: len(d.MCP) > 0,
		Goals: existing, Room: room,
	})
	budget := d.MaxBudgetUSD
	if budget <= 0 {
		budget = 5
	}
	job := harness.Job{
		Kind: harness.KindResearch, Workspace: dir, Prompt: prompt, MaxBudgetUSD: budget,
		Label: project.Slug + "/" + direction.Slug + " research", Timeout: d.Timeout, MCPServers: d.MCP, Model: d.Model,
	}
	acct, err := d.Credentials.Open(ctx, gateway.SessionRef{Kind: string(harness.KindResearch), DirectionID: direction.ID}, scope.NodeID, d.Accounts)
	if err != nil {
		return err
	}
	defer acct.Cleanup()
	acct.Apply(&job)
	job.Meta = harness.SessionMeta{NodeID: scope.NodeID, DirectionID: direction.ID}
	d.logf("%s/%s: researching in %s (%d repos, room for %d)", project.Slug, direction.Slug, dir, len(dirs), room)
	result, err := d.Harness.Run(ctx, job, nil)
	acct.Finish(ctx, result)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("research session ran out of time (%s)", d.Timeout)
	}
	if err != nil {
		return fmt.Errorf("research session: %w", acct.RedactErr(err))
	}
	result.Text = acct.Redact(result.Text)
	specs, err := harness.ParseProposals(result.Text)
	if err != nil {
		// the cost is real and the claim must be let go, so hand in an
		// empty session rather than leaving the direction claimed: a
		// node that cannot read its own director would otherwise hold
		// it for 45 minutes and charge for the privilege
		note := fmt.Sprintf("the session's proposals could not be read (%s): %s", result.SessionID, truncate(result.Text, 300))
		if hand := d.GW.RecordDirectionResearch(ctx, direction.ID, scope.NodeID, nil, Cents(result.CostUSD), note); hand != nil {
			d.logf("%s/%s: %v (and handing in the empty session failed: %v)", project.Slug, direction.Slug, err, hand)
		}
		return fmt.Errorf("research session (%s): %w", result.SessionID, err)
	}
	proposals := make([]gateway.GoalProposal, 0, len(specs))
	for _, sp := range specs {
		proposals = append(proposals, gateway.GoalProposal{
			Slug: slugify(sp.Slug), Title: strings.TrimSpace(sp.Title),
			Statement: strings.TrimSpace(sp.Statement), Acceptance: strings.TrimSpace(sp.Acceptance),
			Why: strings.TrimSpace(sp.Why), BudgetCents: sp.BudgetCents,
		})
	}
	notes := fmt.Sprintf("%d proposed", len(proposals))
	if err := d.GW.RecordDirectionResearch(ctx, direction.ID, scope.NodeID, proposals, Cents(result.CostUSD), notes); err != nil {
		return fmt.Errorf("hand in the research: %w", err)
	}
	d.logf("%s/%s: proposed %d goal(s) ($%.2f)", project.Slug, direction.Slug, len(proposals), result.CostUSD)
	return nil
}

// RoomFor is how many more goals a direction has space for, given the
// org's goals. Two ceilings hold and the smaller wins (runsheet trd
// §6c): the direction's own share, less the live goals that direction
// set, and the project's total over every direction, less every live
// goal on it —
//
//	room = min(direction.maxLiveGoals - live goals of this direction,
//	           project.maxLiveGoals   - live goals of the project)
//
// The platform works this out for real when the proposals land; the
// node counts it too, so that it neither claims nor pays for a session
// whose every proposal would be dropped.
func RoomFor(direction gateway.Direction, project gateway.Project, goals []gateway.Goal) int {
	share := ceilingOr(direction.MaxLiveGoals)
	total := ceilingOr(project.MaxLiveGoals)
	for _, g := range goals {
		if g.ProjectID != direction.ProjectID || !isLiveGoal(g.Status) {
			continue
		}
		total--
		// a goal a person set carries no direction and spends nobody's
		// share — it still fills the project
		if g.DirectionID == direction.ID {
			share--
		}
	}
	return max(min(share, total), 0)
}

// ceilingOr is the platform's reading of a ceiling left unset: 0 is
// DefaultMaxLiveGoals, for a direction and for a project alike.
func ceilingOr(n int) int {
	if n <= 0 {
		return DefaultMaxLiveGoals
	}
	return n
}

// DefaultMaxLiveGoals mirrors the platform's ceiling for a direction or
// a project that named none — the node only uses it to decide whether
// to offer a session and to tell it how much room it has; the platform
// decides for real.
const DefaultMaxLiveGoals = 3

// isLiveGoal is the platform's rule (runsheet trd §6c): a goal that is
// not over takes up the project's room, a paused or blocked one
// included — it is waiting for a person.
func isLiveGoal(status string) bool {
	switch status {
	case "draft", "planning", "awaiting_approval", "running", "paused", "blocked":
		return true
	}
	return false
}

// slugify makes a session's slug into the identity the platform keys
// on: lowercase, hyphens, nothing else.
func slugify(s string) string {
	var b strings.Builder
	last := '-'
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			last = r
		default:
			if last != '-' {
				b.WriteRune('-')
				last = '-'
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
