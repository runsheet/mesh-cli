package meshmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"projectmesh.io/mesh-cli/internal/node"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The work tools (ADR-023, Work tools): mesh_status, mesh_switch and the
// pass-through mesh_progress, mesh_ask, mesh_finish and mesh_release on
// the items mcp-state.json holds. Each names its item by `item` — the
// item's id, its seq, or goal/seq — which defaults to the one held item
// and is required when several are held. A refusal from the gateway is
// the tool's answer, `<tool> refused: <reason>`, never a transport
// error.

// AskPrefix opens the progress note mesh_ask reports when the platform
// serves no hosted mesh_ask (ADR-023, Hosted path).
const AskPrefix = "question: "

// SwitchReason is the release reason mesh_switch gives the item it
// leaves.
const SwitchReason = "switched"

// pushRefused is a finish's or switch's answer to a push git refused
// because the branch moved on origin (node.PushRefusedError) — a pair's
// holder pushed first: the item stays held, nothing is sent.
func pushRefused(h HeldItem, again string) string {
	return "push refused: the branch moved on origin; run git pull --rebase in " + h.Path + " and " + again + " again"
}

// refused is the tool's answer to a refusal.
func refused(tool string, err error) string {
	return tool + " refused: " + err.Error()
}

// HeldStatus is one held item as mesh_status answers it.
type HeldStatus struct {
	HeldItem
	// Uncommitted is whether `git status --porcelain` lists anything
	// (the context under .mesh is excluded from git).
	Uncommitted bool `json:"uncommitted"`
	// CommitsPastBase is `git rev-list --count origin/<base>..HEAD`.
	CommitsPastBase int `json:"commitsPastBase"`
	// Error is why the checkout could not be read, if it could not.
	Error string `json:"error,omitempty"`
}

func baseOf(h HeldItem) string {
	if h.Base != "" {
		return h.Base
	}
	return "main"
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// commitsPastBase counts the checkout's commits past origin/<base>.
func commitsPastBase(ctx context.Context, h HeldItem) (int, error) {
	out, err := gitOutput(ctx, h.Path, "rev-list", "--count", "refs/remotes/origin/"+baseOf(h)+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// checkout reads the held item's checkout: uncommitted changes and
// commits past base.
func checkout(ctx context.Context, h HeldItem) HeldStatus {
	st := HeldStatus{HeldItem: h}
	porcelain, err := gitOutput(ctx, h.Path, "status", "--porcelain")
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Uncommitted = porcelain != ""
	n, err := commitsPastBase(ctx, h)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.CommitsPastBase = n
	return st
}

// held reads the state file's held items.
func (p *Personal) held() ([]HeldItem, error) {
	path, err := p.Join.statePath()
	if err != nil {
		return nil, err
	}
	s, err := ReadState(path)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	return s.Held, nil
}

// pick is the held item args[key] names — its id, seq or goal/seq — or
// the one held item when it names none.
func (p *Personal) pick(args map[string]any, key string) (HeldItem, error) {
	held, err := p.held()
	if err != nil {
		return HeldItem{}, err
	}
	var want string
	switch v := args[key].(type) {
	case string:
		want = strings.TrimSpace(v)
	case float64:
		want = strconv.Itoa(int(v))
	}
	if want == "" {
		switch len(held) {
		case 0:
			return HeldItem{}, errors.New(holdingNone)
		case 1:
			return held[0], nil
		}
		var names []string
		for _, h := range held {
			names = append(names, fmt.Sprintf("%s/%d", h.Goal, h.Seq))
		}
		return HeldItem{}, fmt.Errorf("%s: several items are held (%s); name one", key, strings.Join(names, ", "))
	}
	var found []HeldItem
	for _, h := range held {
		if h.ItemID == want || strconv.Itoa(h.Seq) == want || fmt.Sprintf("%s/%d", h.Goal, h.Seq) == want {
			found = append(found, h)
		}
	}
	switch len(found) {
	case 0:
		return HeldItem{}, fmt.Errorf("%s: no held item %q", key, want)
	case 1:
		return found[0], nil
	}
	return HeldItem{}, fmt.Errorf("%s: %q names several held items; give goal/seq", key, want)
}

// nodeID is the personal node the held items are claimed for.
func (p *Personal) nodeID(ctx context.Context) (string, error) {
	id, err := p.Join.NodeID(ctx)
	if err != nil {
		return "", fmt.Errorf("personal node: %w", err)
	}
	return id, nil
}

// work runs a work tool under the server's lock, or answers `not yet
// available` with no Join.
func (p *Personal) work(call func(context.Context, map[string]any) (string, error)) func(context.Context, map[string]any) (string, error) {
	return func(ctx context.Context, args map[string]any) (string, error) {
		if p.Join == nil {
			return NotYetAvailable, nil
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return call(ctx, args)
	}
}

// statusTool is mesh_status: holding nothing with no Join.
func (p *Personal) statusTool(ctx context.Context, args map[string]any) (string, error) {
	if p.Join == nil {
		return holdingNone, nil
	}
	return p.work(p.status)(ctx, args)
}

const holdingNone = "holding no item: mesh_join takes one"

// status is mesh_status: each held item with its checkout's state.
func (p *Personal) status(ctx context.Context, _ map[string]any) (string, error) {
	held, err := p.held()
	if err != nil {
		return "", err
	}
	if len(held) == 0 {
		return holdingNone, nil
	}
	out := struct {
		Held []HeldStatus `json:"held"`
	}{}
	for _, h := range held {
		out.Held = append(out.Held, checkout(ctx, h))
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// progress is mesh_progress: a note on the held item.
func (p *Personal) progress(ctx context.Context, args map[string]any) (string, error) {
	h, err := p.pick(args, "item")
	if err != nil {
		return "", err
	}
	note := str(args, "note")
	if note == "" {
		return "", errors.New("note: a note is required")
	}
	if p.Join.Hosted.Use(ctx) {
		return p.Join.hosted(ctx, "mesh_progress", ProgressArgs(h.ItemID, note))
	}
	cents, _ := args["costCents"].(float64)
	if err := p.Join.GW.ReportWorkItemProgress(ctx, h.ItemID, note, int(cents), ""); err != nil {
		return refused("mesh_progress", err), nil
	}
	return fmt.Sprintf("noted on %s/%d", h.Goal, h.Seq), nil
}

// ask is mesh_ask: the hosted mesh_ask when the platform serves it, else
// the question as a progress note opening `question: `.
func (p *Personal) ask(ctx context.Context, args map[string]any) (string, error) {
	h, err := p.pick(args, "item")
	if err != nil {
		return "", err
	}
	q := str(args, "question")
	if q == "" {
		return "", errors.New("question: a question is required")
	}
	if p.Join.Hosted.Use(ctx) {
		return p.Join.hosted(ctx, "mesh_ask", AskArgs(h.ItemID, q))
	}
	if err := p.Join.GW.ReportWorkItemProgress(ctx, h.ItemID, AskPrefix+q, 0, ""); err != nil {
		return refused("mesh_ask", err), nil
	}
	return fmt.Sprintf("asked on %s/%d; the answer comes back on the item", h.Goal, h.Seq), nil
}

// finish is mesh_finish: push the branch as committed — nothing is
// committed here — and record it; already_done records the evidence
// with no push. The item leaves the state file.
func (p *Personal) finish(ctx context.Context, args map[string]any) (string, error) {
	h, err := p.pick(args, "item")
	if err != nil {
		return "", err
	}
	// the finish is the build job's, handed back by the personal node
	// (finishJob names it, runsheet ADR 061)
	if id, err := p.nodeID(ctx); err == nil {
		ctx = gateway.WithNodeID(ctx, id)
	}
	hosted := p.Join.Hosted.Use(ctx)
	if str(args, "outcome") == "already_done" {
		ans := fmt.Sprintf("finished %s/%d already_done", h.Goal, h.Seq)
		if hosted {
			text, err := p.Join.Hosted.Call(ctx, "mesh_finish", FinishDoneArgs(h.ItemID, str(args, "evidence")))
			if err != nil {
				return hostedRefused("mesh_finish", err)
			}
			ans = text
		} else if err := p.Join.GW.FinishWorkItemAlreadyDone(ctx, h.ItemID, str(args, "evidence"), 0, nil); err != nil {
			return refused("mesh_finish", err), nil
		}
		if err := p.Join.drop(h.ItemID); err != nil {
			return "", fmt.Errorf("state: %w", err)
		}
		return ans, nil
	}
	st := checkout(ctx, h)
	head, err := p.Join.WS.Push(ctx, h.Path, h.Branch)
	var moved *node.PushRefusedError
	if errors.As(err, &moved) {
		return pushRefused(h, "finish"), nil
	}
	if err != nil {
		return refused("mesh_finish", fmt.Errorf("push %s: %w", h.Branch, err)), nil
	}
	ans := fmt.Sprintf("finished %s/%d: pushed %s at %s", h.Goal, h.Seq, h.Branch, head)
	if hosted {
		text, err := p.Join.Hosted.Call(ctx, "mesh_finish", FinishArgs(h.ItemID, h.Branch, head))
		if err != nil {
			return hostedRefused("mesh_finish", err)
		}
		ans = fmt.Sprintf("pushed %s at %s; %s", h.Branch, head, text)
	} else if err := p.Join.GW.FinishWorkItem(ctx, h.ItemID, h.Branch, head, 0, nil); err != nil {
		return refused("mesh_finish", err), nil
	}
	if err := p.Join.drop(h.ItemID); err != nil {
		return "", fmt.Errorf("state: %w", err)
	}
	if st.Uncommitted {
		ans += "; uncommitted changes in " + h.Path + " were not pushed"
	}
	return ans, nil
}

// release is mesh_release: give the held item back with the reason.
func (p *Personal) release(ctx context.Context, args map[string]any) (string, error) {
	h, err := p.pick(args, "item")
	if err != nil {
		return "", err
	}
	reason := str(args, "reason")
	if reason == "" {
		return "", errors.New("reason: a reason is required")
	}
	text, ans, err := p.giveBack(ctx, "mesh_release", h, reason)
	if ans != "" || err != nil {
		return ans, err
	}
	if text != "" {
		return text, nil
	}
	if h.Paired {
		return fmt.Sprintf("left %s/%d: %s (paired: its holder keeps the claim)", h.Goal, h.Seq, reason), nil
	}
	return fmt.Sprintf("released %s/%d: %s", h.Goal, h.Seq, reason), nil
}

// giveBack releases h and drops it from the state file: text is the
// hosted mesh_release's, a refusal is the answer, "" when released. A
// paired item is only dropped: its claim is the holder's, not ours to
// give back.
func (p *Personal) giveBack(ctx context.Context, tool string, h HeldItem, reason string) (text, ans string, err error) {
	if h.Paired {
		if err := p.Join.drop(h.ItemID); err != nil {
			return "", "", fmt.Errorf("state: %w", err)
		}
		return "", "", nil
	}
	nodeID, err := p.nodeID(ctx)
	if err != nil {
		return "", "", err
	}
	text, err = p.Join.release(ctx, h.ItemID, nodeID, reason)
	var he *HostedError
	switch {
	case errors.As(err, &he):
		return "", tool + " refused: " + he.Text, nil
	case err != nil:
		return "", refused(tool, err), nil
	}
	if err := p.Join.drop(h.ItemID); err != nil {
		return "", "", fmt.Errorf("state: %w", err)
	}
	return text, "", nil
}

// release gives the item back: hosted mesh_release, answering its text,
// when the platform serves it, else releaseWorkItem.
func (j *Join) release(ctx context.Context, itemID, nodeID, reason string) (string, error) {
	if j.Hosted.Use(ctx) {
		return j.Hosted.Call(ctx, "mesh_release", ReleaseArgs(itemID, reason))
	}
	return "", j.GW.ReleaseWorkItem(ctx, itemID, nodeID, reason)
}

// note reports a progress note: hosted mesh_progress when the platform
// serves it, else reportWorkItemProgress.
func (j *Join) note(ctx context.Context, itemID, note string) error {
	if j.Hosted.Use(ctx) {
		_, err := j.Hosted.Call(ctx, "mesh_progress", ProgressArgs(itemID, note))
		return err
	}
	return j.GW.ReportWorkItemProgress(ctx, itemID, note, 0, "")
}

// hosted calls a hosted work tool and relays its text; its isError is
// the answer `<tool> refused: <text>`.
func (j *Join) hosted(ctx context.Context, tool string, args map[string]any) (string, error) {
	text, err := j.Hosted.Call(ctx, tool, args)
	if err != nil {
		return hostedRefused(tool, err)
	}
	return text, nil
}

// hostedRefused is a hosted call's failure as the tool's answer: the
// hosted tool's own text when it answered isError, else the error.
func hostedRefused(tool string, err error) (string, error) {
	var he *HostedError
	if errors.As(err, &he) {
		return tool + " refused: " + he.Text, nil
	}
	return refused(tool, err), nil
}

// switchItem is mesh_switch: push the held item's branch when it has
// commits past base, note why, release it `switched`, and join the
// target, answering the join.
func (p *Personal) switchItem(ctx context.Context, args map[string]any) (string, error) {
	slug := str(args, "goal")
	if slug == "" {
		return "", errors.New("goal: the goal's slug is required")
	}
	seq, err := seqArg(args)
	if err != nil {
		return "", err
	}
	h, err := p.pick(args, "from")
	if err != nil {
		return "", err
	}
	n, err := commitsPastBase(ctx, h)
	if err != nil {
		return "", fmt.Errorf("%s: %w", h.Path, err)
	}
	if n > 0 {
		_, err := p.Join.WS.Push(ctx, h.Path, h.Branch)
		var moved *node.PushRefusedError
		if errors.As(err, &moved) {
			return pushRefused(h, "switch"), nil
		}
		if err != nil {
			return refused("mesh_switch", fmt.Errorf("push %s: %w", h.Branch, err)), nil
		}
	}
	note := str(args, "note")
	if note == "" {
		note = "switching to " + slug
		if seq != 0 {
			note += "/" + strconv.Itoa(seq)
		}
	}
	if n > 0 {
		note += fmt.Sprintf(" (%s pushed, %d commit(s) past %s)", h.Branch, n, baseOf(h))
	}
	if err := p.Join.note(ctx, h.ItemID, note); err != nil {
		return hostedRefused("mesh_switch", err)
	}
	if _, ans, err := p.giveBack(ctx, "mesh_switch", h, SwitchReason); ans != "" || err != nil {
		return ans, err
	}
	pair, _ := args["pair"].(bool)
	return p.Join.answer(p.Join.run(ctx, slug, seq, h.ItemID, pair))
}
