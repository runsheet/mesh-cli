package nodetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/mcptool"
)

// Every write goes through the relay (Mesh v1 spec, "Many git vendors,
// and every write through the relay"). The push tool is how a session
// writes its work to the vendor: it pushes a commit of the lease's
// checkout to a ref through the lease's grant — the relay's loopback
// capability, never a vendor URL, whatever the checkout's remotes say.
// The relay checks the ref against the brief's push rules, refuses a
// deletion and proves a fast-forward, and records the push, allowed or
// refused, as the lease's AccessRecord. The tool records what it pushed
// (or what the relay refused) as its signed ToolRecord.
//
// A push takes a lease (mesh-specs gaps, 35; gitrelay/lease.go): a
// rebased push_to is never a fast-forward, so `lease` replaces push_to
// only if it is still where the job found it — "head", or that commit —
// and a branch someone pushed to since is refused naming where it is
// now, never overwritten. Without a lease a push stays fast-forward
// only. The ref defaults to the brief's push_to (gap 24).

// PushTool is the push node tool's name, as a kind's node_tools names it.
const PushTool = "push"

// pushTimeout bounds one push.
const pushTimeout = 5 * time.Minute

// rewrites are the checkout config keys that could send a push other
// than where the node names: a URL rewrite applies to an explicit URL as
// well as to a remote, so a checkout holding one is refused.
var rewrites = regexp.MustCompile(`(?i)^url\..*\.(insteadof|pushinsteadof)$`)

// gitRunner adapts the package's hardened git to the signature
// CheckRewrites and nodev1's own git readers share: a dir and its args,
// ctx already bound.
func gitRunner(ctx context.Context) func(dir string, args ...string) (string, error) {
	return func(dir string, args ...string) (string, error) { return git(ctx, dir, args...) }
}

// CheckRewrites refuses naming the key when workspace's local git config
// rewrites a URL (insteadOf or pushInsteadOf): since a rewrite applies to
// an explicit URL as well as to a named remote (rewrites, above), a
// session could use one to redirect the node's own post-session reads
// and pushes — through a lease's grant, an explicit loopback URL, or
// "origin" — the same way it could redirect a session's own push. Every
// git the node itself runs against a checkout a session may have
// written to checks this first: the push tool's own call, NodePush, and
// nodev1's remoteHead.
func CheckRewrites(run func(dir string, args ...string) (string, error), workspace string) error {
	keys, _ := run(workspace, "config", "--local", "--name-only", "--get-regexp", `^url\.`)
	for _, k := range strings.Fields(keys) {
		if rewrites.MatchString(k) {
			return fmt.Errorf("your checkout's git config rewrites URLs (%s): remove it, the node pushes only through its relay", k)
		}
	}
	return nil
}

// Grant is a lease's grant on the node's git relay: its loopback base,
// to which a repository's "<owner>/<name>.git" is added.
type Grant interface {
	Base() string
}

// PushOutcome is a push a lease's relay sidecar made for the push tool:
// Pushed, or Refused in the relay's own words, or Failed with git's.
type PushOutcome struct {
	Pushed  bool
	Refused string
	Failed  string
	// Lease is the commit a leased push replaced the ref from.
	Lease string
}

// Pusher pushes sha to ref through the lease's relay sidecar
// (projectmesh-io/api ADR 001 amendment "relay sidecar"): the one place
// every push is checked, so the tool holds no git token and checks no
// push rule itself. lease, set, pushes under a lease: the commit ref
// must be at, or gitrelay.LeaseHeadAsked for where the job found it,
// which the sidecar's relay holds. An error is the sidecar not
// answering.
type Pusher func(ctx context.Context, ref, sha, lease string) (PushOutcome, error)

// Heads is a Grant that holds where the job found the refs a leased
// push may replace (gitrelay.Grant.Head).
type Heads interface {
	Head(ref string) (string, bool)
}

// Push is one lease's push tool.
type Push struct {
	// Pusher, when set, pushes in place of git through Grant: a
	// sandboxed lease whose relay is a sidecar in its pod.
	Pusher    Pusher
	Grant     Grant
	Repo      string
	Workspace string
	// PushTo is the brief's push_to: the ref a push names when it names
	// none, and the one ref a lease may replace.
	PushTo string
	// Refusal answers the relay's latest refusal of a push of the lease
	// since a moment, "" when there is none.
	Refusal  func(since time.Time) string
	Recorder *Recorder
	// Tainted says the lease read untrusted content this session and so
	// lost its push for the rest of the lease (taint.go): a call is
	// refused at once, before Grant or Pusher is ever touched. Nil, or
	// answering false, changes nothing.
	Tainted func() bool
}

// Tool is push {ref?, from?, lease?}.
func (p *Push) Tool() mcptool.Tool {
	refDesc := "the ref to push to: refs/heads/<branch>, or a branch name"
	if p.PushTo != "" {
		refDesc += "; the job's push_to (" + fullRef(p.PushTo) + ") when absent"
	}
	return mcptool.Tool{
		Name: PushTool,
		Description: "Push a commit of your checkout to a ref of the job's repository, through the node's git relay. " +
			"The job's push rules decide which refs you may push; a push must fast-forward the ref unless its rule says otherwise, " +
			"a create-only ref can never be moved, and nothing can be deleted. " +
			"A rebased or rewritten branch is not a fast-forward: push it with lease \"head\" to replace the job's push_to " +
			"only if it is still where the job found it; if someone pushed to it since, the push is refused naming where it is now, " +
			"and nothing is overwritten. Answers the ref and the commit pushed, and the evidence id of the node's signed record of the push.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ref":  map[string]any{"type": "string", "description": refDesc},
				"from": map[string]any{"type": "string", "description": "the commit to push, any revision of your checkout; HEAD when absent"},
				"lease": map[string]any{"type": "string", "description": "replace the ref even if the push is not a fast-forward, only if it is still at this commit: " +
					"\"head\" for where the job found it (or what the job last pushed there), or that commit's sha. Only the job's push_to takes a lease. " +
					"Absent: the push must fast-forward."},
			},
		},
		Call: p.call,
	}
}

// fullRef is ref as git names it: a branch name is refs/heads/<name>.
func fullRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "refs/") {
		return ref
	}
	return "refs/heads/" + ref
}

// leaseArg reads the lease a call asks for: "" for none,
// gitrelay.LeaseHeadAsked for the head, else a revision. true is the
// head too.
func leaseArg(v any) (string, error) {
	switch l := v.(type) {
	case nil:
		return "", nil
	case bool:
		if l {
			return gitrelay.LeaseHeadAsked, nil
		}
		return "", nil
	case string:
		l = strings.TrimSpace(l)
		if strings.EqualFold(l, gitrelay.LeaseHeadAsked) || strings.EqualFold(l, "true") {
			return gitrelay.LeaseHeadAsked, nil
		}
		if strings.HasPrefix(l, "-") {
			return "", errors.New("lease is a commit, not an option")
		}
		return l, nil
	}
	return "", errors.New(`lease is "head" or a commit sha`)
}

func (p *Push) call(ctx context.Context, args map[string]any) (string, error) {
	if p.Tainted != nil && p.Tainted() {
		return "", errors.New("this lease is tainted by untrusted content it read; push is refused for the rest of the lease")
	}
	if (p.Grant == nil && p.Pusher == nil) || p.Repo == "" {
		return "", errors.New("this job has no repository to push to")
	}
	ref, _ := args["ref"].(string)
	from, _ := args["from"].(string)
	ref, from = strings.TrimSpace(ref), strings.TrimSpace(from)
	pushTo := fullRef(p.PushTo)
	if ref == "" {
		ref = pushTo
	}
	if ref == "" {
		return "", errors.New("ref is required: this job names no push_to")
	}
	if strings.HasPrefix(ref, "-") {
		return "", errors.New("ref and from are revisions, not options")
	}
	ref = fullRef(ref)
	if from == "" {
		from = "HEAD"
	}
	if strings.HasPrefix(from, "-") {
		return "", errors.New("ref and from are revisions, not options")
	}
	lease, err := leaseArg(args["lease"])
	if err != nil {
		return "", err
	}
	if lease != "" && (pushTo == "" || ref != pushTo) {
		if pushTo == "" {
			return "", fmt.Errorf("%s takes no lease: this job names no push_to, and a lease replaces only the job's push_to; push without one to fast-forward it", ref)
		}
		return "", fmt.Errorf("%s takes no lease: a lease replaces only the job's push_to (%s), from where the job found it; push without one to fast-forward %s", ref, pushTo, ref)
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	if _, err := git(ctx, p.Workspace, "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("%s is not a ref name", ref)
	}
	sha, err := git(ctx, p.Workspace, "rev-parse", "--verify", "-q", from+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s names no commit in your checkout", from)
	}
	sha = strings.TrimSpace(sha)
	if lease != "" && lease != gitrelay.LeaseHeadAsked && !fullID(lease) {
		// an abbreviated commit the checkout holds
		full, err := git(ctx, p.Workspace, "rev-parse", "--verify", "-q", lease+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("lease %s names no commit: give the full sha, or \"head\"", lease)
		}
		lease = strings.TrimSpace(full)
	}
	if lease == gitrelay.LeaseHeadAsked && p.Pusher == nil {
		h, ok := p.Grant.(Heads)
		head := ""
		if ok {
			head, ok = h.Head(ref)
		}
		if !ok {
			return "", fmt.Errorf("%s takes no lease: the node did not find where it was when the job started; push without one to fast-forward it", ref)
		}
		lease = head
	}
	tree, _ := git(ctx, p.Workspace, "rev-parse", "--verify", "-q", sha+"^{tree}")
	tree = strings.TrimSpace(tree)
	if err := CheckRewrites(gitRunner(ctx), p.Workspace); err != nil {
		return "", err
	}
	start := time.Now()
	var refused error
	if p.Pusher != nil {
		res, err := p.Pusher(ctx, ref, sha, lease)
		switch {
		case err != nil:
			refused = fmt.Errorf("pushing %s to %s: %v", short(sha), ref, err)
		case res.Refused != "":
			refused = fmt.Errorf("the relay refused the push to %s: %s", ref, res.Refused)
		case !res.Pushed:
			refused = fmt.Errorf("pushing %s to %s: %s%s", short(sha), ref, res.Failed, p.leaseHint(ref, lease, res.Failed))
		}
		if lease != "" {
			lease = res.Lease
		}
	} else {
		target := p.Grant.Base() + p.Repo + ".git"
		pushArgs := []string{"push", "--porcelain", "--no-verify", target, sha + ":" + ref}
		if lease != "" {
			pushArgs = gitrelay.LeasedPushArgs(target, sha, ref, lease)
		}
		out, pushErr := git(ctx, p.Workspace, pushArgs...)
		if pushErr != nil {
			if why := p.Refusal(start); why != "" {
				refused = fmt.Errorf("the relay refused the push to %s: %s", ref, why)
			} else if lease != "" && gitrelay.StaleLease(out) {
				// git refused the lease itself: the ref moved since
				ls, _ := git(ctx, p.Workspace, "ls-remote", target, ref)
				refused = fmt.Errorf("the push to %s was refused: %s", ref, gitrelay.StaleLeaseReason(ref, lease, gitrelay.RemoteHead(ls, ref)))
			} else {
				refused = fmt.Errorf("pushing %s to %s: %s%s", short(sha), ref, strings.ReplaceAll(lastLines(out, 5), target, "<relay>"), p.leaseHint(ref, lease, out))
			}
		}
	}
	ended := time.Now()

	input := map[string]string{"repo": p.Repo, "ref": ref, "sha": sha, "tree": tree}
	if lease != "" {
		input["lease"] = lease
	}
	rec := Record{Tool: PushTool, StartedAt: start, EndedAt: ended, InputDigest: InputDigest(input)}
	if refused != nil {
		rec.Exit = 1
		rec.Summary = refused.Error()
	} else if lease != "" {
		rec.Summary = fmt.Sprintf("pushed %s (tree %s) to %s of %s, leased from %s", sha, tree, ref, p.Repo, lease)
	} else {
		rec.Summary = fmt.Sprintf("pushed %s (tree %s) to %s of %s", sha, tree, ref, p.Repo)
	}
	id, recErr := p.Recorder.Record(context.WithoutCancel(ctx), rec, nil)
	if refused != nil {
		if recErr == nil {
			return "", fmt.Errorf("%v (evidence %s)", refused, id)
		}
		return "", refused
	}
	answer := map[string]any{"pushed": true, "ref": ref, "sha": sha, "tree": tree}
	if lease != "" {
		answer["lease"] = lease
	}
	if recErr != nil {
		answer["evidence_error"] = recErr.Error()
	} else {
		answer["evidence_id"] = id
	}
	b, _ := json.Marshal(answer)
	return string(b), nil
}

// NodePush pushes sha of workspace to ref as the node itself, not a
// session (nodev1's final pin, adr-050): through pusher, a lease's relay
// sidecar, when set, else through grant's loopback — the push tool's two
// paths, so the relay's push rules judge it as they judge a session's.
func NodePush(ctx context.Context, grant Grant, pusher Pusher, workspace, repo, ref, sha string) error {
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	if pusher != nil {
		res, err := pusher(ctx, ref, sha, "")
		switch {
		case err != nil:
			return fmt.Errorf("pushing %s to %s: %v", short(sha), ref, err)
		case res.Refused != "":
			return fmt.Errorf("the relay refused the push to %s: %s", ref, res.Refused)
		case !res.Pushed:
			return fmt.Errorf("pushing %s to %s: %s", short(sha), ref, res.Failed)
		}
		return nil
	}
	if grant == nil || repo == "" {
		return fmt.Errorf("no git relay to push %s through", ref)
	}
	if err := CheckRewrites(gitRunner(ctx), workspace); err != nil {
		return err
	}
	target := grant.Base() + repo + ".git"
	if out, err := git(ctx, workspace, "push", "--porcelain", "--no-verify", target, sha+":"+ref); err != nil {
		return fmt.Errorf("pushing %s to %s: %s", short(sha), ref, strings.ReplaceAll(lastLines(out, 5), target, "<relay>"))
	}
	return nil
}

// leaseHint is what a refused unleased push of push_to adds: git refused
// it as no fast-forward, which a lease from where the job found it can
// replace.
func (p *Push) leaseHint(ref, lease, out string) string {
	if lease != "" || ref != fullRef(p.PushTo) || !(strings.Contains(out, "non-fast-forward") || strings.Contains(out, "fetch first")) {
		return ""
	}
	return `; a rebased branch is not a fast-forward: push again with lease "head" to replace it only if it is still where the job found it`
}

// fullID says whether s is a full hex object id.
func fullID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
