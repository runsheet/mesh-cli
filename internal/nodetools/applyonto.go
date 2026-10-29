package nodetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gitapply"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// ApplyOntoTool is the apply_onto node tool's name. It applies the
// brief's changes (code.apply) onto its base (code.onto) as
// internal/gitapply does — three-way merged without a worktree, each
// commit written under the brief's fixed identity (code.apply_as), so
// two nodes write the same shas (runsheet ADR 077, ADR 079) — fetching
// what it needs through the lease's relay, and leaves the checkout
// detached at the result. It answers {sha, tree} or {conflict: {index,
// paths}}, and records either.
const ApplyOntoTool = "apply_onto"

// applyTimeout bounds one apply, its fetches among it.
const applyTimeout = 10 * time.Minute

// ApplyOnto is one lease's apply_onto tool.
type ApplyOnto struct {
	Grant     Grant
	Repo      string
	Workspace string
	Code      *meshapi.Code
	Recorder  *Recorder
	// Escalate hears a conflict, its summary why (PR 2.11, trigger
	// "conflict"), and answers the model the session moves to, "" when
	// it does not; nil never escalates.
	Escalate func(why string) string
}

// Tool is apply_onto {}: everything it applies is the brief's.
func (a *ApplyOnto) Tool() mcptool.Tool {
	return mcptool.Tool{
		Name: ApplyOntoTool,
		Description: "Apply the job's changes onto the job's base commit, as the job's brief names them, and check the result out " +
			"(detached) in your checkout. Answers the commit and tree written, or the change that conflicts and its paths, " +
			"and the evidence id of the node's signed record of it.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Call:   a.call,
	}
}

func (a *ApplyOnto) call(ctx context.Context, _ map[string]any) (string, error) {
	c := a.Code
	if c == nil || c.Onto == nil || c.Onto.SHA == "" || len(c.Apply) == 0 {
		return "", errors.New("this job's brief names no changes to apply onto a base (code.onto and code.apply)")
	}
	if a.Grant == nil || a.Repo == "" {
		return "", errors.New("this job has no repository to fetch the changes from")
	}
	mode := gitapply.Linear
	switch c.ApplyMode {
	case "", "linear":
	case "merge":
		mode = gitapply.Merge
	default:
		return "", fmt.Errorf("the brief's apply_mode %q is not linear or merge", c.ApplyMode)
	}
	id := gitapply.Identity{Name: "Mesh", Email: "node@projectmesh.io"}
	if as := c.ApplyAs; as != nil {
		if as.Name != "" {
			id.Name = as.Name
		}
		if as.Email != "" {
			id.Email = as.Email
		}
		if as.Date != "" {
			when, err := time.Parse(time.RFC3339, as.Date)
			if err != nil {
				return "", fmt.Errorf("the brief's apply_as.date %q is not RFC 3339", as.Date)
			}
			id.When = when
		}
	}
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := a.fetch(ctx); err != nil {
		return "", err
	}
	changes := make([]gitapply.Change, len(c.Apply))
	for i, ch := range c.Apply {
		changes[i] = gitapply.Change{Head: ch.SHA, Base: ch.Base}
	}
	start := time.Now()
	g := gitapply.Git{Command: func(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
		return gitCmd(ctx, dir, env, args...)
	}}
	steps, conflict, err := g.Apply(ctx, a.Workspace, c.Onto.SHA, changes, mode, id)
	ended := time.Now()
	if err != nil {
		return "", fmt.Errorf("applying the changes: %v", err)
	}
	input := InputDigest(map[string]any{"onto": c.Onto, "apply": c.Apply, "apply_mode": mode.String(), "apply_as": id})
	rec := Record{Tool: ApplyOntoTool, StartedAt: start, EndedAt: ended, InputDigest: input}
	answer := map[string]any{}
	if conflict != nil {
		rec.Exit = 1
		rec.Summary = fmt.Sprintf("change %d (%s) conflicts with %s: %s", conflict.Index, short(conflict.Head), short(c.Onto.SHA), strings.Join(conflict.Paths, ", "))
		answer["conflict"] = map[string]any{"index": conflict.Index, "head": conflict.Head, "paths": conflict.Paths}
	} else {
		last := steps[len(steps)-1]
		if out, err := git(ctx, a.Workspace, "checkout", "-q", "--detach", last.Commit); err != nil {
			return "", fmt.Errorf("checking out %s: %s", short(last.Commit), lastLines(out, 5))
		}
		rec.Summary = fmt.Sprintf("applied %d changes onto %s (%s): %s, tree %s", len(steps), short(c.Onto.SHA), mode, last.Commit, last.Tree)
		answer["sha"], answer["tree"] = last.Commit, last.Tree
	}
	id2, recErr := a.Recorder.Record(context.WithoutCancel(ctx), rec, nil)
	if recErr != nil {
		answer["evidence_error"] = recErr.Error()
	} else {
		answer["evidence_id"] = id2
	}
	if conflict != nil && a.Escalate != nil {
		if to := a.Escalate("apply_onto conflicted: " + rec.Summary); to != "" {
			answer["escalated_to"] = to
		}
	}
	b, _ := json.Marshal(answer)
	return string(b), nil
}

// fetch brings the base and every change into the checkout through the
// relay: by their refs where the brief names them, then any commit still
// missing by its sha.
func (a *ApplyOnto) fetch(ctx context.Context) error {
	target := a.Grant.Base() + a.Repo + ".git"
	c := a.Code
	var refs []string
	want := []string{c.Onto.SHA}
	if c.Onto.Ref != "" {
		refs = append(refs, c.Onto.Ref)
	}
	for _, ch := range c.Apply {
		if ch.Ref != "" {
			refs = append(refs, ch.Ref)
		}
		want = append(want, ch.SHA)
		if ch.Base != "" {
			want = append(want, ch.Base)
		}
	}
	for _, r := range append(refs, want...) {
		if strings.HasPrefix(r, "-") {
			return fmt.Errorf("the brief names %q, not a ref or a commit", r)
		}
	}
	if len(refs) > 0 {
		if out, err := git(ctx, a.Workspace, append([]string{"fetch", "-q", "--no-tags", target}, refs...)...); err != nil {
			return fmt.Errorf("fetching %s through the relay: %s", strings.Join(refs, ", "), strings.ReplaceAll(lastLines(out, 5), target, "<relay>"))
		}
	}
	for _, sha := range want {
		if _, err := git(ctx, a.Workspace, "cat-file", "-e", sha+"^{commit}"); err == nil {
			continue
		}
		if out, err := git(ctx, a.Workspace, "fetch", "-q", "--no-tags", target, sha); err != nil {
			return fmt.Errorf("fetching %s through the relay: %s", short(sha), strings.ReplaceAll(lastLines(out, 5), target, "<relay>"))
		}
	}
	return nil
}
