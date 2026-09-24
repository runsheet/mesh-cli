package commands

import (
	"context"
	"fmt"

	"projectmesh.io/mesh-cli/internal/channel"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
)

// channelWorkspace is where channel mode prepares its items: node.Git,
// whose AuthorizeOperator gives a checkout on the old platform's
// fallback the operator's gh, which the shared session's empty
// GH_CONFIG_DIR would otherwise hide from its push.
type channelWorkspace interface {
	node.Workspacer
	AuthorizeOperator(ctx context.Context, dir string) error
}

// channelItem is a claimed item made ready for the channel session: its
// work_item.ready event and the GitHub token its workspace runs on (nil
// on the old platform's fallback).
type channelItem struct {
	Res     *node.Resolved
	Content string
	Meta    map[string]string
	Cred    *node.ItemCred
}

// readyChannelItem does on the node's side what the shared session must
// not do for itself: resolve the item, ask for its GitHub token and put
// it on the workspace's own credential helper, then clone or fetch and
// check out the branch on it. Only then is there an event to push. Any
// error fails the item, as it would a poll-mode build.
func readyChannelItem(ctx context.Context, gw node.Gateway, ws channelWorkspace, scopes node.Scopes, item gateway.WorkItem, workRoot string, logf func(string, ...any)) (*channelItem, error) {
	res, err := node.Resolve(ctx, gw, item)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	spec := node.WorkspaceSpec{Root: workRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: res.Branch(), BaseRef: res.BaseRef()}
	cred, err := node.NewItemCred(ctx, gw, ws, scopes, item, spec, logf)
	if err != nil {
		return nil, err
	}
	dir, err := ws.Prepare(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	if cred == nil {
		if err := ws.AuthorizeOperator(ctx, dir); err != nil {
			return nil, fmt.Errorf("workspace: %w", err)
		}
	}
	content, meta := channel.ReadyEvent(res, dir)
	return &channelItem{Res: res, Content: content, Meta: meta, Cred: cred}, nil
}

// refreshItemCreds renews every held item's GitHub token that is near
// its expiry and rewrites its workspace's helper, so an item the session
// holds for longer than a token lives does not fail its push.
func refreshItemCreds(ctx context.Context, creds map[string]*node.ItemCred, logf func(string, ...any)) {
	for id, c := range creds {
		if err := c.Refresh(ctx); err != nil && ctx.Err() == nil {
			logf("item %s: %v", id, err)
		}
	}
}
