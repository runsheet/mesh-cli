package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/jobresult"
	"projectmesh.io/mesh-cli/internal/mcptool"
)

// handInScript is a scripted session handing the node its result through
// mesh_result (ADR-041), as the model would: result when the test gave
// one; else the mesh-work call the script named (mesh_verdict,
// mesh_plan) as the finish's object; else the script's final message
// read the way the model was once told to write it — a VERDICT line and
// its FAILED lines, a fenced JSON graph. The node reads none of that
// text itself any more; this is the fake session reading its own script.
// Nothing is handed in for a job with no node tools (a build) or a kind
// with no session result.
func handInScript(job harness.Job, result map[string]any, text string, workCalls map[string]map[string]any) {
	tool, ok := mcptool.Find(job.NodeTools, jobresult.ResultTool)
	if !ok {
		return
	}
	if result == nil {
		if why := scriptedFailure(text); why != "" {
			if fail, ok := mcptool.Find(job.NodeTools, jobresult.FailTool); ok {
				_, _ = fail.Call(context.Background(), map[string]any{"error": why})
			}
			return
		}
		result = scriptedResult(job.Kind, text, workCalls)
	}
	if result == nil {
		return
	}
	_, _ = tool.Call(context.Background(), result)
}

// scriptedResult is the mesh_result args a script implies, nil for none.
func scriptedResult(kind harness.Kind, text string, workCalls map[string]map[string]any) map[string]any {
	switch kind {
	case harness.KindEvaluate:
		if in, ok := workCalls["mesh_verdict"]; ok {
			return map[string]any{"verdict": in}
		}
		if v := scriptedVerdict(text); v != nil {
			return map[string]any{"verdict": v}
		}
	case harness.KindPlan:
		if in, ok := workCalls["mesh_plan"]; ok {
			return map[string]any{"plan": in}
		}
		if graph := fencedJSON(text); graph != nil {
			if graph["items"] != nil || graph["split"] != nil {
				return map[string]any{"plan": graph}
			}
		}
	case harness.KindResearch:
		if graph := fencedJSON(text); graph != nil {
			if graph["goals"] != nil || graph["pitches"] != nil {
				return map[string]any{"research": graph}
			}
		}
	case harness.KindRebase:
		if head, files, ok := scriptedRebase(text); ok {
			return map[string]any{"rebasedHead": map[string]any{"headSha": head, "files": files}}
		}
	}
	return nil
}

// scriptedRebase reads a script's REBASED/RESOLVED lines, the shape the
// fixtures wrote before the result tool; ok is false with none.
func scriptedRebase(text string) (head string, files []any, ok bool) {
	files = []any{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		l := strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "`*_"))
		upper := strings.ToUpper(l)
		switch {
		case strings.HasPrefix(upper, "REBASED:"):
			head = strings.Trim(strings.TrimSpace(l[len("REBASED:"):]), "`*_ ")
			ok = head != ""
		case strings.HasPrefix(upper, "RESOLVED:"):
			for _, f := range strings.Split(l[len("RESOLVED:"):], ",") {
				if f = strings.TrimSpace(f); f != "" && f != "(none)" {
					files = append(files, f)
				}
			}
		}
	}
	return head, files, ok
}

// scriptedFailure is a script's REBASE FAILED line, "" with none: the
// fake session's mesh_fail (handInScript).
func scriptedFailure(text string) string {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		l := strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "`*_"))
		if strings.HasPrefix(strings.ToUpper(l), "REBASE FAILED:") {
			return strings.TrimSpace(l[len("REBASE FAILED:"):])
		}
	}
	return ""
}

// scriptedVerdict reads a script's VERDICT line, the notes above it and
// its FAILED/SATISFY lines; nil when the script reaches no verdict.
func scriptedVerdict(text string) map[string]any {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(strings.Trim(strings.TrimSpace(lines[i]), "`*_"))
		var passed bool
		switch {
		case strings.EqualFold(l, harness.VerdictPass):
			passed = true
		case strings.EqualFold(l, harness.VerdictFail):
		default:
			continue
		}
		notes := strings.TrimSpace(strings.Join(lines[:i], "\n"))
		verdict := map[string]any{"passed": passed, "notes": notes}
		var clauses []any
		for _, line := range lines[:i] {
			c := strings.TrimSpace(strings.Trim(strings.TrimLeft(strings.TrimSpace(line), "-*•># \t"), "`*_"))
			if !strings.HasPrefix(strings.ToUpper(c), harness.ClauseFailed) {
				continue
			}
			clause, satisfy, ok := strings.Cut(c[len(harness.ClauseFailed):], harness.ClauseSep)
			if !ok {
				continue
			}
			satisfy = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(satisfy), harness.ClauseSatisfy))
			if clause = strings.TrimSpace(clause); clause != "" && satisfy != "" {
				clauses = append(clauses, map[string]any{"clause": clause, "satisfy": satisfy})
			}
		}
		if len(clauses) > 0 {
			verdict["failedClauses"] = clauses
		}
		return verdict
	}
	return nil
}

// fencedJSON is the last fenced JSON object of a script, or the whole
// script when it is one; nil for none.
func fencedJSON(text string) map[string]any {
	raw := strings.TrimSpace(text)
	candidates := []string{raw}
	if i := strings.LastIndex(raw, "```json"); i >= 0 {
		body := raw[i+len("```json"):]
		if j := strings.Index(body, "```"); j >= 0 {
			body = body[:j]
		}
		candidates = append([]string{strings.TrimSpace(body)}, candidates...)
	}
	for _, c := range candidates {
		var m map[string]any
		if err := json.Unmarshal([]byte(c), &m); err == nil && m != nil {
			return m
		}
	}
	return nil
}

// planOf reads a scripted graph — fenced or bare, an object with items
// or a bare array — as DecodePlan reads the plan handed in: the tests'
// fixtures were written as final messages before the result tool.
func planOf(text string, merged ...int) ([]harness.PlannedSpec, error) {
	graph := fencedJSON(text)
	if graph == nil {
		raw := strings.TrimSpace(text)
		if i := strings.LastIndex(raw, "```json"); i >= 0 {
			raw = raw[i+len("```json"):]
			if j := strings.Index(raw, "```"); j >= 0 {
				raw = raw[:j]
			}
			raw = strings.TrimSpace(raw)
		}
		var list []any
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil, errors.New("no graph in the script")
		}
		graph = map[string]any{"items": list}
	}
	return harness.DecodePlan(graph, merged...)
}

// verdictOfScript reads a scripted verdict as the fixtures wrote it: its
// passed, the notes above the VERDICT line, and its FAILED clauses.
func verdictOfScript(text string) (passed bool, notes string, clauses []gateway.FailedClause, ok bool) {
	v := scriptedVerdict(text)
	if v == nil {
		return false, "", nil, false
	}
	passed, _ = v["passed"].(bool)
	notes, _ = v["notes"].(string)
	list, _ := v["failedClauses"].([]any)
	for _, e := range list {
		m := e.(map[string]any)
		clauses = append(clauses, gateway.FailedClause{Clause: m["clause"].(string), Satisfy: m["satisfy"].(string)})
	}
	return passed, notes, clauses, true
}
