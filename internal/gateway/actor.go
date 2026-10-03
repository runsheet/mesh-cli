package gateway

import (
	"context"
	"sync"
)

// Role is what a node's session does on the platform: the first half of
// the actor it sends on its commands (ADR-015's amendment 2026-10-03,
// every actor has a name).
type Role string

const (
	RoleBuilder   Role = "builder"
	RoleEvaluator Role = "evaluator"
	RolePlanner   Role = "planner"
)

// ActorField is the command input's key for the session's actor,
// `agent:<role>@<slug>`. It is sent only when the platform's input type
// takes it (takesActor).
const ActorField = "actor"

type actorKey struct{}

// WithActor marks ctx as a session acting as role on the node slug: the
// commands sent on it carry actor `agent:<role>@<slug>`. A ctx without
// it — the operator's own commands, `mesh mcp`'s pass-through work
// tools acting as the person — carries none.
func WithActor(ctx context.Context, role Role, slug string) context.Context {
	return context.WithValue(ctx, actorKey{}, "agent:"+string(role)+"@"+slug)
}

// ActorFrom answers the actor WithActor put on ctx, `agent:<role>@<slug>`.
func ActorFrom(ctx context.Context) (string, bool) {
	a, ok := ctx.Value(actorKey{}).(string)
	return a, ok && a != ""
}

// actorInputs are the commands that carry the actor and the input type
// each is introspected on: finishWorkItem's already-done path shares
// FinishWorkItemInput, the no-verdict path EvaluateWorkItemInput.
var actorInputs = map[string]string{
	"claimWorkItem":          "ClaimWorkItemInput",
	"reportWorkItemProgress": "ReportWorkItemProgressInput",
	"finishWorkItem":         "FinishWorkItemInput",
	"evaluateWorkItem":       "EvaluateWorkItemInput",
	"planGoal":               "PlanGoalInput",
}

// actorProbes caches, per input type, whether the platform's input takes
// `actor` (see takesActor).
type actorProbes struct {
	mu     sync.Mutex
	byType map[string]*holdingProbe
}

func (a *actorProbes) probe(typ string) *holdingProbe {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byType == nil {
		a.byType = map[string]*holdingProbe{}
	}
	p, ok := a.byType[typ]
	if !ok {
		p = &holdingProbe{}
		a.byType[typ] = p
	}
	return p
}

// withActor puts the ctx's actor on the input of field when field is one
// of actorInputs and the platform's input type takes `actor` —
// introspected once per type and kept for ScalingProbeEvery. A failed
// probe, or a type without the field, sends the input without it, never
// an error: the actor is never sent to be refused, since a refused
// attempt would be recorded carrying it.
func (c *Client) withActor(ctx context.Context, field string, in map[string]any) {
	actor, ok := ActorFrom(ctx)
	if !ok {
		return
	}
	typ, ok := actorInputs[field]
	if !ok {
		return
	}
	if c.takesInputField(ctx, c.actorSchema.probe(typ), typ, ActorField) {
		in[ActorField] = actor
	}
}
