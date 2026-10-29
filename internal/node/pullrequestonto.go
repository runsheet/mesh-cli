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

// A hand-opened pull request validated as merged onto main (runsheet
// ADR 077, the Zuul gate's wave 0): its brief names onto (the default
// branch's head when the platform asked), the head to apply, merge mode
// and pushTo, the one ref the node may create. The node checks the head
// out, merges it onto onto (gitapply, parents [onto, head]), pushes that
// commit to pushTo, and validates and attests it; the platform takes an
// attestation of a commit whose parents are those two.

// OntoConflictError is a pull request's head that does not merge onto
// the brief's onto: the validation fails with its reason, `onto_conflict:
// <paths>`, which the platform shows as a neutral check, never red.
type OntoConflictError struct {
	Paths []string
}

func (e *OntoConflictError) Error() string {
	return "onto_conflict: " + strings.Join(e.Paths, ", ")
}

// ontoPull is what a pull request's brief asks of its validation onto
// main: the brief's code and the refs its push may touch.
type ontoPull struct {
	code  gateway.JobCode
	rules []gateway.JobPushRef
	// when is the validation's request, the applied commit's date when
	// the brief fixes none: a retried run writes the same commit, and its
	// create-only push finds it already there.
	when time.Time
}

// pullRequestOnto is the onto the brief of pr's validation names, nil
// for a brief without one or a validation this process did not claim
// through claimJob (its older claim carries no brief): validated on its
// head, as before.
func pullRequestOnto(gw any, pr gateway.MainValidationPullRequest) *ontoPull {
	r, ok := gw.(briefReader)
	if !ok {
		return nil
	}
	b, ok := r.JobBriefOf(queueValidate, gateway.JobSubject{Type: subjectPullRequest, ID: pr.PullRequestValidationID})
	if !ok || b == nil || b.Code.Onto == nil {
		return nil
	}
	o := &ontoPull{code: b.Code, when: pr.RequestedAt}
	if b.Git != nil {
		o.rules = b.Git.Push
	}
	return o
}

// ontoValidator is the identity a validation's merge is written as when
// its brief names none: the same on every node, so the commit is too.
var ontoValidator = Identity{Name: "Mesh validator", Email: "mesh-validator@runsheet.dev"}

// mergeOnto merges the checked-out head at t.dir onto c.onto, checks the
// merge out detached and pushes it to the brief's pushTo, and answers c
// as the merge: the commit validateCheckout holds the attestation to. A
// head that does not merge is an *OntoConflictError. Anything else that
// stops it — the fetch, a git older than 2.40, the push — is logged and
// c answered as it was: the head validated alone, as before, which the
// platform still takes.
func (r *Runner) mergeOnto(ctx context.Context, t validationTarget, c mainCommit) (mainCommit, error) {
	o, prefix := c.onto, t.prefix
	asBefore := func(format string, args ...any) (mainCommit, error) {
		r.logf("%s: onto: %s; the head validated alone", prefix, fmt.Sprintf(format, args...))
		return c, nil
	}
	ref := strings.TrimPrefix(strings.TrimPrefix(o.code.Onto.Ref, "refs/heads/"), "origin/")
	if ref == "" {
		ref = t.base
	}
	fctx, err := r.validator().onFreshItemToken(ctx, t)
	if err != nil {
		return asBefore("github token for %s: %v", c.token, err)
	}
	// onto's branch fetched with history enough to share a merge base
	// with the head
	if _, err := r.WS.ChangedPaths(fctx, t.dir, ref); err != nil {
		return asBefore("fetch %s: %v", ref, err)
	}
	git := func(args ...string) (string, error) {
		out, err := hardenedGitCmd(ctx, "", t.dir, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	want := o.code.Onto.SHA
	if want == "" {
		want = "origin/" + ref
	}
	tip, err := git("rev-parse", "--verify", "--quiet", want+"^{commit}")
	if err != nil || tip == "" {
		return asBefore("%s is not on %s as fetched", short(want), ref)
	}
	var changes []gitapply.Change
	for _, a := range o.code.Apply {
		head := a.SHA
		if head == "" {
			head = c.sha
		}
		if _, err := git("cat-file", "-e", head+"^{commit}"); err != nil {
			return asBefore("%s to apply is not in the checkout", short(head))
		}
		changes = append(changes, gitapply.Change{Head: head, Base: a.Base})
	}
	if len(changes) == 0 {
		changes = []gitapply.Change{{Head: c.sha}}
	}
	mode, err := applyMode(o.code, gitapply.Merge)
	if err != nil {
		return asBefore("%v", err)
	}
	id, err := applyIdentity(o.code.ApplyAs, ontoValidator, o.when)
	if err != nil {
		return asBefore("%v", err)
	}
	if id.When.IsZero() {
		id.When = time.Now()
	}
	steps, conflict, err := ontoGit().Apply(ctx, t.dir, tip, changes, mode, id)
	switch {
	case errors.Is(err, gitapply.ErrGitTooOld):
		return asBefore("%v", err)
	case err != nil:
		return asBefore("apply onto %s %s: %v", ref, short(tip), err)
	case conflict != nil:
		err := &OntoConflictError{Paths: conflict.Paths}
		r.logf("%s: %s with %s %s", prefix, err, ref, short(tip))
		return c, err
	case len(steps) == 0:
		return asBefore("apply onto %s %s wrote nothing", ref, short(tip))
	}
	merged := steps[len(steps)-1].Commit
	if out, err := hardenedGitCmd(ctx, "", t.dir, "checkout", "--quiet", "--detach", merged).CombinedOutput(); err != nil {
		return asBefore("check out %s: %v: %s", short(merged), err, strings.TrimSpace(string(out)))
	}
	if push := o.code.PushTo; push != "" {
		pp, ok := r.WS.(pinPusher)
		if !ok {
			return r.backToHead(ctx, t, c, "this workspace pushes no ref")
		}
		pushed, err := pp.PushPin(WithPushRules(fctx, o.rules), t.dir, push)
		if err != nil {
			var seen []string
			if t.repoTok != nil {
				seen = append(seen, t.repoTok.Token)
			}
			return r.backToHead(ctx, t, c, fmt.Sprintf("push %s: %v", push, scrubbed(err, seen)))
		}
		if pushed != merged {
			return r.backToHead(ctx, t, c, fmt.Sprintf("pushed %s to %s, not the merge %s", short(pushed), push, short(merged)))
		}
		r.logf("%s: onto: merged %s onto %s %s as %s, pushed to %s", prefix, short(c.sha), ref, short(tip), short(merged), push)
	} else {
		r.logf("%s: onto: merged %s onto %s %s as %s", prefix, short(c.sha), ref, short(tip), short(merged))
	}
	c.what = fmt.Sprintf("%s merged onto %s %s", c.what, ref, short(tip))
	c.sha = merged
	return c, nil
}

// backToHead checks c's head out again after a merge that cannot be
// handed back, and answers c: the head validated alone, as before.
func (r *Runner) backToHead(ctx context.Context, t validationTarget, c mainCommit, why string) (mainCommit, error) {
	if out, err := hardenedGitCmd(ctx, "", t.dir, "checkout", "--quiet", "--detach", c.sha).CombinedOutput(); err != nil {
		return c, fmt.Errorf("onto: %s, and the head %s could not be checked out again: %v: %s", why, short(c.sha), err, strings.TrimSpace(string(out)))
	}
	r.logf("%s: onto: %s; the head validated alone", t.prefix, why)
	return c, nil
}
