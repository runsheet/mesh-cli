package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// Every write goes through the relay (Mesh v1 spec, "Many git vendors,
// and every write through the relay"). The push node tool
// (nodetools.Push) pushes a commit of the lease's checkout through the
// lease's grant — the relay's loopback capability, never a vendor URL —
// and the relay checks it against the brief's push rules and records it
// as the lease's AccessRecord. There is no other path from nodev1 to a
// vendor: no credential helper, no write token outside the relay and no
// fallback (guard_test.go).

// NodeToolsServer is the in-process MCP server the node's own tools are
// served at on a lease's sessionproxy, beside the product's servers.
const NodeToolsServer = "mesh-node"

// PushTool is the push node tool's name, as a kind's node_tools names it.
const PushTool = nodetools.PushTool

// refusalSince is the reason of the relay's latest refused push of the
// lease since since, "" when there is none.
func (r *Runtime) refusalSince(lease string, since time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var recs []meshapi.AccessRecord
	if l := r.lease(lease); l != nil {
		recs = l.access
	}
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

// finalPin is what a lease's grant of one exact, create-only pin ref
// becomes once the session has ended (adr-050): the node, not the
// session, pushes push_to's final head to the brief's pin — unless the
// session already pushed the pin itself, in which case it is left
// exactly where the session put it, mismatch or not. finish is the
// settled finish result; finalPin returns it unchanged when the brief
// names no pin, no push_to, or no way to reach the relay, and otherwise
// returns it with a "pin" key added naming what happened.
func (r *Runtime) finalPin(ctx context.Context, b *meshapi.Brief, grant *gitrelay.Grant, pusher nodetools.Pusher, workspace string, finish json.RawMessage) json.RawMessage {
	pin := ""
	if b.Git != nil {
		pin = b.Git.Pin()
	}
	if pin == "" || (grant == nil && pusher == nil) || b.Code == nil || b.Code.PushTo == "" {
		return finish
	}
	git := leaseGitRun(ctx)
	headRef := fullRef(b.Code.PushTo)
	headSHA, err := remoteHead(git, workspace, headRef)
	if err != nil {
		r.logf("nodev1: lease %s: the pin %s: reading %s: %v", b.LeaseID, pin, headRef, err)
		return finish
	}
	if headSHA == "" {
		return finish
	}
	pinSHA, err := remoteHead(git, workspace, pin)
	if err != nil {
		r.logf("nodev1: lease %s: the pin %s: reading its own state: %v", b.LeaseID, pin, err)
		return finish
	}
	if pinSHA != "" {
		result := map[string]any{"ref": pin, "sha": pinSHA, "head_sha": headSHA, "pushed": false}
		if pinSHA != headSHA {
			result["mismatch"] = true
			result["reason"] = fmt.Sprintf("the pin %s already holds %s, which is not the branch's final head %s; left unmoved", pin, short(pinSHA), short(headSHA))
			r.logf("nodev1: lease %s: the pin %s holds %s, not the final head %s; left unmoved", b.LeaseID, pin, short(pinSHA), short(headSHA))
		}
		return withPin(finish, result)
	}
	repo := ""
	if b.Git != nil {
		repo = b.Git.Repo
	}
	result := map[string]any{"ref": pin, "head_sha": headSHA}
	if err := pushPin(ctx, grant, pusher, workspace, repo, pin, headSHA); err != nil {
		result["pushed"] = false
		result["reason"] = err.Error()
		r.logf("nodev1: lease %s: pinning %s to %s: %v", b.LeaseID, pin, short(headSHA), err)
		return withPin(finish, result)
	}
	result["sha"] = headSHA
	result["pushed"] = true
	r.logf("nodev1: lease %s: pinned %s to its final head %s", b.LeaseID, pin, short(headSHA))
	return withPin(finish, result)
}

// remoteHead is ref's current head in the vendor's repository, read
// through the checkout's own origin remote — the relay's loopback,
// never a vendor URL — "" when the ref does not exist there. A session
// could otherwise point "origin" elsewhere with a url.*.insteadOf
// rewrite in the checkout's own config (unaffected by githarden, which
// clears command drivers but not URL rewrites) the same way it could
// redirect its own push, so this refuses first (nodetools.CheckRewrites,
// the push tool's own check).
func remoteHead(git func(string, ...string) (string, error), workspace, ref string) (string, error) {
	if err := nodetools.CheckRewrites(git, workspace); err != nil {
		return "", err
	}
	out, err := git(workspace, "ls-remote", "origin", ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == ref {
			return f[0], nil
		}
	}
	return "", nil
}

// pushPin pushes sha to the pin ref as the node, by the push tool's own
// paths (nodetools.NodePush), so the relay's create-only rule judges it.
func pushPin(ctx context.Context, grant *gitrelay.Grant, pusher nodetools.Pusher, workspace, repo, ref, sha string) error {
	var g nodetools.Grant
	if grant != nil {
		g = grant
	}
	return nodetools.NodePush(ctx, g, pusher, workspace, repo, ref, sha)
}

// withPin adds pin's result as finish's "pin" key: finish is always a
// JSON object when non-nil (settle.go), so this never fails in
// practice.
func withPin(finish json.RawMessage, pin map[string]any) json.RawMessage {
	var obj map[string]json.RawMessage
	if len(finish) == 0 || json.Unmarshal(finish, &obj) != nil || obj == nil {
		obj = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(pin)
	if err != nil {
		return finish
	}
	obj["pin"] = raw
	out, err := json.Marshal(obj)
	if err != nil {
		return finish
	}
	return out
}
