package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// DeadCheckoutIdle is how long a dead checkout must have gone untouched
// before the quartermaster removes it: a checkout written within the
// hour may be one a session is still setting up.
const DeadCheckoutIdle = time.Hour

// deadItemStatuses are the item statuses whose checkouts no session will
// use again (goal the-quartermaster-keeps-the-nodes-fit, item 3).
var deadItemStatuses = map[string]bool{
	"merged": true, "done": true, "already_done": true, "cancelled": true, "superseded": true,
}

// itemCheckout is one item workspace under the work root: a build's
// <root>/<goal>/<seq> or an evaluation's <root>/<goal>/<seq>-eval.
type itemCheckout struct {
	Dir      string
	GoalSlug string
	Seq      int
	Label    string // <goal>/<seq> or <goal>/<seq>-eval; <job label>/checkout
	// ItemID, set, is the item of a job's checkout (ADR-039), its status
	// read by id.
	ItemID string
}

// DeadCheckoutCheck is the quartermaster's dead-checkout check: every
// item workspace under root (WorkspaceSpec.Dir's <goal>/<seq> and
// EvalWorkspaceSpec's <goal>/<seq>-eval) whose item the platform says is
// merged, done, cancelled or superseded — or does not know — with no
// session using it (live) and untouched for DeadCheckoutIdle is removed
// by git.RemoveWorkspace, its bytes counted. A checkout the platform
// cannot answer for is kept: an outage is not a leak (mesh-cli ADR 008).
// Every shared clone's prunable worktrees — registered, their directory
// gone — are counted and pruned. It yields one `dead_checkout` Finding,
// warn when a dead checkout could not be removed or the platform could
// not answer.
func DeadCheckoutCheck(gw Gateway, git *Git, root string, live func(dir string) bool) Check {
	return func(ctx context.Context, now time.Time) []Finding {
		reposRoot := git.ReposRoot
		if reposRoot == "" {
			reposRoot = filepath.Join(filepath.Dir(root), "repos")
		}
		prune := func(ctx context.Context) (int, []string) {
			return pruneGoneWorktrees(ctx, git, reposRoot, git.lockRoot(root, ""))
		}
		return []Finding{deadCheckouts(ctx, gw, git, root, itemCheckouts(root), prune, live, now)}
	}
}

// JobDeadCheckoutCheck is DeadCheckoutCheck over the node's jobs
// (ADR-039): every item job's checkout, evaluation clone and rebase
// clone, its item's status read by id.
func JobDeadCheckoutCheck(gw Gateway, git *Git, l *Layout, live func(dir string) bool) Check {
	return func(ctx context.Context, now time.Time) []Finding {
		// every job's checkout is a clone of its own: no shared clone
		// holds a registration to prune
		prune := func(context.Context) (int, []string) { return 0, nil }
		return []Finding{deadCheckouts(ctx, gw, git, l.JobsDir(), jobCheckouts(l), prune, live, now)}
	}
}

// jobCheckouts is every item job's workspace with a `.git`: its
// checkout, eval and rebase, sorted by path.
func jobCheckouts(l *Layout) []itemCheckout {
	jobs, _ := l.Jobs()
	var out []itemCheckout
	for _, fj := range jobs {
		if fj.Meta.Unit != JobItem {
			continue
		}
		j, err := l.Job(fj.Meta.ID)
		if err != nil {
			continue
		}
		for _, dir := range []string{j.Checkout(), j.Eval(), j.Rebase()} {
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
				continue
			}
			out = append(out, itemCheckout{Dir: dir, ItemID: fj.Meta.ItemID, Label: cmpLabel(fj.Meta) + "/" + filepath.Base(dir)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

func deadCheckouts(ctx context.Context, gw Gateway, git *Git, root string, checkouts []itemCheckout, prune func(context.Context) (int, []string), live func(string) bool, now time.Time) Finding {
	f := Finding{Kind: "dead_checkout", Severity: "ok", Subject: root, At: now}
	var notes []string

	pruned, pruneErrs := prune(ctx)
	notes = append(notes, pruneErrs...)

	items := &checkoutItems{gw: gw}
	var dead, removed, keptLive, keptRecent, unread int
	var unreadErr error
	var failed []string
	for _, c := range checkouts {
		status, err := items.statusOf(ctx, c)
		if err != nil {
			unread++
			if unreadErr == nil {
				unreadErr = err
			}
			continue
		}
		if status != "unknown" && !deadItemStatuses[status] {
			continue
		}
		dead++
		if live != nil && live(c.Dir) {
			keptLive++
			continue
		}
		if now.Sub(lastTouched(c.Dir)) < DeadCheckoutIdle {
			keptRecent++
			continue
		}
		size := dirBytes(c.Dir)
		f.Bytes += size
		if err := git.RemoveWorkspace(ctx, c.Dir); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", c.Label, err))
			continue
		}
		removed++
		f.Reclaimed += size
	}

	f.Detail = fmt.Sprintf("%d dead found, %d removed, %d bytes reclaimed, %d kept because live", dead, removed, f.Reclaimed, keptLive)
	if keptRecent > 0 {
		f.Detail += fmt.Sprintf(", %d kept as touched within %s", keptRecent, DeadCheckoutIdle)
	}
	f.Detail += fmt.Sprintf("; %d prunable worktrees pruned", pruned)
	if unread > 0 {
		f.Severity = "warn"
		f.Detail += fmt.Sprintf("; the platform could not answer for %d checkouts, kept: %v", unread, unreadErr)
	}
	if len(failed) > 0 {
		f.Severity = "warn"
		f.Detail += "; could not remove " + strings.Join(failed, "; ")
	}
	if len(notes) > 0 {
		f.Severity = "warn"
		f.Detail += "; " + strings.Join(notes, "; ")
	}
	return f
}

// itemCheckouts is every item workspace under root: <goal>/<seq> and
// <goal>/<seq>-eval with a `.git`, seq at least 1 (the goal doc's
// <goal>/0, plan and research checkouts and the node's own dirs are not
// items'), sorted by path.
func itemCheckouts(root string) []itemCheckout {
	var out []itemCheckout
	goals, _ := os.ReadDir(root)
	for _, g := range goals {
		if !g.IsDir() || strings.HasPrefix(g.Name(), ".") {
			continue
		}
		subs, _ := os.ReadDir(filepath.Join(root, g.Name()))
		for _, s := range subs {
			if !s.IsDir() {
				continue
			}
			seq, ok := checkoutSeq(s.Name())
			if !ok {
				continue
			}
			dir := filepath.Join(root, g.Name(), s.Name())
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
				continue
			}
			out = append(out, itemCheckout{Dir: dir, GoalSlug: g.Name(), Seq: seq, Label: g.Name() + "/" + s.Name()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// checkoutSeq reads an item workspace's directory name, <seq> or
// <seq>-eval, as its seq; ok false for anything else or a seq below 1.
func checkoutSeq(name string) (int, bool) {
	seq, err := strconv.Atoi(strings.TrimSuffix(name, "-eval"))
	if err != nil || seq < 1 || strconv.Itoa(seq) != strings.TrimSuffix(name, "-eval") {
		return 0, false
	}
	return seq, true
}

// checkoutItems answers an item's status by goal slug and seq, reading
// each served org's goals once and each goal's items once.
type checkoutItems struct {
	gw    Gateway
	read  bool
	err   error
	goals map[string]string // slug → goal id
	items map[string]map[int]string
	ierr  map[string]error
}

// statusOf is a checkout's item's status: by id for a job's checkout,
// else by goal slug and seq (status).
func (c *checkoutItems) statusOf(ctx context.Context, co itemCheckout) (string, error) {
	if co.ItemID == "" {
		return c.status(ctx, co.GoalSlug, co.Seq)
	}
	it, err := c.gw.WorkItem(ctx, co.ItemID)
	if errors.Is(err, gateway.ErrNotFound) || (err == nil && it == nil) {
		return "unknown", nil
	}
	if err != nil {
		return "", fmt.Errorf("item %s: %w", co.ItemID, err)
	}
	return itemStatus(*it), nil
}

// status is the item's status, "unknown" when the platform has no goal
// of that slug in any served org or no item of that seq in it; an error
// when the platform could not answer.
func (c *checkoutItems) status(ctx context.Context, slug string, seq int) (string, error) {
	if !c.read {
		c.read = true
		c.goals = map[string]string{}
		orgs, err := c.gw.MyOrgs(ctx)
		if err != nil {
			c.err = fmt.Errorf("orgs: %w", err)
		}
		for _, org := range orgs {
			goals, err := c.gw.Goals(ctx, org.ID)
			if err != nil {
				c.err = fmt.Errorf("goals of %s: %w", org.ID, err)
				break
			}
			for _, g := range goals {
				c.goals[g.Slug] = g.ID
			}
		}
	}
	if c.err != nil {
		return "", c.err
	}
	goalID, ok := c.goals[slug]
	if !ok {
		return "unknown", nil
	}
	if c.items == nil {
		c.items, c.ierr = map[string]map[int]string{}, map[string]error{}
	}
	if err, ok := c.ierr[goalID]; ok {
		return "", err
	}
	rows, ok := c.items[goalID]
	if !ok {
		list, err := c.gw.GoalWorkItems(ctx, goalID)
		if errors.Is(err, gateway.ErrNotFound) {
			list, err = nil, nil
		}
		if err != nil {
			c.ierr[goalID] = fmt.Errorf("items of %s: %w", slug, err)
			return "", c.ierr[goalID]
		}
		rows = map[int]string{}
		for _, it := range list {
			rows[it.Seq] = itemStatus(it)
		}
		c.items[goalID] = rows
	}
	status, ok := rows[seq]
	if !ok {
		return "unknown", nil
	}
	return status, nil
}

// lastTouched is the latest mtime among dir, its entries, its session
// dir <dir>.session and its git admin files (index, HEAD, logs/HEAD) —
// what a session writes as it works — without walking the whole tree.
func lastTouched(dir string) time.Time {
	var latest time.Time
	note := func(p string) {
		if fi, err := os.Lstat(p); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	note(dir)
	note(dir + ".session")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		note(filepath.Join(dir, e.Name()))
	}
	gitdir := filepath.Join(dir, ".git")
	if b, err := os.ReadFile(gitdir); err == nil {
		if p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:"); ok {
			gitdir = strings.TrimSpace(p)
			if !filepath.IsAbs(gitdir) {
				gitdir = filepath.Join(dir, gitdir)
			}
		}
	}
	for _, name := range []string{"index", "HEAD", filepath.Join("logs", "HEAD")} {
		note(filepath.Join(gitdir, name))
	}
	return latest
}

// dirBytes is the size of every regular file under dir.
func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// pruneGoneWorktrees forgets each shared clone's registrations whose
// directory is gone and that are neither locked (lockWorktree) nor live
// on this node (WorkspaceLive) — never a blanket `git worktree prune`
// (ADR-028, What RemoveWorkspace removes) — under the repo's lock; it
// returns the count and a note per clone it could not clean.
func pruneGoneWorktrees(ctx context.Context, git *Git, reposRoot, lockRoot string) (int, []string) {
	var n int
	var notes []string
	for _, clone := range sharedClones(reposRoot) {
		admins := goneRegistrations(clone)
		if len(admins) == 0 {
			continue
		}
		if err := git.forgetRegistrations(ctx, lockRoot, clone, admins); err != nil {
			notes = append(notes, fmt.Sprintf("worktree prune %s: %v", clone, err))
			continue
		}
		n += len(admins)
	}
	return n, notes
}

// goneRegistrations is clone's .git/worktrees/<n> whose worktree
// directory is gone, unlocked and not live on this node.
func goneRegistrations(clone string) []string {
	worktrees := filepath.Join(clone, ".git", "worktrees")
	regs, err := os.ReadDir(worktrees)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range regs {
		admin := filepath.Join(worktrees, r.Name())
		wt, ok := registeredDir(admin)
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil || WorkspaceLive(wt) {
			continue
		}
		if _, err := os.Stat(filepath.Join(admin, "locked")); err == nil {
			continue
		}
		out = append(out, admin)
	}
	return out
}

// sharedClones is every clone under reposRoot — <owner>/<name>, or one
// directory for a repo named by path — that has a `.git` directory.
func sharedClones(reposRoot string) []string {
	isClone := func(p string) bool {
		fi, err := os.Stat(filepath.Join(p, ".git"))
		return err == nil && fi.IsDir()
	}
	var out []string
	tops, _ := os.ReadDir(reposRoot)
	for _, top := range tops {
		if !top.IsDir() || strings.HasPrefix(top.Name(), ".") {
			continue
		}
		p := filepath.Join(reposRoot, top.Name())
		if isClone(p) {
			out = append(out, p)
			continue
		}
		subs, _ := os.ReadDir(p)
		for _, s := range subs {
			if s.IsDir() && isClone(filepath.Join(p, s.Name())) {
				out = append(out, filepath.Join(p, s.Name()))
			}
		}
	}
	return out
}
