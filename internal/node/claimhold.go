package node

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// claimHoldWords are the words that class a ClaimWorkItem refusal as a
// hold, in the order they are tried: a refusal naming both a pause and
// a budget ("goal is paused: budget exhausted") is a budget hold.
var claimHoldWords = []struct {
	kind  string
	words []string
}{
	{"budget", []string{"budget", "over budget", "spent"}},
	{"pause", []string{"paused", "pause"}},
	{"approval", []string{"approval", "approve", "checkpoint", "awaiting a person"}},
}

// claimHoldKind classes a ClaimWorkItem refusal's text: "budget",
// "pause" or "approval" when it names one of their words, matched
// whole and lowercase, else "" — a refusal the next tick may well not
// repeat, such as `item is claimed, not ready` (ADR-024).
func claimHoldKind(msg string) string {
	words := strings.FieldsFunc(strings.ToLower(msg), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	text := " " + strings.Join(words, " ") + " "
	for _, k := range claimHoldWords {
		for _, w := range k.words {
			if strings.Contains(text, " "+w+" ") {
				return k.kind
			}
		}
	}
	return ""
}

// claimHold is one goal held by a classed claim refusal: the goal row
// as it was when refused, and whether the refused item was in the last
// ready listing.
type claimHold struct {
	Kind, Reason, ItemID string
	UpdatedAt            time.Time
	// Status and PausedReason stand in for UpdatedAt on an older
	// platform, which serves no updatedAt.
	Status, PausedReason string
	Since                time.Time
	Listed               bool
}

// ClaimHolds is the goals whose items this node does not ask to claim
// again: a ClaimWorkItem refused for budget, pause or approval holds
// its goal until the goal's row changes (its updatedAt, else its status
// and paused reason) or the refused item leaves the ready listing for a
// tick and is listed again (ADR-024). The zero value holds nothing.
type ClaimHolds struct {
	// Now stamps a hold's Since; nil is time.Now.
	Now   func() time.Time
	mu    sync.Mutex
	holds map[string]*claimHold
}

// Note holds goalID after itemID's claim was refused with reason,
// classed kind; goal is the row as read this tick.
func (c *ClaimHolds) Note(goalID, itemID, kind, reason string, goal gateway.Goal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.holds == nil {
		c.holds = map[string]*claimHold{}
	}
	since := time.Now()
	if c.Now != nil {
		since = c.Now()
	}
	c.holds[goalID] = &claimHold{Kind: kind, Reason: reason, ItemID: itemID, UpdatedAt: goal.UpdatedAt,
		Status: goal.Status, PausedReason: goal.PausedReason, Since: since, Listed: true}
}

// Holds answers whether goalID is still held, with the hold's reason:
// while goal (this tick's row; an empty one, unread, changes nothing)
// is the row it was noted on, and the refused item has not been absent
// from a ready listing and listed again — listedNow says whether it is
// listed this tick. A hold that no longer holds answers false and is
// left for the caller to Clear; Has tells it there was one.
func (c *ClaimHolds) Holds(goalID string, goal gateway.Goal, listedNow bool) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.holds[goalID]
	if h == nil {
		return "", false
	}
	if listedNow && !h.Listed {
		return "", false // listed claimable again
	}
	h.Listed = listedNow
	if goal.ID != "" {
		if !h.UpdatedAt.IsZero() || !goal.UpdatedAt.IsZero() {
			if !goal.UpdatedAt.Equal(h.UpdatedAt) {
				return "", false
			}
		} else if goal.Status != h.Status || goal.PausedReason != h.PausedReason {
			return "", false
		}
	}
	return h.Kind + ": " + h.Reason, true
}

// Has reports whether goalID is held.
func (c *ClaimHolds) Has(goalID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.holds[goalID] != nil
}

// Items is each held goal's refused item, by goal id.
func (c *ClaimHolds) Items() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.holds))
	for g, h := range c.holds {
		out[g] = h.ItemID
	}
	return out
}

// Unlisted marks each held goal whose refused item is not in listed, the
// tick's ready listing: listed again on a later tick, the hold lifts.
func (c *ClaimHolds) Unlisted(listed map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range c.holds {
		if !listed[h.ItemID] {
			h.Listed = false
		}
	}
}

// Clear drops goalID's hold.
func (c *ClaimHolds) Clear(goalID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.holds, goalID)
}

// goalOf is the goal goalID as read this tick (the tick's one GoalIndex
// read per org); an empty Goal when it cannot be read.
func (l *Loop) goalOf(ctx context.Context, sc Scope, goalID string) gateway.Goal {
	goals, _ := l.goals(ctx, sc)
	for _, g := range goals {
		if g.ID == goalID {
			return g
		}
	}
	return gateway.Goal{}
}

// holdClaim holds item's goal after its claim was refused with msg,
// classed kind: recorded as a held goal and said once.
func (l *Loop) holdClaim(ctx context.Context, sc Scope, item gateway.WorkItem, kind, msg string) {
	if l.ClaimHolds.Now == nil {
		l.ClaimHolds.Now = l.now
	}
	goal := l.goalOf(ctx, sc, item.GoalID)
	slug := goal.Slug
	if slug == "" {
		slug = item.GoalID
	}
	l.ClaimHolds.Note(item.GoalID, item.ID, kind, msg, goal)
	l.NoteHeld(HeldGoal{GoalID: item.GoalID, Slug: slug, Org: sc.Name(), Kind: "item", Reason: kind + ": " + msg, Since: l.now()})
	l.logf("claim: %s goal %s held (%s): %s; not asked again until the goal changes", sc.Name(), slug, kind, msg)
}

// claimHeld answers whether item's goal is held by a classed claim
// refusal, passing it over silently when it is; a hold that has lifted —
// the goal's row changed, or the refused item listed claimable again —
// is cleared and said.
func (l *Loop) claimHeld(ctx context.Context, sc Scope, item gateway.WorkItem, rank int) bool {
	if !l.ClaimHolds.Has(item.GoalID) {
		return false
	}
	goal := l.goalOf(ctx, sc, item.GoalID)
	why, ok := l.ClaimHolds.Holds(item.GoalID, goal, l.tickListed[l.ClaimHolds.Items()[item.GoalID]])
	if ok {
		l.passOver(item.GoalID, "item", priorityName(rank), why, time.Time{})
		return true
	}
	l.ClaimHolds.Clear(item.GoalID)
	l.ClearHeld(item.GoalID, "item")
	slug := goal.Slug
	if slug == "" {
		slug = item.GoalID
	}
	l.logf("claim: %s goal %s hold lifted", sc.Name(), slug)
	return false
}

// noteUnlisted keeps the tick's ready listing (tickListed) and marks the
// held goals whose refused item it lacks. A tick whose listing of any
// org failed marks none: an unread item is not an absent one.
func (l *Loop) noteUnlisted(listed [][]gateway.WorkItem, complete bool) {
	l.tickListed = map[string]bool{}
	for _, items := range listed {
		for _, it := range items {
			l.tickListed[it.ID] = true
		}
	}
	if complete {
		l.ClaimHolds.Unlisted(l.tickListed)
	}
}
