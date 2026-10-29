package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gitapply"
	"projectmesh.io/mesh-cli/internal/sessionunit"
	"projectmesh.io/mesh-cli/internal/validate"
)

// A merge batch's chain candidate (runsheet ADR 079, the Zuul gate's
// wave 1): a chain batch holds one member, stacked on the chain batches
// ahead of it, and no candidate exists until a node writes it. Its
// validation's brief names onto (main's head when the chain was
// formed), apply (the members ahead, then this batch's own, each with
// its ref), applyMode merge, applyAs (the integrator, its date the root
// batch's formed_at, so every node writes the same shas) and pushTo
// (refs/heads/<candidate branch>, the one ref the node may write). The
// node clones the repository through the relay, fetches onto and every
// change, applies them in order (gitapply, merge mode), pushes the tip
// to pushTo, validates and attests the tip, and finishes with the
// attestation and a `candidate {sha, tree}` part; a change that does not
// merge finishes with a `conflict {index, paths}` part and nothing
// pushed or attested — the platform's to act on, never a failure of the
// node's. A brief without apply is a candidate the platform built, and
// validated as it always was.

// ChainConflictError is a chain candidate whose changes did not apply:
// the change at Index of the brief's apply (Head) does not merge onto
// what the ones before it wrote, on Paths. The loop finishes the
// validation with it as the conflict part (runMainJob); it is no failure
// of the node's.
type ChainConflictError struct {
	Index int
	Head  string
	Paths []string
}

func (e *ChainConflictError) Error() string {
	return fmt.Sprintf("chain_conflict: change %d (%s): %s", e.Index, short(e.Head), strings.Join(e.Paths, ", "))
}

// chainCandidate is what a chain batch's brief asks of its validation:
// the brief's code and the refs its push may touch, and when the batch's
// validation was requested — the written commits' date when the brief
// fixes none.
type chainCandidate struct {
	code  gateway.JobCode
	rules []gateway.JobPushRef
	when  time.Time
}

// candidateChain is the chain the brief of batch's validation names, nil
// for a brief without apply or a validation this process did not claim
// through claimJob (its older claim carries no brief): a candidate the
// platform built, validated as before.
func candidateChain(gw any, batch gateway.MainValidationBatch) *chainCandidate {
	r, ok := gw.(briefReader)
	if !ok {
		return nil
	}
	b, ok := r.JobBriefOf(queueValidate, gateway.JobSubject{Type: subjectCandidate, ID: batch.MergeBatchID})
	if !ok || b == nil || len(b.Code.Apply) == 0 {
		return nil
	}
	ch := &chainCandidate{code: b.Code, when: batch.RequestedAt}
	if b.Git != nil {
		ch.rules = b.Git.Push
	}
	return ch
}

// chainClones makes a chain candidate's checkout (*Git).
type chainClones interface {
	AddChainClone(ctx context.Context, repo, dir, onto string, wants []string) error
	RemoveWorkspace(ctx context.Context, dir string) error
}

// chainValidator is the identity a chain's commits are written as when
// its brief names none: the same on every node, so the commits are too.
var chainValidator = ontoValidator

// validateChain writes, pushes and validates a chain batch's candidate
// at t.dir (the package comment): the repository cloned on t's token —
// through the relay when it is on — with onto's branch and every change
// fetched; the changes applied in order onto onto; the tip checked out
// detached, pushed to the brief's pushTo under its push rules, and
// validated in c's place, its changed paths taken from onto. A change
// that does not merge is a *ChainConflictError with nothing pushed or
// run. Every other stop is the validation's error — the loop fails it
// with the reason, and the platform hands it to another node — since a
// chain has no candidate of the platform's to fall back on. The clone
// is removed on every path.
func (r *Runner) validateChain(ctx context.Context, t validationTarget, c mainCommit) (validate.Attestation, error) {
	ch, prefix := c.chain, t.prefix
	push := ch.code.PushTo
	if push == "" {
		return validate.Attestation{}, errors.New("chain: the brief names no pushTo; not validated")
	}
	wt, ok := r.WS.(chainClones)
	if !ok {
		return validate.Attestation{}, ErrNoCandidateWorktrees
	}
	dir := t.dir
	defer func() {
		if err := wt.RemoveWorkspace(context.WithoutCancel(ctx), dir); err != nil {
			r.logf("%s: remove %s: %v", prefix, dir, err)
		}
		_ = os.RemoveAll(sessionunit.Dir(dir))
		_ = os.Remove(dir + ".attestation.json")
	}()
	fctx, err := r.validator().onFreshItemToken(ctx, t)
	if err != nil {
		err = &fetchTokenError{name: c.token, err: err}
		r.logf("%s: %v; not fetched", prefix, err)
		return validate.Attestation{}, err
	}
	ref := t.base
	if ch.code.Onto != nil {
		if o := strings.TrimPrefix(strings.TrimPrefix(ch.code.Onto.Ref, "refs/heads/"), "origin/"); o != "" {
			ref = o
		}
	}
	// each change by its ref, else its sha: a ref is fetched as it is
	// (fetchRefspec), a sha by itself
	var wants []string
	for _, a := range ch.code.Apply {
		if w := a.Ref; w != "" {
			wants = append(wants, w)
		} else if a.SHA != "" {
			wants = append(wants, a.SHA)
		}
	}
	if err := wt.AddChainClone(fctx, c.repo, dir, ref, wants); err != nil {
		return validate.Attestation{}, validate.Classify(fmt.Errorf("fetch %s and the chain's %d changes: %w", ref, len(ch.code.Apply), err))
	}
	git := func(args ...string) (string, error) {
		out, err := hardenedGitCmd(ctx, "", dir, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	want := "refs/remotes/origin/" + ref
	if ch.code.Onto != nil && ch.code.Onto.SHA != "" {
		want = ch.code.Onto.SHA
	}
	tip, err := git("rev-parse", "--verify", "--quiet", want+"^{commit}")
	if err != nil || tip == "" {
		return validate.Attestation{}, fmt.Errorf("chain: onto %s is not on %s as fetched; not validated", short(want), ref)
	}
	changes := make([]gitapply.Change, 0, len(ch.code.Apply))
	for i, a := range ch.code.Apply {
		head := a.SHA
		if head == "" {
			_, local := fetchRefspec(a.Ref)
			if head, err = git("rev-parse", "--verify", "--quiet", local+"^{commit}"); err != nil || head == "" {
				return validate.Attestation{}, fmt.Errorf("chain: change %d's ref %s was not fetched; not validated", i, a.Ref)
			}
		}
		if _, err := git("cat-file", "-e", head+"^{commit}"); err != nil {
			return validate.Attestation{}, fmt.Errorf("chain: change %d's head %s is not in the clone; not validated", i, short(head))
		}
		changes = append(changes, gitapply.Change{Head: head, Base: a.Base})
	}
	mode, err := applyMode(ch.code, gitapply.Merge)
	if err != nil {
		return validate.Attestation{}, fmt.Errorf("chain: %w", err)
	}
	id, err := applyIdentity(ch.code.ApplyAs, chainValidator, ch.when)
	if err != nil {
		return validate.Attestation{}, fmt.Errorf("chain: %w", err)
	}
	if id.When.IsZero() {
		id.When = time.Now()
	}
	steps, conflict, err := ontoGit().Apply(ctx, dir, tip, changes, mode, id)
	switch {
	case err != nil:
		// a git older than gitapply.MinGit among them: another node may
		// apply it
		return validate.Attestation{}, fmt.Errorf("chain: apply onto %s %s: %w", ref, short(tip), err)
	case conflict != nil:
		ce := &ChainConflictError{Index: conflict.Index, Head: conflict.Head, Paths: conflict.Paths}
		r.logf("%s: chain: %s, onto %s %s; nothing pushed", prefix, ce, ref, short(tip))
		return validate.Attestation{}, ce
	case len(steps) != len(changes):
		return validate.Attestation{}, fmt.Errorf("chain: applied %d of %d changes onto %s %s", len(steps), len(changes), ref, short(tip))
	}
	written := steps[len(steps)-1].Commit
	if out, err := hardenedGitCmd(ctx, "", dir, "checkout", "--quiet", "--detach", written).CombinedOutput(); err != nil {
		return validate.Attestation{}, fmt.Errorf("chain: check out %s: %v: %s", short(written), err, strings.TrimSpace(string(out)))
	}
	pp, ok := r.WS.(pinPusher)
	if !ok {
		return validate.Attestation{}, errors.New("chain: this workspace pushes no ref; not validated")
	}
	pushed, err := pp.PushPin(WithPushRules(fctx, ch.rules), dir, push)
	if err != nil {
		var seen []string
		if tok := t.cred.Token(); tok != nil {
			seen = append(seen, tok.Token)
		}
		if t.repoTok != nil {
			seen = append(seen, t.repoTok.Token)
		}
		return validate.Attestation{}, validate.Classify(fmt.Errorf("chain: push %s: %w", push, scrubbed(err, seen)))
	}
	if pushed != written {
		return validate.Attestation{}, fmt.Errorf("chain: pushed %s to %s, not the candidate %s", short(pushed), push, short(written))
	}
	r.logf("%s: chain: applied %d changes onto %s %s as %s, pushed to %s", prefix, len(changes), ref, short(tip), short(written), push)
	c.what = fmt.Sprintf("chain candidate %s onto %s %s", short(written), ref, short(tip))
	c.sha = written
	t.diffFrom = tip
	return r.validateCheckout(ctx, t, c)
}

// fullSHA is a commit's full hex name: a chain change asked for by its
// sha alone is fetched as that, not as a branch.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// AddChainClone makes a chain candidate's checkout at dir (ADR-039, a
// clone of its own): a checkout a crashed run left there removed first,
// repo cloned into dir alone with no checkout, onto's branch and every
// want — a ref fetched as fetchRefspec spells it, a full sha by itself —
// fetched in one fetch under the per-repo fetch lock. Nothing is checked
// out: the caller writes the candidate and checks that out. Its fetches
// go out on whatever token ctx carries (OnItemToken), through the relay
// when it is on.
func (g *Git) AddChainClone(ctx context.Context, repo, dir, onto string, wants []string) error {
	if _, err := os.Stat(dir); err == nil {
		if err := g.RemoveWorkspace(ctx, dir); err != nil {
			return err
		}
	}
	root, key := g.lockRoot("", dir), RepoLockKey(repo)
	if err := g.cloneOwn(ctx, root, key, repo, dir, "", "--no-checkout"); err != nil {
		return err
	}
	spec, _ := fetchRefspec(onto)
	args := []string{"fetch", "--quiet", "origin", spec}
	for _, w := range wants {
		if fullSHA.MatchString(w) {
			args = append(args, w)
			continue
		}
		s, _ := fetchRefspec(w)
		args = append(args, s)
	}
	_, err := g.fetchLocked(ctx, root, key, dir, "git", args...)
	return err
}
