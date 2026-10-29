package nodetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

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

// PushTool is the push node tool's name, as a kind's node_tools names it.
const PushTool = "push"

// pushTimeout bounds one push.
const pushTimeout = 5 * time.Minute

// rewrites are the checkout config keys that could send a push other
// than where the node names: a URL rewrite applies to an explicit URL as
// well as to a remote, so a checkout holding one is refused.
var rewrites = regexp.MustCompile(`(?i)^url\..*\.(insteadof|pushinsteadof)$`)

// Grant is a lease's grant on the node's git relay: its loopback base,
// to which a repository's "<owner>/<name>.git" is added.
type Grant interface {
	Base() string
}

// Push is one lease's push tool.
type Push struct {
	Grant     Grant
	Repo      string
	Workspace string
	// Refusal answers the relay's latest refusal of a push of the lease
	// since a moment, "" when there is none.
	Refusal  func(since time.Time) string
	Recorder *Recorder
}

// Tool is push {ref, from?}.
func (p *Push) Tool() mcptool.Tool {
	return mcptool.Tool{
		Name: PushTool,
		Description: "Push a commit of your checkout to a ref of the job's repository, through the node's git relay. " +
			"The job's push rules decide which refs you may push; a push must fast-forward the ref unless its rule says otherwise, " +
			"a create-only ref can never be moved, and nothing can be deleted. Answers the ref and the commit pushed, " +
			"and the evidence id of the node's signed record of the push.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ref":  map[string]any{"type": "string", "description": "the ref to push to: refs/heads/<branch>, or a branch name"},
				"from": map[string]any{"type": "string", "description": "the commit to push, any revision of your checkout; HEAD when absent"},
			},
			"required": []string{"ref"},
		},
		Call: p.call,
	}
}

func (p *Push) call(ctx context.Context, args map[string]any) (string, error) {
	if p.Grant == nil || p.Repo == "" {
		return "", errors.New("this job has no repository to push to")
	}
	ref, _ := args["ref"].(string)
	from, _ := args["from"].(string)
	ref, from = strings.TrimSpace(ref), strings.TrimSpace(from)
	if ref == "" {
		return "", errors.New("ref is required")
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	if from == "" {
		from = "HEAD"
	}
	if strings.HasPrefix(from, "-") || strings.HasPrefix(ref, "-") {
		return "", errors.New("ref and from are revisions, not options")
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
	tree, _ := git(ctx, p.Workspace, "rev-parse", "--verify", "-q", sha+"^{tree}")
	tree = strings.TrimSpace(tree)
	if keys, _ := git(ctx, p.Workspace, "config", "--local", "--name-only", "--get-regexp", `^url\.`); keys != "" {
		for _, k := range strings.Fields(keys) {
			if rewrites.MatchString(k) {
				return "", fmt.Errorf("your checkout's git config rewrites URLs (%s): remove it, the node pushes only through its relay", k)
			}
		}
	}
	start := time.Now()
	target := p.Grant.Base() + p.Repo + ".git"
	out, pushErr := git(ctx, p.Workspace, "push", "--porcelain", "--no-verify", target, sha+":"+ref)
	ended := time.Now()

	rec := Record{Tool: PushTool, StartedAt: start, EndedAt: ended,
		InputDigest: InputDigest(map[string]string{"repo": p.Repo, "ref": ref, "sha": sha, "tree": tree})}
	var refused error
	if pushErr != nil {
		rec.Exit = 1
		if why := p.Refusal(start); why != "" {
			refused = fmt.Errorf("the relay refused the push to %s: %s", ref, why)
		} else {
			refused = fmt.Errorf("pushing %s to %s: %s", short(sha), ref, strings.ReplaceAll(lastLines(out, 5), target, "<relay>"))
		}
		rec.Summary = refused.Error()
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
	if recErr != nil {
		answer["evidence_error"] = recErr.Error()
	} else {
		answer["evidence_id"] = id
	}
	b, _ := json.Marshal(answer)
	return string(b), nil
}
