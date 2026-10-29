package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// Every write goes through the relay (Mesh v1 spec, "Many git vendors,
// and every write through the relay"). The push node tool is how a
// session writes its work to the vendor: an in-process MCP tool on the
// lease's sessionproxy, run by the node, that pushes a commit of the
// lease's checkout to a ref through the lease's grant — the relay's
// loopback capability, never a vendor URL, whatever the checkout's
// remotes say. The relay checks the ref against the brief's push rules,
// refuses a deletion and proves a fast-forward, and records the push,
// allowed or refused, as the lease's AccessRecord. There is no other
// path from nodev1 to a vendor: no credential helper, no write token
// outside the relay and no fallback (guard_test.go).

// NodeToolsServer is the in-process MCP server the node's own tools are
// served at on a lease's sessionproxy, beside the product's servers.
const NodeToolsServer = "mesh-node"

// PushTool is the push node tool's name, as a kind's node_tools names it.
const PushTool = "push"

// pushTimeout bounds one push.
const pushTimeout = 5 * time.Minute

// pushConfig are git's -c flags for a push the node runs in a checkout
// the session has written to: no credential helper or askpass, no hook
// and no fsmonitor of the session's (each would run the session's
// command as the node), no proxy and no ssh command.
var pushConfig = []string{
	"-c", "credential.helper=", "-c", "core.askPass=", "-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false", "-c", "http.proxy=", "-c", "core.sshCommand=",
}

// rewrites are the checkout config keys that could send a push other
// than where the node names: a URL rewrite applies to an explicit URL as
// well as to a remote, so a checkout holding one is refused.
var rewrites = regexp.MustCompile(`(?i)^url\..*\.(insteadof|pushinsteadof)$`)

// pusher is one lease's push tool.
type pusher struct {
	grant     *gitrelay.Grant
	repo      string
	workspace string
	// refusal answers the relay's latest refusal of a push of the lease
	// since a moment, "" when there is none.
	refusal func(since time.Time) string
}

// pushTool is the lease's push tool: push {ref, from?}.
func (r *Runtime) pushTool(b *meshapi.Brief, grant *gitrelay.Grant, workspace string) mcptool.Tool {
	p := &pusher{grant: grant, workspace: workspace, refusal: func(since time.Time) string { return r.refusalSince(b.LeaseID, since) }}
	if b.Git != nil {
		p.repo = b.Git.Repo
	}
	return mcptool.Tool{
		Name: PushTool,
		Description: "Push a commit of your checkout to a ref of the job's repository, through the node's git relay. " +
			"The job's push rules decide which refs you may push; a push must fast-forward the ref unless its rule says otherwise, " +
			"a create-only ref can never be moved, and nothing can be deleted. Answers the ref and the commit pushed.",
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

func (p *pusher) call(ctx context.Context, args map[string]any) (string, error) {
	if p.grant == nil || p.repo == "" {
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
	if _, err := p.git(ctx, "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("%s is not a ref name", ref)
	}
	sha, err := p.git(ctx, "rev-parse", "--verify", "-q", from+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s names no commit in your checkout", from)
	}
	sha = strings.TrimSpace(sha)
	if keys, _ := p.git(ctx, "config", "--local", "--name-only", "--get-regexp", `^url\.`); keys != "" {
		for _, k := range strings.Fields(keys) {
			if rewrites.MatchString(k) {
				return "", fmt.Errorf("your checkout's git config rewrites URLs (%s): remove it, the node pushes only through its relay", k)
			}
		}
	}
	start := time.Now()
	target := p.grant.Base() + p.repo + ".git"
	out, err := p.git(ctx, append(append([]string(nil), pushConfig...), "push", "--porcelain", "--no-verify", target, sha+":"+ref)...)
	if err != nil {
		if why := p.refusal(start); why != "" {
			return "", fmt.Errorf("the relay refused the push to %s: %s", ref, why)
		}
		return "", fmt.Errorf("pushing %s to %s: %s", short(sha), ref, scrub(lastLines(out, 5), target))
	}
	answer, _ := json.Marshal(map[string]any{"pushed": true, "ref": ref, "sha": sha})
	return string(answer), nil
}

// git runs one git command in the checkout with no global or system
// config of the node's and no credential, answering its output.
func (p *pusher) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string(nil), noCredentials...), args...)...)
	cmd.Dir = p.workspace
	cmd.Env = append(gitEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// refusalSince is the reason of the relay's latest refused push of the
// lease since since, "" when there is none.
func (r *Runtime) refusalSince(lease string, since time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs := r.access[lease]
	for i := len(recs) - 1; i >= 0; i-- {
		rec := recs[i]
		if rec.At.Before(since.Add(-time.Second)) {
			break
		}
		if rec.Kind == "git" && rec.Service == "receive-pack" && rec.Outcome == "refused" {
			return rec.Reason
		}
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
