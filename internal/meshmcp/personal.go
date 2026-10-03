package meshmcp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The personal server: `mesh mcp`, Mesh inside Claude Code on the
// person's own credential (ADR-023, mesh mcp). It proxies the hosted
// reads; with a Join it claims and works items (join.go, work.go), and
// without one the work tools answer `not yet available`.

// Instructions is what Claude Code is told of the personal server.
const Instructions = `Mesh's goals and work items, on your own login. ` +
	`mesh_goals lists the live goals and their open items; mesh_join takes one to work on, mesh_status says what you hold. ` +
	`Report with mesh_progress, ask with mesh_ask, end with mesh_finish or mesh_release.`

// itemArg describes a work tool's `item` argument.
const itemArg = "the held item: its id, seq or goal/seq (the one held item when omitted)"

// NotYetAvailable is the answer of a tool this release names but does
// not yet serve.
const NotYetAvailable = "not yet available"

// Reads is what the personal server reads from the gateway:
// *gateway.Client has it.
type Reads interface {
	MyOrgs(ctx context.Context) ([]gateway.Org, error)
	Goals(ctx context.Context, orgID string) ([]gateway.Goal, error)
	GoalWorkItems(ctx context.Context, goalID string) ([]gateway.WorkItem, error)
}

// Personal is the server's state: the gateway and, once set, Join,
// which mesh_join claims and prepares an item with and whose state file
// the work tools read (nil answers `not yet available`).
type Personal struct {
	GW   Reads
	Join *Join

	mu sync.Mutex
}

// NewServer is `mesh mcp`'s server over gw: no channel capability.
func NewServer(gw Reads) *Server {
	return (&Personal{GW: gw}).Server()
}

// Server is the personal server over p: no channel capability.
func (p *Personal) Server() *Server {
	return &Server{Instructions: Instructions, Tools: p.Tools()}
}

// Tools lists the personal server's tools.
func (p *Personal) Tools() []Tool {
	return []Tool{
		{Name: "mesh_goals", Description: "List the live goals of the orgs you reach and their open work items. Use it to find an item to join.",
			Schema: ObjectSchema(map[string]string{"org": "only this org's login", "goal": "only this goal's slug"}),
			Call:   p.goals},
		{Name: "mesh_status", Description: "The items this Claude Code session holds through Mesh: each one's goal, seq, path, branch, whether the checkout has uncommitted changes and its commits past base.",
			Schema: ObjectSchema(nil),
			Call:   p.statusTool},
		{Name: "mesh_join", Description: "Claim a goal's item and prepare its checkout on the item's branch; answers the path, brief, acceptance and context. pair joins the branch another holder works on.",
			Schema: ObjectSchema(map[string]string{"goal": "the goal's slug", "item": "the item's seq (the next ready one when omitted)", "pair": "work the same branch as its holder"}, "goal"),
			Call:   p.join},
		{Name: "mesh_switch", Description: "Leave the item you hold — push its commits, note why and release it — and join another; answers the join.",
			Schema: ObjectSchema(map[string]string{"goal": "the goal's slug to join", "item": "the item's seq to join (the next ready one when omitted)", "note": "why you are switching", "pair": "work the same branch as its holder", "from": "the held item to leave (" + itemArg + "); required when several are held"}, "goal"),
			Call:   p.work(p.switchItem)},
		{Name: "mesh_progress", Description: "Report progress on the item you hold: one short note.",
			Schema: ObjectSchema(map[string]string{"item": itemArg, "note": "what happened, one line", "costCents": "what the work cost, in cents"}, "note"),
			Call:   p.work(p.progress)},
		{Name: "mesh_ask", Description: "Ask the goal's people a question about the item you hold; the answer comes back on the item.",
			Schema: ObjectSchema(map[string]string{"item": itemArg, "question": "the question, one or two sentences"}, "question"),
			Call:   p.work(p.ask)},
		{Name: "mesh_finish", Description: "Commit your work first: this pushes the item's branch as committed, records it and ends the item. If main already holds the work, finish with outcome already_done and the evidence.",
			Schema: ObjectSchema(map[string]string{"item": itemArg, "outcome": "built (the default) or already_done", "evidence": "for already_done: the PRs or commits on main that satisfy the acceptance"}),
			Call:   p.work(p.finish)},
		{Name: "mesh_release", Description: "Give the item you hold back to the platform unfinished, with the reason.",
			Schema: ObjectSchema(map[string]string{"item": itemArg, "reason": "why it is given back"}, "reason"),
			Call:   p.work(p.release)},
	}
}

// liveGoal is a goal still to be worked: not ended.
func liveGoal(status string) bool {
	switch status {
	case "done", "cancelled", "failed":
		return false
	}
	return true
}

// openItem is an item not yet ended.
func openItem(status string) bool {
	switch status {
	case "merged", "cancelled", "superseded", "already_done":
		return false
	}
	return true
}

func (p *Personal) goals(ctx context.Context, args map[string]any) (string, error) {
	orgs, err := p.GW.MyOrgs(ctx)
	if err != nil {
		return "", err
	}
	only, slug := str(args, "org"), str(args, "goal")
	var b strings.Builder
	for _, o := range orgs {
		if only != "" && o.Name() != only {
			continue
		}
		goals, err := p.GW.Goals(ctx, o.ID)
		if err != nil {
			return "", fmt.Errorf("goals of %s: %w", o.Name(), err)
		}
		for _, g := range goals {
			if !liveGoal(g.Status) || (slug != "" && g.Slug != slug) {
				continue
			}
			fmt.Fprintf(&b, "%s/%s (%s): %s\n", o.Name(), g.Slug, g.Status, g.Title)
			items, err := p.GW.GoalWorkItems(ctx, g.ID)
			if err != nil {
				return "", fmt.Errorf("items of %s: %w", g.Slug, err)
			}
			for _, it := range items {
				if !openItem(it.Status) {
					continue
				}
				fmt.Fprintf(&b, "  %d %s (%s)\n", it.Seq, it.Title, it.Status)
			}
		}
	}
	if b.Len() == 0 {
		return "no live goals", nil
	}
	return b.String(), nil
}

func (p *Personal) join(ctx context.Context, args map[string]any) (string, error) {
	if p.Join == nil {
		return NotYetAvailable, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Join.call(ctx, args)
}

func str(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}
