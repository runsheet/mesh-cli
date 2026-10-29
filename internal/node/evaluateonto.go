package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gitapply"
)

// OntoConflictPrefix opens the reason an evaluation ends with when its
// change does not apply onto the brief's onto: runsheet defers the item
// to a rebase on it and spends none of its tries (gateway/finishjob_onto.go).
const OntoConflictPrefix = "onto_conflict:"

// evaluateOnto puts the branch's head onto the brief's onto in the
// evaluation's checkout before it is diffed (runsheet ADR 077, the queue
// validates the would-be merge): the onto ref fetched, the head applied
// onto it (gitapply, linear unless the brief says merge) and checked out
// detached, so the evaluator reads and runs main as it is now with the
// change on it. It answers the ref the diff is then taken from, "" to
// diff as before, and conflict — onto_conflict: <paths> — when the
// change does not apply. Anything else that stops it is logged and the
// evaluation runs on the branch as before.
func (e *Evaluator) evaluateOnto(ctx context.Context, dir, label string, code gateway.JobCode) (diffFrom, conflict string) {
	logf := func(format string, args ...any) { e.logf("item %s: onto: %s", label, fmt.Sprintf(format, args...)) }
	ref := strings.TrimPrefix(strings.TrimPrefix(code.Onto.Ref, "refs/heads/"), "origin/")
	if ref == "" {
		// the diff is taken from a branch of origin's: a brief naming a
		// commit alone has none to take it from
		logf("the brief's onto names no ref; evaluated on the branch as before")
		return "", ""
	}
	if _, err := e.WS.ChangedPaths(ctx, dir, ref); err != nil {
		logf("fetch %s: %v; evaluated on the branch as before", ref, err)
		return "", ""
	}
	git := func(args ...string) (string, error) {
		out, err := hardenedGitCmd(ctx, "", dir, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	want := code.Onto.SHA
	if want == "" {
		want = "origin/" + ref
	}
	tip, err := git("rev-parse", "--verify", "--quiet", want+"^{commit}")
	if err != nil || tip == "" {
		logf("%s is not in the checkout; evaluated on the branch as before", want)
		return "", ""
	}
	head, err := gitHead(ctx, dir)
	if err != nil {
		logf("HEAD: %v; evaluated on the branch as before", err)
		return "", ""
	}
	where := ref + " " + short(tip)
	if _, err := git("merge-base", "--is-ancestor", tip, head); err == nil {
		e.logf("item %s: onto: %s already holds %s; evaluated as it is", label, short(head), where)
		return ref, ""
	}
	name, email := AgentIdentity(string(gateway.RoleEvaluator), agentSlug(e.Cfg))
	id, err := applyIdentity(code.ApplyAs, Identity{Name: name, Email: email}, time.Now())
	if err != nil {
		logf("%v; evaluated on the branch as before", err)
		return "", ""
	}
	mode, err := applyMode(code, gitapply.Linear)
	if err != nil {
		logf("%v; evaluated on the branch as before", err)
		return "", ""
	}
	steps, c, err := ontoGit().Apply(ctx, dir, tip, []gitapply.Change{{Head: head}}, mode, id)
	switch {
	case errors.Is(err, gitapply.ErrGitTooOld):
		logf("%v; evaluated on the branch as before", err)
		return "", ""
	case err != nil:
		logf("apply onto %s: %v; evaluated on the branch as before", where, err)
		return "", ""
	case c != nil:
		return "", OntoConflictPrefix + " " + strings.Join(c.Paths, ", ")
	case len(steps) != 1:
		logf("apply onto %s wrote %d commits, not one; evaluated on the branch as before", where, len(steps))
		return "", ""
	}
	if out, err := hardenedGitCmd(ctx, "", dir, "checkout", "--quiet", "--detach", steps[0].Commit).CombinedOutput(); err != nil {
		logf("check out %s: %v: %s; evaluated on the branch as before", short(steps[0].Commit), err, strings.TrimSpace(string(out)))
		return "", ""
	}
	e.logf("item %s: onto: applied %s onto %s as %s (%s); evaluated there", label, short(head), where, short(steps[0].Commit), mode)
	return ref, ""
}

// ontoConflict ends the evaluation of a change that does not apply onto
// the brief's onto: no session, no verdict, the job closed with the
// node's outcome naming the paths, which runsheet answers with a rebase
// and not a spent try.
func (e *Evaluator) ontoConflict(ctx context.Context, item gateway.WorkItem, label, reason string) error {
	reason = truncate(reason, 4000)
	e.logf("item %s: not evaluated, the change does not apply: %s", label, reason)
	stopHeartbeat(ctx)
	nodeID := nodeIDOf(e.Scopes, item.OrgID, e.Cfg)
	ctx = gateway.WithNodeID(ctx, nodeID)
	res := map[string]any{"outcome": "no_verdict", "reason": reason}
	if _, err := e.GW.FinishJob(ctx, nodeID, EvaluateKind, gateway.JobSubject{Type: "item", ID: item.ID}, map[string]any{"result": res}); err != nil {
		_ = handInRefused(e.logf, nil, label, "FinishJob", err)
		return fmt.Errorf("record onto conflict: %w", err)
	}
	return nil
}
