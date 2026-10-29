// Package escalate is a lease's model policy (Mesh v1 spec, "Harness on
// every job": the model escalates within the same session on red,
// conflict, stall or when asked; PR 2.11). The kind names its default
// model, the ladder it escalates along, the triggers it escalates on and
// its ceiling; a Ladder walks that policy for one lease and keeps every
// step it took, which the node reports with the lease's usage.
package escalate

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The triggers a kind's model.on may name.
const (
	// OnRed is a suite gone red (run_suite).
	OnRed = "red"
	// OnConflict is work that would not apply (apply_onto).
	OnConflict = "conflict"
	// OnStall is a session quiet past the node's stall time.
	OnStall = "stall"
	// OnAsked is the session asking, through the escalate node tool.
	OnAsked = "asked"
	// OnRule is a kind's stop rule whose action is escalate
	// (projectmesh-io/api docs/adr/adr-001-amendment-learnings-and-
	// transcripts.md): the kind asked for it by declaring the rule, so it
	// steps the ladder whatever the policy's on names.
	OnRule = "rule"
)

// Ladder is one lease's walk up its kind's model policy. It is safe for
// concurrent use: a node tool and the session's own loop both step it.
type Ladder struct {
	policy meshapi.ModelPolicy

	mu    sync.Mutex
	model string
	steps []meshapi.Escalation
}

// New starts a lease's ladder on the policy's default model.
func New(p meshapi.ModelPolicy) *Ladder {
	return &Ladder{policy: p, model: p.Default}
}

// Model is the model the lease runs on now; "" is the CLI's own.
func (l *Ladder) Model() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.model
}

// next is the model after the current one, and why there is none: the
// policy does not escalate on the trigger, or the ladder is at its top
// or its ceiling.
func (l *Ladder) next(on string) (string, error) {
	p := l.policy
	if on != OnRule && !slices.Contains(p.On, on) {
		return "", fmt.Errorf("this kind does not escalate on %s", on)
	}
	top := len(p.Escalate)
	if i := slices.Index(p.Escalate, p.Max); p.Max != "" && i >= 0 {
		top = i + 1
	}
	if l.model != "" && l.model == p.Max {
		return "", fmt.Errorf("the session already runs on %s, the kind's ceiling", p.Max)
	}
	i := slices.Index(p.Escalate, l.model) + 1 // a model off the ladder (the default) starts it
	for ; i < top; i++ {
		if p.Escalate[i] != l.model {
			return p.Escalate[i], nil
		}
	}
	return "", fmt.Errorf("no model is left above %s on the kind's ladder", orCLI(l.model))
}

// Peek answers the model trigger on would escalate to, without taking
// the step.
func (l *Ladder) Peek(on string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next(on)
}

// Step escalates for on: the lease's model moves to the next on the
// ladder, and the step is kept. It answers the step, or why there is
// none.
func (l *Ladder) Step(on, reason string) (meshapi.Escalation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	to, err := l.next(on)
	if err != nil {
		return meshapi.Escalation{}, err
	}
	e := meshapi.Escalation{From: l.model, To: to, On: on, Reason: reason, At: time.Now().UTC()}
	l.model = to
	l.steps = append(l.steps, e)
	return e, nil
}

// Failed marks the latest step to to as not taken — the session refused
// the model — and moves the lease back to the model it came from.
func (l *Ladder) Failed(to, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.steps) - 1; i >= 0; i-- {
		if l.steps[i].To == to && l.steps[i].Failed == "" {
			l.steps[i].Failed = why
			l.model = l.steps[i].From
			return
		}
	}
}

// Steps are the escalations the lease took, in order.
func (l *Ladder) Steps() []meshapi.Escalation {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.steps)
}

func orCLI(model string) string {
	if model == "" {
		return "the CLI's default model"
	}
	return model
}
