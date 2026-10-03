package meshmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
)

// mesh_join (ADR-023, mesh_join): claim a goal's item for the person's
// personal node and make its workspace here, as a node's builder would
// have it — the checkout on the item's branch with the item's own token
// on its credential helper, and the project's context under
// .mesh/context — then answer where it is and what to build.

// JoinHarness is the harness a mesh_join claim names.
const JoinHarness = "claude-code"

// StateFile is where `mesh mcp` keeps the items it holds, under ~/.mesh.
const StateFile = "mcp-state.json"

// Join is what mesh_join needs beyond the reads: the gateway a node
// uses, the workspace, and the personal node the claim is made for.
type Join struct {
	GW node.Gateway
	// WS is the workspace: *node.Git on GitHub (GitHubURL its base).
	WS *node.Git
	// NodeID answers the personal node's id (item 2 registers it).
	NodeID func(ctx context.Context) (string, error)
	// WorkRoot is ~/.mesh/work when empty.
	WorkRoot string
	// StatePath is ~/.mesh/mcp-state.json when empty.
	StatePath string
	// Cwd is the server's working directory when empty: used in place of
	// the work root when it is a checkout of the item's repo.
	Cwd string
	// Fallback says once that the platform issues no item token.
	Fallback *node.TokenFallback
	// Hosted is the platform's /mcp: its mesh_claim and work tools when
	// it serves them, else the mutations, said once (ADR-023, Hosted
	// path). Nil is the mutations, nothing said.
	Hosted *Hosted
	// Logf is where the plumbing's lines go (never stdout).
	Logf func(string, ...any)
}

// JoinAnswer is mesh_join's answer.
type JoinAnswer struct {
	Path       string   `json:"path"`
	Branch     string   `json:"branch"`
	Brief      string   `json:"brief"`
	Acceptance string   `json:"acceptance"`
	Context    []string `json:"context"`
	Item       JoinItem `json:"item"`
	// Paired is a pair's join: the holder's branch, no claim.
	Paired bool `json:"paired,omitempty"`
}

// JoinItem names the joined item.
type JoinItem struct {
	ID    string `json:"id"`
	Goal  string `json:"goal"`
	Seq   int    `json:"seq"`
	Title string `json:"title"`
}

// HeldItem is one item `mesh mcp` holds, in mcp-state.json.
type HeldItem struct {
	ItemID string `json:"itemId"`
	Goal   string `json:"goal"`
	Seq    int    `json:"seq"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	// Base is the item's base ref: mesh_status counts commits past it.
	Base      string    `json:"base,omitempty"`
	ClaimedAt time.Time `json:"claimedAt"`
	// Paired is a join with pair:true on an item another node holds: the
	// holder's branch, no claim of this node's (ADR-023, Pairing).
	Paired bool `json:"paired,omitempty"`
}

// State is mcp-state.json: the items held, for mesh_status.
type State struct {
	Held []HeldItem `json:"held"`
}

func (j *Join) logf(format string, args ...any) {
	if j.Logf != nil {
		j.Logf(format, args...)
	}
}

func (j *Join) statePath() (string, error) {
	if j.StatePath != "" {
		return j.StatePath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", StateFile), nil
}

// ReadState reads mcp-state.json at path; none is an empty state.
func ReadState(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// hold records h in the state file, replacing an earlier record of the
// same item.
func (j *Join) hold(h HeldItem) error {
	path, err := j.statePath()
	if err != nil {
		return err
	}
	s, err := ReadState(path)
	if err != nil {
		return err
	}
	kept := s.Held[:0]
	for _, o := range s.Held {
		if o.ItemID != h.ItemID {
			kept = append(kept, o)
		}
	}
	s.Held = append(kept, h)
	return writeState(path, s)
}

// drop removes the item from the state file.
func (j *Join) drop(itemID string) error {
	path, err := j.statePath()
	if err != nil {
		return err
	}
	s, err := ReadState(path)
	if err != nil {
		return err
	}
	kept := s.Held[:0]
	for _, o := range s.Held {
		if o.ItemID != itemID {
			kept = append(kept, o)
		}
	}
	s.Held = kept
	return writeState(path, s)
}

// writeState writes s to path atomically.
func writeState(path string, s State) error {
	if s.Held == nil {
		s.Held = []HeldItem{}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// findGoal finds the goal by slug among the orgs the person reaches,
// with its org's name.
func (j *Join) findGoal(ctx context.Context, slug string) (*gateway.Goal, string, error) {
	orgs, err := j.GW.MyOrgs(ctx)
	if err != nil {
		return nil, "", err
	}
	for _, o := range orgs {
		goals, err := j.GW.Goals(ctx, o.ID)
		if err != nil {
			return nil, "", fmt.Errorf("goals of %s: %w", o.Name(), err)
		}
		for _, g := range goals {
			if g.Slug == slug {
				return &g, o.Name(), nil
			}
		}
	}
	return nil, "", fmt.Errorf("no goal %q in your orgs", slug)
}

// findItem is the goal's item at seq, or its first ready one but
// exclude's when seq is 0.
func (j *Join) findItem(ctx context.Context, g *gateway.Goal, seq int, exclude string) (*gateway.WorkItem, error) {
	items, err := j.GW.GoalWorkItems(ctx, g.ID)
	if err != nil {
		return nil, fmt.Errorf("items of %s: %w", g.Slug, err)
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Seq < items[b].Seq })
	for _, it := range items {
		if (seq != 0 && it.Seq == seq) || (seq == 0 && it.Status == "ready" && it.ID != exclude) {
			return &it, nil
		}
	}
	if seq != 0 {
		return nil, fmt.Errorf("goal %s has no item %d", g.Slug, seq)
	}
	return nil, fmt.Errorf("goal %s has no ready item", g.Slug)
}

// seqArg reads the item's seq: a number, or a string of one.
func seqArg(args map[string]any) (int, error) {
	switch v := args["item"].(type) {
	case nil:
		return 0, nil
	case float64:
		return int(v), nil
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("item: %q is not a seq", v)
		}
		return n, nil
	}
	return 0, fmt.Errorf("item: %v is not a seq", args["item"])
}

// call is the mesh_join tool.
func (j *Join) call(ctx context.Context, args map[string]any) (string, error) {
	slug := str(args, "goal")
	if slug == "" {
		return "", errors.New("goal: the goal's slug is required")
	}
	seq, err := seqArg(args)
	if err != nil {
		return "", err
	}
	pair, _ := args["pair"].(bool)
	return j.answer(j.run(ctx, slug, seq, "", pair))
}

// answer renders a join's result as the tool's answer.
func (j *Join) answer(ans *JoinAnswer, err error) (string, error) {
	if err != nil {
		var refused *ClaimRefused
		if errors.As(err, &refused) {
			return refused.Error(), nil
		}
		var pairRefused *PairRefused
		if errors.As(err, &pairRefused) {
			return pairRefused.Error(), nil
		}
		return "", err
	}
	b, err := json.MarshalIndent(ans, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ClaimRefused is the platform refusing the claim: the tool's answer,
// not a transport error.
type ClaimRefused struct{ Reason string }

func (e *ClaimRefused) Error() string { return "claim refused: " + e.Reason }

// PairRefused is the platform refusing a pair the item's token: the
// tool's answer, not a transport error.
type PairRefused struct{ Reason string }

func (e *PairRefused) Error() string { return "pair refused: " + e.Reason }

// heldStatuses are the statuses in which an item has a holder a pair
// can join.
var heldStatuses = map[string]bool{"claimed": true, "running": true, "in_progress": true, "evaluating": true, "review": true}

// heldByOther reports whether another node than nodeID holds the item.
func heldByOther(item gateway.WorkItem, nodeID string) bool {
	return heldStatuses[item.Status] && item.NodeID != "" && item.NodeID != nodeID
}

// Run claims the goal's item (seq, or its first ready one at 0) and
// prepares its workspace.
func (j *Join) Run(ctx context.Context, slug string, seq int) (*JoinAnswer, error) {
	return j.run(ctx, slug, seq, "", false)
}

// run is Run, passing over the item exclude when choosing the first
// ready one: mesh_switch never joins the item it just released. With
// pair, an item another node holds is joined on its holder's branch and
// not claimed (ADR-023, Pairing); one nobody holds is claimed as ever.
func (j *Join) run(ctx context.Context, slug string, seq int, exclude string, pair bool) (*JoinAnswer, error) {
	goal, orgName, err := j.findGoal(ctx, slug)
	if err != nil {
		return nil, err
	}
	item, err := j.findItem(ctx, goal, seq, exclude)
	if err != nil {
		return nil, err
	}
	nodeID, err := j.NodeID(ctx)
	if err != nil {
		return nil, fmt.Errorf("personal node: %w", err)
	}
	paired := pair && heldByOther(*item, nodeID)
	if err := j.claim(ctx, goal.Slug, *item, nodeID, paired); err != nil {
		return nil, err
	}
	ans, err := j.prepare(ctx, *item, nodeID, orgName, paired)
	if err != nil {
		if paired {
			// the claim is the holder's: nothing of ours to give back
			return nil, err
		}
		// the claim is given back: a workspace that could not be made
		// leaves nothing for the person to work in
		if _, rerr := j.release(ctx, item.ID, nodeID, "mesh_join: "+err.Error()); rerr != nil {
			j.logf("item %s: release: %v", item.ID, rerr)
		}
		return nil, err
	}
	return ans, nil
}

// claim claims the item for nodeID: hosted mesh_claim when the
// platform serves it — a pair's join too, pair true, so the platform
// records the pair — else claimWorkItem, and a pair's join nothing.
func (j *Join) claim(ctx context.Context, slug string, item gateway.WorkItem, nodeID string, paired bool) error {
	if paired {
		j.logf("item %s: pairing with node %s on its branch; no claim of ours", item.ID, item.NodeID)
	}
	if j.Hosted.Use(ctx) {
		_, err := j.Hosted.Call(ctx, "mesh_claim", ClaimArgs(slug, item.Seq, paired, nodeID))
		var he *HostedError
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.As(err, &he) && paired:
			return &PairRefused{Reason: he.Text}
		case errors.As(err, &he):
			return &ClaimRefused{Reason: he.Text}
		}
		return fmt.Errorf("mesh_claim: %w", err)
	}
	if paired {
		return nil
	}
	if err := j.GW.ClaimWorkItem(ctx, item.ID, nodeID, JoinHarness); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &ClaimRefused{Reason: err.Error()}
	}
	return nil
}

func (j *Join) prepare(ctx context.Context, item gateway.WorkItem, nodeID, orgName string, paired bool) (*JoinAnswer, error) {
	res, err := node.Resolve(ctx, j.GW, item)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	root := j.WorkRoot
	if root == "" {
		if root, err = node.WorkRoot(); err != nil {
			return nil, err
		}
	}
	repo := res.Repo.FullName()
	spec := node.WorkspaceSpec{Root: root, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: repo, Branch: node.BranchName(res.Goal.Slug, item.Seq), BaseRef: res.BaseRef()}
	var ws node.Workspacer = j.WS
	if top := j.cwdCheckout(ctx, repo); top != "" {
		spec.Path = top
		ws = cwdWorkspace{j.WS}
	}
	scopes := node.Scopes{{ID: item.OrgID, NodeID: nodeID}}
	cred, err := node.NewItemCred(ctx, j.GW, ws, scopes, item, spec, j.Fallback, j.logf)
	if err != nil {
		if paired && ctx.Err() == nil {
			return nil, &PairRefused{Reason: err.Error()}
		}
		return nil, err
	}
	dir, err := node.PrepareWithRetry(ctx, ws, spec, cred, j.logf)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	cx, err := node.FetchContext(ctx, j.GW, orgName, res.Project.Slug, node.ContextOptions{Repos: []string{repo}, Checkouts: map[string]string{repo: dir}, Goal: res.Goal, Logf: j.logf})
	if err != nil {
		j.logf("item %s: context unavailable: %v", item.ID, err)
		cx = &node.Context{}
	}
	names, written, err := node.WriteContext(dir, cx)
	if err != nil {
		return nil, fmt.Errorf("write context: %w", err)
	}
	j.logf("item %s: %s", item.ID, written.Line())
	files := []string{}
	for _, n := range names {
		files = append(files, filepath.Join(dir, node.ContextDir, n))
	}
	if index := filepath.Join(dir, node.ContextDir, node.IndexFile); fileExists(index) {
		files = append(files, index)
	}
	if err := j.hold(HeldItem{ItemID: item.ID, Goal: res.Goal.Slug, Seq: item.Seq, Path: dir, Branch: spec.Branch, Base: spec.BaseRef, ClaimedAt: time.Now().UTC(), Paired: paired}); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	return &JoinAnswer{Path: dir, Branch: spec.Branch, Brief: item.Brief, Acceptance: item.Acceptance, Context: files,
		Item: JoinItem{ID: item.ID, Goal: res.Goal.Slug, Seq: item.Seq, Title: item.Title}, Paired: paired}, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// cwdCheckout is the top of the server's working directory when it is a
// git checkout whose origin names repo (owner/name), else "".
func (j *Join) cwdCheckout(ctx context.Context, repo string) string {
	cwd := j.Cwd
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return ""
		}
	}
	gitOut := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	top := gitOut("rev-parse", "--show-toplevel")
	if top == "" || !OriginNames(gitOut("remote", "get-url", "origin"), repo) {
		return ""
	}
	return top
}

// OriginNames reports whether a remote URL — https, ssh (git@host:o/n)
// or a path — ends in the repo's owner/name, .git or not, any case.
func OriginNames(url, repo string) bool {
	u := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(url), "/"), ".git")
	if u == "" || repo == "" {
		return false
	}
	u = strings.ReplaceAll(u, ":", "/")
	u = strings.ToLower(u)
	want := strings.ToLower(repo)
	return u == want || strings.HasSuffix(u, "/"+want)
}

// cwdWorkspace is the person's own checkout made the item's workspace:
// the item token goes on its own credential helper, but its origin URL
// is left as the person set it.
type cwdWorkspace struct{ *node.Git }

func (w cwdWorkspace) Authorize(ctx context.Context, spec node.WorkspaceSpec, tok *gateway.GitHubToken) error {
	base := w.GitHubURL
	if base == "" {
		base = node.GitHubURL
	}
	return node.WriteCredentialHelper(ctx, spec.Dir(), base, tok)
}
