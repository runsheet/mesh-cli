package node

import (
	"context"
	"sync"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/jobresult"
)

// briefReader is a gateway that keeps the briefs of the jobs it claimed
// (gateway.Client.JobBriefOf; ADR-041).
type briefReader interface {
	JobBriefOf(kind string, subject gateway.JobSubject) (*gateway.JobBrief, bool)
}

// kindReader is a gateway that reads the catalogue (gateway.Client.JobKindOf).
type kindReader interface {
	JobKindOf(ctx context.Context, kind string) (gateway.JobKind, bool)
}

// subjectTypes is each runsheet kind's subject type (gateway.jobRoutes).
var subjectTypes = map[string]string{
	"runsheet/build":    "item",
	"runsheet/evaluate": "item",
	"runsheet/rebase":   "item",
	"runsheet/plan":     "goal",
	"runsheet/research": "direction",
}

// jobBriefOf is the brief claimJob answered for the job of kind on the
// subject with id, as gw kept it; for a job this process did not claim
// through claimJob (an adopted session, a test's), the kind's catalogue
// row stands in for its guidance, finish and finishSchema; else an empty
// brief, and the kind's built-in prompt runs (ADR-041).
func jobBriefOf(ctx context.Context, gw any, kind, id string) gateway.JobBrief {
	if r, ok := gw.(briefReader); ok {
		if b, ok := r.JobBriefOf(kind, gateway.JobSubject{Type: subjectTypes[kind], ID: id}); ok && b != nil {
			return *b
		}
	}
	if r, ok := gw.(kindReader); ok {
		if k, ok := r.JobKindOf(ctx, kind); ok {
			return gateway.JobBrief{Kind: kind, Subject: gateway.JobSubject{Type: subjectTypes[kind], ID: id}, Finish: k.Finish, Guidance: k.Guidance, FinishSchema: k.FinishSchema}
		}
	}
	return gateway.JobBrief{}
}

// noGuidanceLine is logged once per kind whose brief carries no
// guidance: a platform before runsheet ADR 067 published it, whose jobs
// run on the node's built-in prompt.
const noGuidanceLine = "brief carries no guidance; running the built-in prompt"

// NoGuidance logs noGuidanceLine once per kind. The zero value is ready,
// and a copy shares the noting (the kinds map; noGuidanceMu guards every
// NoGuidance, so a Runner or an Evaluator copied by value stays sound).
type NoGuidance struct {
	kinds map[string]bool
}

var noGuidanceMu sync.Mutex

// Note logs the line for kind the first time.
func (n *NoGuidance) Note(kind string, logf func(string, ...any)) {
	noGuidanceMu.Lock()
	defer noGuidanceMu.Unlock()
	if n.kinds == nil {
		n.kinds = map[string]bool{}
	}
	if n.kinds[kind] {
		return
	}
	n.kinds[kind] = true
	if logf != nil {
		logf("%s: %s", kind, noGuidanceLine)
	}
}

// recoverResult reads a mesh_result call the session's stream recorded
// (harness.Result.WorkCalls) into holder when the node received none
// (jobresult.Holder.Recover): the relay or the proxy failed between the
// session and the node, and the call is the same call read late. Logged
// either way it goes; nothing of the final message is read.
func recoverResult(holder *jobresult.Holder, result harness.Result, label string, logf func(string, ...any)) {
	if holder == nil {
		return
	}
	args, ok := result.WorkCalls[jobresult.ResultTool]
	if !ok {
		return
	}
	recorded, err := holder.Recover(args)
	switch {
	case err != nil:
		logf("%s: the stream's mesh_result call did not fit and was not recorded: %v", label, err)
	case recorded:
		logf("%s: mesh_result read from the session's stream: the call never reached the node", label)
	}
}
