package nodev1

import (
	"context"
	"slices"
	"time"

	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/release"
)

// nodeTools are the node tools the brief's kind names (Mesh v1 spec,
// "Evidence signing"): run_suite, apply_onto and push, each recording
// its signed ToolRecord through the lease's recorder, which keeps the
// evidence ids for the finish. Each is signed keylessly as the lease's
// session — tokens, held here alone, give Fulcio the session's ID token
// for audience `sigstore` (SigningToken), never the mesh-session token,
// which Fulcio refuses — or, when the node has no Fulcio and Rekor or
// cannot reach them, by the node's own key. escalate, and a red run_suite or a
// conflicted apply_onto, move the session up the lease's ladder through
// its mailbox (PR 2.11). A tool the node does not have never reaches
// here: work hands the lease back tool_unavailable first (causes.go).
// suites, set, runs run_suite's steps in the lease's pod; nil runs them
// on the host only on a node explicitly unsandboxed (hostSuites), and
// otherwise work hands the lease back tool_unavailable (podsuites.go).
//
// pusher, set, is how push pushes: through the lease's relay sidecar
// (relaypod.go), never the node's own grant.
//
// clock is the lease's: its ctx, in which a run_suite run goes on past
// the call that started it, and when it began, from which a validation
// is held to its repository's budget (ADR-049).
//
// tb, set, is the lease's own time budget (timebudget.go), already
// running by the time its tools are built: request_time extends it,
// inside the kind's own cap of b.Harness.Budget.Minutes per ask. A
// kind naming request_time with no budget of its own serves none.
//
// tainted, set, is the lease's taint lockdown (taint.go): once it
// answers true, push is refused for the rest of the lease (ADR-046's
// 2026-11 amendment).
func (r *Runtime) nodeTools(b *meshapi.Brief, grant *gitrelay.Grant, workspace string, tokens *sessionTokens, ladder *escalate.Ladder, mb *Mailbox, suites nodetools.SuiteRunner, pusher nodetools.Pusher, clock leaseClock, tb *timeBudget, tainted func() bool) ([]mcptool.Tool, *nodetools.Recorder) {
	if len(b.NodeTools) == 0 {
		return nil, nil
	}
	rec := r.recorderFor(b, tokens)
	var g nodetools.Grant
	repo := ""
	if grant != nil {
		g = grant
	}
	if b.Git != nil {
		repo = b.Git.Repo
	}
	// rs, built ahead of the loop whenever run_suite is named, is the one
	// RunSuite instance both run_suite and stop_suite share — stop_suite
	// reaches a run's still-going lanes only through run_suite's own
	// registry, whichever order the kind names them in.
	var rs *nodetools.RunSuite
	if slices.Contains(b.NodeTools, nodetools.RunSuiteTool) {
		rs = &nodetools.RunSuite{Workspace: workspace, Recorder: rec, Run: suites,
			OnHost: suites == nil && r.hostSuites(), Escalate: escalateOn(ladder, mb, escalate.OnRed),
			Wait: r.SuiteWait, Context: clock.ctx, Began: clock.began, Budget: suiteRunBudget(clock.budget), OnFinished: clock.answers.addFunc(), OnCoverage: clock.coverage.set,
			OnActivity: func(what string) { r.stepProgress(b.LeaseID, nodetools.RunSuiteTool+": "+what) },
			Logf: func(format string, args ...any) {
				r.logf("nodev1: lease %s: "+format, append([]any{b.LeaseID}, args...)...)
			}}
		if b.Results != nil {
			rs.Results = *b.Results
			if tokens != nil {
				rs.Session = tokens
			}
		}
	}
	var out []mcptool.Tool
	for _, name := range b.NodeTools {
		switch name {
		case nodetools.PushTool:
			pushTo := ""
			if b.Code != nil {
				pushTo = b.Code.PushTo
			}
			p := &nodetools.Push{Grant: g, Pusher: pusher, Repo: repo, Workspace: workspace, Recorder: rec, PushTo: pushTo,
				Refusal: func(since time.Time) string { return r.refusalSince(b.LeaseID, since) }, Tainted: tainted}
			out = append(out, p.Tool())
		case nodetools.RunSuiteTool:
			out = append(out, rs.Tool())
		case nodetools.StopSuiteTool:
			if rs == nil {
				r.logf("nodev1: lease %s: the kind names node tool %q without %q to stop a lane of", b.LeaseID, name, nodetools.RunSuiteTool)
				continue
			}
			out = append(out, (&nodetools.StopSuite{Runs: rs}).Tool())
		case nodetools.ApplyOntoTool:
			var vendor string
			if b.Git != nil {
				vendor = b.Git.Vendor
			}
			out = append(out, (&nodetools.ApplyOnto{Grant: g, Repo: repo, Workspace: workspace, Code: b.Code, Vendor: vendor, Recorder: rec,
				Escalate: escalateOn(ladder, mb, escalate.OnConflict)}).Tool())
		case nodetools.SignReleaseTool:
			sr := &nodetools.SignRelease{Workspace: workspace}
			if tokens != nil && r.Sigstore.Enabled() {
				fulcio, rekor := r.Sigstore.FulcioURL, r.Sigstore.RekorURL
				sr.Sign = func(ctx context.Context, sums []byte) ([]byte, error) {
					tok, err := tokens.SigningToken(ctx)
					if err != nil {
						return nil, err
					}
					return release.Sign(ctx, sums, tok, release.SignOptions{FulcioURL: fulcio, RekorURL: rekor})
				}
			}
			out = append(out, sr.Tool())
		case EscalateTool:
			// the lease's own session's, by the harness its brief names
			// (harnessroute.go)
			sess, _ := r.sessionFor(b)
			_, codex := sess.(*CodexSession)
			out = append(out, escalateTool(ladder, mb, codex))
		case RequestTimeTool:
			if tb == nil {
				r.logf("nodev1: lease %s: the kind names node tool %q, but has no time budget to extend", b.LeaseID, name)
				continue
			}
			tool := requestTimeTool(tb, time.Duration(b.Harness.Budget.Minutes)*time.Minute)
			out = append(out, r.recordRequestTime(b, ladderModel(ladder, b), tool))
		default:
			// result, hand_back and progress are endTools' (endtools.go)
			if isEndTool(name) {
				continue
			}
			r.logf("nodev1: lease %s: the kind names node tool %q, which this node does not have", b.LeaseID, name)
		}
	}
	return r.reportsWhileRunning(clock.ctx, b.LeaseID, out), rec
}

// recorderFor is the lease's own Recorder: every node tool a session's
// (nodeTools) and a no-session validate lease's (serveValidate) build
// shares the one construction, signed keylessly as the lease's session
// when tokens is set, else by the node's own key.
func (r *Runtime) recorderFor(b *meshapi.Brief, tokens *sessionTokens) *nodetools.Recorder {
	signer := &nodetools.Signer{Sigstore: r.Sigstore, NodeKey: r.SigningKey}
	if tokens != nil {
		signer.Token = tokens.SigningToken
	}
	return &nodetools.Recorder{
		Lease: nodetools.Lease{JobID: b.JobID, LeaseID: b.LeaseID, LeaseN: b.LeaseN, Product: b.Product, Tenant: b.Tenant,
			NodeID: r.NodeID(), EvidenceRequired: b.EvidenceRequired},
		Signer: signer, Mesh: r.API,
	}
}

// escalateOn is a node tool's escalation on trigger on: when the kind
// escalates on it and the ladder has a next model, it leaves the step in
// the lease's mailbox for the session's loop and answers that model;
// otherwise "". Nil when the lease has no ladder or mailbox.
func escalateOn(ladder *escalate.Ladder, mb *Mailbox, on string) func(string) string {
	if ladder == nil || mb == nil {
		return nil
	}
	return func(why string) string {
		to, err := ladder.Peek(on)
		if err != nil {
			return ""
		}
		if mb.Post(Message{Kind: MsgEscalate, On: on, Text: clip(why, 500)}) != nil {
			return ""
		}
		return to
	}
}

// leaseClock is a lease's ctx and when its work began.
type leaseClock struct {
	ctx   context.Context
	began time.Time
	// budget is the validation budget the lease is held to, zero for
	// none, and answers hears run_suite's answers for the report the
	// node makes if the budget stops the session (validatebudget.go)
	budget  time.Duration
	answers *suiteAnswers
	// coverage is the e2e coverage map a run_suite run wrote, carried
	// on the validate result's "coverage" key (withCoverage)
	coverage *coverageNote
}

// suiteRunBudget is how long from the lease's start run_suite's runs
// may go on: the budget less suiteGrace, so the session hears their
// answers before the node stops it.
func suiteRunBudget(budget time.Duration) time.Duration {
	if budget <= 0 {
		return 0
	}
	return budget - suiteGrace(budget)
}

// suiteBudget is the validation budget the lease is held to: the
// smaller of the checkout's and its default branch's validate.yml
// budget (validatebudget.go), on a validation kind (validationKind);
// zero otherwise, or when neither states one.
func (r *Runtime) suiteBudget(b *meshapi.Brief, workspace string) time.Duration {
	if !validationKind(b) {
		return 0
	}
	budget := validateBudget(workspace)
	if budget > 0 {
		r.logf("nodev1: lease %s: held to the repository's validation budget of %s", b.LeaseID, budget)
	}
	return budget
}
