package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// A claim outlives the node (mesh-cli ADR 009, Claims): a plan or
// research claim is written to the work root's claims dir the moment the
// platform grants it, before its session has a credential, a context or
// a unit — so a node that exits in between (an upgrade's swap) leaves a
// record the next process re-opens, instead of a claim the platform
// holds until PlanClaimTTL with nobody working it.

// claimsDirName is the claims dir under the work root: a dot dir, never
// a checkout and never a session dir, so FindSessions passes it by.
const claimsDirName = ".claims"

// ClaimsDir is the claims dir of the work root.
func ClaimsDir(root string) string { return filepath.Join(root, claimsDirName) }

// ClaimRecord is a plan or research claim this node holds on the
// platform, as written at claim time.
type ClaimRecord struct {
	NodeID      string       `json:"nodeId"`
	Kind        harness.Kind `json:"kind"`
	GoalID      string       `json:"goalId,omitempty"`
	DirectionID string       `json:"directionId,omitempty"`
	// ScopeID is the org the claim was taken in.
	ScopeID string `json:"scopeId"`
	// Name is the goal's slug or the direction's project/slug, for the log.
	Name      string    `json:"name,omitempty"`
	ClaimedAt time.Time `json:"claimedAt"`
}

// Subject is the claim's goal (a plan) or direction (research).
func (r ClaimRecord) Subject() string {
	if r.Kind == harness.KindResearch {
		return r.DirectionID
	}
	return r.GoalID
}

// label is how the log names the subject: its name, else its id.
func (r ClaimRecord) label() string {
	if r.Name != "" {
		return r.Name
	}
	return r.Subject()
}

// claimFile is the record's file in the claims dir.
func claimFile(dir string, kind harness.Kind, subject string) string {
	return filepath.Join(dir, string(kind)+"-"+subject+".json")
}

// WriteClaim writes r to dir atomically: a temporary file renamed over
// the record.
func WriteClaim(dir string, r ClaimRecord) error {
	if r.Subject() == "" {
		return errors.New("claim record: no subject")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".claim-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), claimFile(dir, r.Kind, r.Subject()))
}

// RemoveClaim removes the record of a claim; none is no error.
func RemoveClaim(dir string, kind harness.Kind, subject string) error {
	err := os.Remove(claimFile(dir, kind, subject))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// FindClaims lists the claim records under root that name one of
// nodeIDs, oldest first. An unreadable record is skipped; no claims dir
// is none.
func FindClaims(root string, nodeIDs []string) ([]ClaimRecord, error) {
	dir := ClaimsDir(root)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ClaimRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r ClaimRecord
		if json.Unmarshal(b, &r) != nil || r.NodeID == "" || r.Subject() == "" || !slices.Contains(nodeIDs, r.NodeID) {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ClaimedAt.Before(out[j].ClaimedAt) })
	return out, nil
}

// recordClaim writes the record of a claim just granted (nothing without
// a ClaimsDir); a failure is logged, never the claim's.
func (l *Loop) recordClaim(r ClaimRecord) {
	if l.ClaimsDir == "" {
		return
	}
	if r.ClaimedAt.IsZero() {
		r.ClaimedAt = l.now().UTC()
	}
	if err := WriteClaim(l.ClaimsDir, r); err != nil {
		l.logf("claim record of %s %s: %v", r.Kind, r.label(), err)
	}
}

// dropClaim removes the record of a claim whose session's end is handled.
func (l *Loop) dropClaim(kind harness.Kind, subject string) {
	if l.ClaimsDir == "" {
		return
	}
	if err := RemoveClaim(l.ClaimsDir, kind, subject); err != nil {
		l.logf("claim record of %s %s: %v", kind, subject, err)
	}
}

// startPlan runs the planner on a goal already marked in l.planning, in
// its own goroutine. The claim's record goes once the planner returns —
// the plan handed in, released, or held — unless ctx is done: a planner
// the node let go of keeps its record for the next process.
func (l *Loop) startPlan(ctx context.Context, g gateway.Goal, sc Scope) {
	l.countFor(g.ID, sc.ID)
	l.wg.Add(1)
	go func(ctx context.Context, g gateway.Goal, sc Scope) {
		defer crash.Guard()
		defer l.wg.Done()
		defer func() {
			unlessDetached(ctx, func() { l.dropClaim(harness.KindPlan, g.ID) })
			l.mu.Lock()
			delete(l.planning, g.ID)
			delete(l.sessionOrg, g.ID)
			l.mu.Unlock()
			l.notify()
		}()
		l.Plan(ctx, g, sc)
	}(ctx, g, sc)
}

// startResearch runs the director on a direction already marked in
// l.researching, as startPlan runs the planner.
func (l *Loop) startResearch(ctx context.Context, d gateway.Direction, p gateway.Project, sc Scope) {
	l.countFor(d.ID, sc.ID)
	l.wg.Add(1)
	go func(ctx context.Context, d gateway.Direction, p gateway.Project, sc Scope) {
		defer crash.Guard()
		defer l.wg.Done()
		defer func() {
			unlessDetached(ctx, func() { l.dropClaim(harness.KindResearch, d.ID) })
			l.mu.Lock()
			delete(l.researching, d.ID)
			delete(l.sessionOrg, d.ID)
			l.mu.Unlock()
			l.notify()
		}()
		l.Research(ctx, d, p, sc)
	}(ctx, d, p, sc)
}

// markClaim marks subject in the map of its kind and reports whether it was free.
func (l *Loop) markClaim(kind harness.Kind, subject string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := &l.planning
	if kind == harness.KindResearch {
		m = &l.researching
	}
	if *m == nil {
		*m = map[string]bool{}
	}
	if (*m)[subject] {
		return false
	}
	(*m)[subject] = true
	return true
}

func (l *Loop) unmarkClaim(kind harness.Kind, subject string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if kind == harness.KindResearch {
		delete(l.researching, subject)
	} else {
		delete(l.planning, subject)
	}
}

// ReopenClaims re-opens, on start and after AdoptSessions, every plan or
// research claim a process before this one recorded but never opened a
// session for (mesh-cli ADR 009, Claims): one whose subject has a found
// session is AdoptSessions' and is left alone. Each is claimed again by
// the same node id — the platform lets its holder re-claim within
// PlanClaimTTL — logged once, and run as a fresh session through the
// same marking the tick's pass uses. A refused re-claim, a goal no longer
// in planning, a direction no longer live, or a node id no org here is
// served under removes the record and says why; a read that fails leaves
// it for the next start. It returns how many were re-opened.
func (l *Loop) ReopenClaims(ctx context.Context, claims []ClaimRecord, found []FoundSession) int {
	if l.Draining() {
		return 0
	}
	open := map[string]bool{}
	for _, s := range found {
		open[s.State.Kind+"/"+s.Subject()] = true
	}
	n := 0
	for _, r := range claims {
		if open[string(r.Kind)+"/"+r.Subject()] {
			continue
		}
		if l.reopen(ctx, r) {
			n++
		}
	}
	return n
}

func (l *Loop) reopen(ctx context.Context, r ClaimRecord) bool {
	since := r.ClaimedAt.UTC().Format(time.RFC3339)
	drop := func(format string, args ...any) {
		l.dropClaim(r.Kind, r.Subject())
		l.logf("%s; claim record removed", fmt.Sprintf(format, args...))
	}
	var sc Scope
	ok := false
	for _, s := range l.CurrentScopes() {
		if s.NodeID == r.NodeID {
			sc, ok = s, true
			break
		}
	}
	// Only plans and research are recorded: an evaluation or a build has
	// the platform's record instead — an item in evaluating or running
	// held by this node is found by the tick (evaluateHeld, the held
	// builds) and started again, or adopted when its session opened — so
	// no claim file is written for them and none is re-opened here.
	switch r.Kind {
	case harness.KindPlan:
		if l.Plan == nil {
			return false
		}
		if !ok {
			drop("the plan claim of goal %s: node %s serves no org here", r.label(), r.NodeID)
			return false
		}
		g, err := l.GW.Goal(ctx, r.GoalID)
		if err != nil {
			l.logf("the plan claim of goal %s: %v; left for the next start", r.label(), err)
			return false
		}
		if g == nil || g.Status != "planning" {
			status := "gone"
			if g != nil {
				status = g.Status
			}
			drop("the plan claim of goal %s: the goal is %s, not planning", r.label(), status)
			return false
		}
		if !l.markClaim(harness.KindPlan, g.ID) {
			return false
		}
		if err := l.GW.ClaimPlan(ctx, g.ID, r.NodeID); err != nil {
			l.unmarkClaim(harness.KindPlan, g.ID)
			drop("re-opening the plan claim of goal %s refused: %v", g.Slug, err)
			return false
		}
		l.logf("re-opening the plan claim of goal %s held since %s", g.Slug, since)
		priority := priorityName(gateway.PriorityRank(g.Priority))
		actx := withPriority(ctx, priority)
		if a, ok := l.accountToFinish(actx, priority, "plan"); ok {
			actx = a
		}
		l.startPlan(actx, *g, sc)
		return true
	case harness.KindResearch:
		if l.Research == nil {
			return false
		}
		if !ok {
			drop("the research claim of %s: node %s serves no org here", r.label(), r.NodeID)
			return false
		}
		directions, err := l.GW.Directions(ctx, sc.ID, "")
		if err != nil {
			l.logf("the research claim of %s: %v; left for the next start", r.label(), err)
			return false
		}
		var d *gateway.Direction
		for i := range directions {
			if directions[i].ID == r.DirectionID {
				d = &directions[i]
				break
			}
		}
		if d == nil || !d.Active() {
			drop("the research claim of %s: the direction is no longer live", r.label())
			return false
		}
		p, err := l.GW.Project(ctx, d.ProjectID)
		if err != nil || p == nil {
			l.logf("the research claim of %s: project %s: %v; left for the next start", r.label(), d.ProjectID, err)
			return false
		}
		name := p.Slug + "/" + d.Slug
		if !l.markClaim(harness.KindResearch, d.ID) {
			return false
		}
		if err := l.GW.ClaimDirectionResearch(ctx, d.ID, r.NodeID); err != nil {
			l.unmarkClaim(harness.KindResearch, d.ID)
			drop("re-opening the research claim of %s refused: %v", name, err)
			return false
		}
		l.logf("re-opening the research claim of %s held since %s", name, since)
		actx := ctx
		if a, ok := l.accountToFinish(ctx, "", ""); ok {
			actx = a
		}
		l.startResearch(actx, *d, *p, sc)
		return true
	}
	drop("claim record of kind %q: nothing re-opens it", r.Kind)
	return false
}
