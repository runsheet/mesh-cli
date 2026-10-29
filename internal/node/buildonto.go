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

// buildOnto puts a build's change onto the brief's onto before it is
// validated (runsheet ADR 077, the queue validates the would-be merge):
// the onto ref fetched on the item's token, the change — the checkout's
// base to HEAD — applied onto it (gitapply, linear unless the brief says
// merge), and the branch reset --keep to the result. The build's base is
// then onto (b.baseSHA, which the finish names and the changeset is
// diffed from) and the suites are selected by the diff from it
// (b.ontoSHA); the retry turn works on the new head and the push carries
// it.
//
// A change that conflicts with onto leaves the head as it was, is
// validated on its own base as before, and answers true: its finish says
// rebaseNeeded. Anything else that stops it — the fetch, a git older
// than 2.40, the reset — is noted on the item and the build validated as
// before.
func (r *Runner) buildOnto(ctx context.Context, b *buildRun, code gateway.JobCode) (rebaseNeeded bool) {
	note := func(format string, args ...any) {
		msg := "onto: " + fmt.Sprintf(format, args...)
		r.logf("item %s: %s", b.label, msg)
		b.report(ctx, msg)
	}
	ref := strings.TrimPrefix(strings.TrimPrefix(code.Onto.Ref, "refs/heads/"), "origin/")
	if ref == "" && code.Onto.SHA == "" {
		note("the brief's onto names no ref and no commit; validated on the item's base")
		return false
	}
	git := func(args ...string) (string, error) {
		out, err := hardenedGitCmd(ctx, "", b.dir, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	if ref != "" {
		// the onto ref fetched as the validation's base is, on the item's
		// token, with history enough to share a merge base with HEAD
		if _, err := r.WS.ChangedPaths(r.validator().onItemToken(ctx, b.target()), b.dir, ref); err != nil {
			note("fetch %s: %v; validated on the item's base", ref, b.acct.RedactErr(err))
			return false
		}
	}
	want := code.Onto.SHA
	if want == "" {
		want = "origin/" + ref
	}
	tip, err := git("rev-parse", "--verify", "--quiet", want+"^{commit}")
	if err != nil || tip == "" {
		note("%s is not in the checkout; validated on the item's base", want)
		return false
	}
	head, err := gitHead(ctx, b.dir)
	if err != nil {
		note("HEAD: %v; validated on the item's base", err)
		return false
	}
	ancestor := func(a, d string) bool {
		_, err := git("merge-base", "--is-ancestor", a, d)
		return err == nil
	}
	where := short(tip)
	if ref != "" {
		where = ref + " " + short(tip)
	}
	if ancestor(tip, head) {
		r.logf("item %s: onto: %s already holds %s's base; validated as it is", b.label, short(head), where)
		b.baseSHA, b.ontoSHA = tip, tip
		return false
	}
	base := b.baseSHA
	if base == "" || !ancestor(base, head) {
		if base, err = git("merge-base", tip, head); err != nil || base == "" {
			note("%s shares no history with %s; validated on the item's base", short(head), where)
			return false
		}
	}
	// the change as one commit: its commits' messages, oldest first
	msg, _ := git("log", "--reverse", "--format=%B", base+".."+head)
	id, err := applyIdentity(code.ApplyAs, b.ident, time.Now())
	if err != nil {
		note("%v; validated on the item's base", err)
		return false
	}
	mode, err := applyMode(code, gitapply.Linear)
	if err != nil {
		note("%v; validated on the item's base", err)
		return false
	}
	steps, conflict, err := ontoGit().Apply(ctx, b.dir, tip, []gitapply.Change{{Head: head, Base: base, Message: strings.TrimSpace(msg) + "\n"}}, mode, id)
	switch {
	case errors.Is(err, gitapply.ErrGitTooOld):
		note("%v; validated on the item's base", err)
		return false
	case err != nil:
		note("apply onto %s: %v; validated on the item's base", where, err)
		return false
	case conflict != nil:
		note("the change conflicts with %s in %s; validated on the item's base, and the finish asks for a rebase", where, strings.Join(conflict.Paths, ", "))
		return true
	case len(steps) != 1:
		note("apply onto %s wrote %d commits, not one; validated on the item's base", where, len(steps))
		return false
	}
	if out, err := hardenedGitCmd(ctx, "", b.dir, "reset", "--quiet", "--keep", steps[0].Commit).CombinedOutput(); err != nil {
		note("reset to %s: %v: %s; validated on the item's base", short(steps[0].Commit), err, strings.TrimSpace(string(out)))
		return false
	}
	r.logf("item %s: onto: applied %s onto %s as %s (%s); validated there", b.label, short(head), where, short(steps[0].Commit), mode)
	b.baseSHA, b.ontoSHA = tip, tip
	return false
}
