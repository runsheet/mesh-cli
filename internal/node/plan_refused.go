package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// PlanRefused is a plan the node's check refused on its final answer —
// a re-land of a failed item's acceptance, a narrow re-plan that did not
// split, an item outside the goal's repo (ADR-021, ADR-023 (d)). Reason
// is the joined refusal text. Released is set when this refusal was the
// second identical one running and the plan claim was released with it
// (ADR-023, a refused plan is released, not re-planned): logged by the
// planner already.
type PlanRefused struct {
	Goal     string
	Reason   string
	Released bool
}

func (e *PlanRefused) Error() string { return e.Reason }

// PlanRefusedOf finds a *PlanRefused in err's chain.
func PlanRefusedOf(err error) (*PlanRefused, bool) {
	var pr *PlanRefused
	if errors.As(err, &pr) {
		return pr, true
	}
	return nil, false
}

// planHandInRefused is a plan hand-in the platform answered with err, as the plan
// path returns it: a refusal (the gateway's own answer, not a transport
// failure) is a *PlanRefused, so the same refusal twice running releases
// the claim and holds the goal off this node until its row changes —
// persisted, so a fresh Planner per job does not plan it again; anything
// else stays an error, a try counted.
func planHandInRefused(goal gateway.Goal, what string, err error) error {
	if gateway.IsRefusal(err) && !gateway.IsUnsupported(err) {
		return &PlanRefused{Goal: goal.Slug, Reason: fmt.Sprintf("%s: %v", what, err)}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// normaliseRefusal is a refusal as the plan path compares two of them:
// lower-case, whitespace collapsed, trailing punctuation dropped — so a
// refusal re-worded only in case, wrapping or a closing full stop is the
// same refusal (normaliseAcceptance's approach, kept to the ends: the
// item numbers and lineages inside it are what makes two refusals the
// same).
func normaliseRefusal(s string) string {
	s = strings.Join(strings.Fields(strings.ToLower(s)), " ")
	return strings.TrimRight(s, ".,;:!? ")
}

// releasedPlan is a goal whose plan claim this node released for a
// repeated refusal: the reason and the goal row's updatedAt then (zero
// on a platform that serves none), and its status.
type releasedPlan struct {
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updatedAt"`
	Status    string    `json:"status"`
}

// planRefusals is what refusals.json under the work root keeps, so a
// fresh Planner — one per plan in `mesh node start` — and a restarted
// node read the same record.
type planRefusals struct {
	Last     map[string]string       `json:"last"`
	Released map[string]releasedPlan `json:"released"`
}

// PlanRefusalsPath is where the planner keeps its refusals:
// <root>/.plans/refusals.json.
func PlanRefusalsPath(root string) string { return filepath.Join(root, ".plans", "refusals.json") }

// loadRefusals reads the record from the work root into p's maps (p.mu
// held); with no work root, or nothing on disk yet, the maps stand.
func (p *Planner) loadRefusals() {
	if p.lastRefusal == nil {
		p.lastRefusal = map[string]string{}
	}
	if p.released == nil {
		p.released = map[string]releasedPlan{}
	}
	if p.WorkRoot == "" {
		return
	}
	data, err := os.ReadFile(PlanRefusalsPath(p.WorkRoot))
	if err != nil {
		return
	}
	var r planRefusals
	if json.Unmarshal(data, &r) != nil {
		return
	}
	p.lastRefusal, p.released = map[string]string{}, map[string]releasedPlan{}
	for k, v := range r.Last {
		p.lastRefusal[k] = v
	}
	for k, v := range r.Released {
		p.released[k] = v
	}
}

// saveRefusals writes p's maps under the work root, atomically (p.mu
// held); a failure is logged, never the plan's.
func (p *Planner) saveRefusals() {
	if p.WorkRoot == "" {
		return
	}
	path := PlanRefusalsPath(p.WorkRoot)
	data, err := json.Marshal(planRefusals{Last: p.lastRefusal, Released: p.released})
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = os.WriteFile(path+".tmp", data, 0o600)
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err != nil {
		p.logf("plan refusals: %v", err)
	}
}

// stillReleased says rec still holds for goal: the row's updatedAt
// unchanged, or — on a platform that serves none — the goal still in
// planning.
func stillReleased(rec releasedPlan, goal gateway.Goal) bool {
	if rec.UpdatedAt.IsZero() || goal.UpdatedAt.IsZero() {
		return goal.Status == "planning"
	}
	return goal.UpdatedAt.Equal(rec.UpdatedAt)
}

// Released says the node released goal's plan claim for a repeated
// refusal and the goal's row has not changed since: the reason is the
// refusal's text. The loop asks it before it claims a plan (ADR-023, a
// refused plan is released, not re-planned).
func (p *Planner) Released(goal gateway.Goal) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadRefusals()
	rec, ok := p.released[goal.ID]
	if !ok || !stillReleased(rec, goal) {
		return "", false
	}
	return rec.Reason, true
}

// forgetChanged drops the release of a goal whose row changed since,
// and the refusal before it: the person changed the goal, so it is
// planned afresh and refused twice again before it is released again.
func (p *Planner) forgetChanged(goal gateway.Goal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadRefusals()
	if rec, ok := p.released[goal.ID]; ok && !stillReleased(rec, goal) {
		delete(p.released, goal.ID)
		delete(p.lastRefusal, goal.ID)
		p.saveRefusals()
	}
}

// refused handles a plan the check refused on its final answer: the
// first refusal (or one that differs from the last) is kept and the
// error returned as before; a second identical one releases the plan
// claim with the refusal's text as the reason, records the goal
// released, and marks the error Released — no further try is counted.
func (p *Planner) refused(ctx context.Context, goal gateway.Goal, scope Scope, pr *PlanRefused) *PlanRefused {
	norm := normaliseRefusal(pr.Reason)
	p.mu.Lock()
	p.loadRefusals()
	last, seen := p.lastRefusal[goal.ID]
	if !seen || last != norm {
		p.lastRefusal[goal.ID] = norm
		p.saveRefusals()
		p.mu.Unlock()
		return pr
	}
	p.mu.Unlock()
	rerr := p.GW.ReleaseGoalPlan(ctx, goal.ID, scope.NodeID, pr.Reason)
	switch {
	case rerr == nil:
	case gateway.IsUnsupported(rerr):
		p.noGoalRelease.Do(func() {
			p.logf("goal %s: platform has no releaseGoalPlan; held on the node", goal.Slug)
		})
	default:
		p.logf("goal %s: releaseGoalPlan: %v; held on the node", goal.Slug, rerr)
	}
	p.logf("goal %s: plan refused twice with the same reason; released, not re-planned: %s", goal.Slug, pr.Reason)
	// the row as it stands after the release: a claim or a release that
	// touches the row is not the goal changing
	rec := releasedPlan{Reason: pr.Reason, UpdatedAt: goal.UpdatedAt, Status: goal.Status}
	if g, err := p.GW.Goal(ctx, goal.ID); err == nil && g != nil && !g.UpdatedAt.IsZero() {
		rec.UpdatedAt = g.UpdatedAt
	}
	p.mu.Lock()
	p.loadRefusals()
	p.released[goal.ID] = rec
	p.saveRefusals()
	p.mu.Unlock()
	return &PlanRefused{Goal: pr.Goal, Reason: pr.Reason, Released: true}
}
