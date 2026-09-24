package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The goal's record in the repo: `specs/goal-<slug>.md`, flat under
// `specs/` so Prologue's own layout indexes it on the push that merges
// the item. Prologue is read-only over git — writing to it is committing
// this file. Frontmatter names the goal; the body carries the statement,
// the acceptance, the items table (regenerated from the gateway's rows
// every time) and a log that only ever appends.

// GoalDoc is what the document is rendered from.
type GoalDoc struct {
	Slug       string
	Title      string
	Statement  string
	Acceptance string
	// GoalStatus is the goal's status on the platform; it maps to the
	// document's status (draft while running, approved when done,
	// superseded when cancelled).
	GoalStatus string
	// Parent is the project's PRD slug when Prologue lists one.
	Parent string
	// SpentCents is the goal's total spend as the platform has it —
	// the budget guard's own number, not a sum over Items.
	SpentCents int
	Items      []gateway.WorkItem
}

// GoalDocPath is where the document lives in a workspace.
func GoalDocPath(workspace, slug string) string {
	return filepath.Join(workspace, "specs", "goal-"+slug+".md")
}

// DocStatus maps a goal status to the document's.
func DocStatus(goalStatus string) string {
	switch goalStatus {
	case "done":
		return "approved"
	case "cancelled":
		return "superseded"
	default:
		return "draft"
	}
}

// Summary is the document's one-line summary — AGENTS.md asks every
// document for one. A goal that has ended says how it ended (the items
// merged and what it cost); one still running says what it is for.
func Summary(d GoalDoc) string {
	switch d.GoalStatus {
	case "done", "cancelled":
		merged := 0
		for _, it := range d.Items {
			if it.Status == "merged" {
				merged++
			}
		}
		return fmt.Sprintf("%d of %d items merged · $%.2f spent", merged, len(d.Items), float64(d.SpentCents)/100)
	}
	if s := firstLine(d.Statement); s != "" {
		return s
	}
	if s := firstLine(d.Title); s != "" {
		return s
	}
	// the line is never empty: a goal with neither is still this goal.
	return "Mesh goal " + strings.TrimSpace(d.Slug)
}

// firstLine is the first non-blank line of s, its runs of whitespace
// collapsed to single spaces.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			return l
		}
	}
	return ""
}

// LogLine renders one log entry: `- 2026-09-19 · item 2 · <summary>`.
func LogLine(at time.Time, seq int, summary string) string {
	summary = strings.Join(strings.Fields(summary), " ")
	return fmt.Sprintf("- %s · item %d · %s", at.UTC().Format("2006-01-02"), seq, summary)
}

const logHeading = "## Log"

// RenderGoalDoc renders the document, keeping the log lines and the
// sections it does not own of existing (the file as it is now, or "")
// and appending logLine unless the log already ends with that exact
// line.
func RenderGoalDoc(existing string, d GoalDoc, logLine string) string {
	logLines, kept := existingSections(existing)
	logLine = strings.TrimSpace(logLine)
	if logLine != "" && (len(logLines) == 0 || logLines[len(logLines)-1] != logLine) {
		logLines = append(logLines, logLine)
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: Goal — %s\n", strings.TrimSpace(d.Title))
	b.WriteString("kind: plan\n")
	fmt.Fprintf(&b, "status: %s\n", DocStatus(d.GoalStatus))
	summary := Summary(d)
	fmt.Fprintf(&b, "summary: %s\n", summary)
	if d.Parent != "" {
		fmt.Fprintf(&b, "parent: %s\n", d.Parent)
	}
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# Goal — %s\n\n", strings.TrimSpace(d.Title))
	fmt.Fprintf(&b, "Mesh goal `%s`, %s.\n\n", d.Slug, d.GoalStatus)
	if DocStatus(d.GoalStatus) != "draft" {
		// the goal has ended: the body carries its final state too.
		fmt.Fprintf(&b, "**%s**\n\n", summary)
	}
	if s := strings.TrimSpace(d.Statement); s != "" {
		fmt.Fprintf(&b, "## Statement\n\n%s\n\n", s)
	}
	if s := strings.TrimSpace(d.Acceptance); s != "" {
		fmt.Fprintf(&b, "## Acceptance\n\n%s\n\n", s)
	}
	b.WriteString("## Items\n\n| Seq | Title | Status | Branch | PR | Cost |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, it := range d.Items {
		pr := it.PRURL
		if pr == "" {
			pr = "—"
		}
		branch := it.Branch
		if branch == "" {
			branch = "—"
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | $%.2f |\n", it.Seq, cell(it.Title), it.Status, cell(branch), cell(pr), float64(it.CostCents)/100)
	}
	for _, sec := range kept {
		b.WriteString("\n" + sec + "\n")
	}
	b.WriteString("\n" + logHeading + "\n\n")
	for _, l := range logLines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

func cell(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", "\\|")
}

// ownHeadings are the sections RenderGoalDoc writes; every other `## `
// section of an existing document is someone else's and is kept.
var ownHeadings = map[string]bool{"## Statement": true, "## Acceptance": true, "## Items": true, logHeading: true}

// existingSections returns the log lines of a rendered document and,
// in order, the `## ` sections it does not own, each trimmed of blank
// lines at either end. A section after the log is still a section: only
// the log's own `- ` lines are log lines.
func existingSections(doc string) (logLines, kept []string) {
	var heading string
	var body []string
	flush := func() {
		switch {
		case heading == logHeading:
			for _, l := range body {
				if l = strings.TrimRight(l, " \t\r"); strings.HasPrefix(l, "- ") {
					logLines = append(logLines, l)
				}
			}
		case heading != "" && !ownHeadings[heading]:
			kept = append(kept, strings.Trim(heading+"\n"+strings.Join(body, "\n"), "\n"))
		}
	}
	inFront := strings.HasPrefix(doc, "---\n")
	for i, l := range strings.Split(doc, "\n") {
		if inFront {
			if i > 0 && l == "---" {
				inFront = false
			}
			continue
		}
		if strings.HasPrefix(l, "## ") {
			flush()
			heading, body = strings.TrimRight(l, " \t\r"), nil
			continue
		}
		body = append(body, l)
	}
	flush()
	return logLines, kept
}

// WriteGoalDoc renders and writes the document in workspace. It reports
// whether the file changed and the path relative to the workspace.
func WriteGoalDoc(workspace string, d GoalDoc, logLine string) (rel string, changed bool, err error) {
	path := GoalDocPath(workspace, d.Slug)
	rel, _ = filepath.Rel(workspace, path)
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return rel, false, readErr
	}
	out := RenderGoalDoc(string(existing), d, logLine)
	if string(existing) == out {
		return rel, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return rel, false, err
	}
	return rel, true, os.WriteFile(path, []byte(out), 0o644)
}

// RecordEndedGoal writes the goal's document once, when the goal has
// ended (ADR 003, decision 4): into its primary repo — the project's
// first — on that repo's default branch, committed and pushed straight,
// with no item branch and no PR. A goal still live writes nothing. The
// checkout is its own (seq 0: no item has it), fresh from the default
// branch, since the primary repo need not be the one an item ran in;
// like the planner's Fetch it is project-wide, not one item's, so it is
// on the node's own credentials. No log line is appended: the doc is the
// goal's final state alone, the same whichever finish writes it, so a
// second writer finds it unchanged and pushes nothing.
func RecordEndedGoal(ctx context.Context, gw Gateway, ws Workspacer, root, orgLogin, goalID string, logf func(string, ...any)) error {
	goal, err := gw.Goal(ctx, goalID)
	if err != nil {
		return fmt.Errorf("goal: %w", err)
	}
	if isLiveGoal(goal.Status) {
		return nil
	}
	project, err := gw.Project(ctx, goal.ProjectID)
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}
	if len(project.RepoIDs) == 0 {
		return fmt.Errorf("project %s has no repos", project.Slug)
	}
	primary, err := gw.Repo(ctx, project.RepoIDs[0])
	if err != nil {
		return fmt.Errorf("primary repo: %w", err)
	}
	base := primary.DefaultBranch
	if base == "" {
		base = "main"
	}
	dir, err := ws.Prepare(ctx, WorkspaceSpec{Root: root, GoalSlug: goal.Slug, Seq: 0, Repo: primary.FullName(), Branch: base, BaseRef: base})
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	items, err := gw.GoalWorkItems(ctx, goal.ID)
	if err != nil {
		return fmt.Errorf("items: %w", err)
	}
	parent := ""
	if cx, err := FetchContext(ctx, gw, orgLogin, project.Slug); err == nil {
		parent = cx.PRDSlug
	}
	doc := GoalDoc{Slug: goal.Slug, Title: goal.Title, Statement: goal.Statement, Acceptance: goal.Acceptance,
		GoalStatus: goal.Status, Parent: parent, SpentCents: goal.SpentCents, Items: items}
	rel, changed, err := WriteGoalDoc(dir, doc, "")
	if err != nil || !changed {
		return err
	}
	if err := ws.Commit(ctx, dir, []string{rel}, fmt.Sprintf("docs(mesh): goal %s — finished", goal.Slug)); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if _, err := ws.Push(ctx, dir, base); err != nil {
		return fmt.Errorf("push %s: %w", base, err)
	}
	if logf != nil {
		logf("goal %s: %s; wrote %s to %s@%s", goal.Slug, goal.Status, rel, primary.FullName(), base)
	}
	return nil
}
