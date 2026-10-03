package node

import (
	"fmt"
	"strings"
)

// FinishWho is who FinishDescription names on the pull request a finish
// opens: the actor the build session acted as (gateway.ActorFrom's
// `agent:builder@<slug>`), the model and adapter it ran on, and who it
// was paired with.
type FinishWho struct {
	Actor   string
	Model   string
	Adapter string
	Pairers []Pairer
}

// FinishDescription is the PR body FinishWorkItem and
// FinishWorkItemAlreadyDone carry (every-actor-has-a-name item 3): a
// `## Who` section naming who built the item and who it was paired
// with. The evaluator is not known at finish time — it is the
// evaluation's own actor, EvaluateWorkItem's `actor`, that names it
// (docs/adr/adr-015-a-node-registers-once-with-an-owner.md's identity
// amendment), so the line here only says where to look.
func FinishDescription(d FinishWho) string {
	var b strings.Builder
	b.WriteString("## Who\n\n")
	fmt.Fprintf(&b, "Built by %s on %s (%s)\n", d.Actor, modelName(d.Model), d.Adapter)
	if len(d.Pairers) == 0 {
		b.WriteString("Paired with nobody\n")
	} else {
		for _, p := range d.Pairers {
			fmt.Fprintf(&b, "Paired with %s <%s>\n", p.Name, p.Email)
		}
	}
	b.WriteString("\nEvaluated by: recorded by the evaluation's actor\n")
	return b.String()
}
