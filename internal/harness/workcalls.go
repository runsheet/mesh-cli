package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

// VerdictCall reads an evaluator's mesh_verdict call (mesh-cli ADR-023,
// sessions finish by tool call): its passed, its notes with one READING
// line per reading appended — so the node's READING handling (ParseReadings,
// the notes' truncation that keeps them) is the text path's — and its
// failed clauses, each with both halves, duplicates dropped, at most
// ClauseMax. A call naming no failed_clauses has its notes' FAILED lines
// read instead. ok is false when the call carries no passed the node can
// read: such a call is no verdict, and the final message is parsed.
func VerdictCall(in map[string]any) (passed bool, notes string, clauses []FailedClause, ok bool) {
	switch v := in["passed"].(type) {
	case bool:
		passed = v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "pass", "passed":
			passed = true
		case "false", "fail", "failed":
		default:
			return false, "", nil, false
		}
	default:
		return false, "", nil, false
	}
	notes = strings.TrimSpace(callString(in["notes"]))
	have := map[string]bool{}
	for _, r := range ParseReadings(notes) {
		have[r.Clause] = true
	}
	var lines []string
	for _, e := range callList(in["readings"]) {
		clause := strings.TrimSpace(callString(e["clause"]))
		value := strings.TrimSpace(callString(e["reading"]))
		if value == "" {
			value = strings.TrimSpace(callString(e["value"]))
		}
		if clause == "" || value == "" || have[clause] {
			continue
		}
		have[clause] = true
		lines = append(lines, fmt.Sprintf("%s %s %s %s", ClauseReading, clause, ClauseSep, value))
	}
	if len(lines) > 0 {
		notes = strings.TrimSpace(notes + "\n\n" + strings.Join(lines, "\n"))
	}
	seen := map[string]bool{}
	for _, e := range callList(in["failed_clauses"]) {
		clause := strings.TrimSpace(callString(e["clause"]))
		satisfy := strings.TrimSpace(callString(e["satisfy"]))
		if clause == "" || satisfy == "" || seen[clause] {
			continue
		}
		seen[clause] = true
		if clauses = append(clauses, FailedClause{Clause: clause, Satisfy: satisfy}); len(clauses) == ClauseMax {
			break
		}
	}
	if len(clauses) == 0 {
		clauses = ParseFailedClauses(notes)
	}
	return passed, notes, clauses, true
}

// PlanCall renders a planner's mesh_plan call (mesh-cli ADR-023) as the
// fenced JSON block its final message would end with — its items,
// questions, checkpoints and split as given — so ParsePlan,
// ParsePlanQuestions, ParsePlanCheckpoints and ParsePlanSplit read the
// call exactly as they read the text, and every check after them is the
// text path's. ok is false when the call carries neither items nor a
// split: the final message is parsed instead.
func PlanCall(in map[string]any) (text string, ok bool) {
	graph := map[string]any{}
	for _, key := range []string{"items", "questions", "checkpoints", "split"} {
		v, has := in[key]
		if !has || v == nil {
			continue
		}
		// a client that sent the array as a JSON string
		if s, isString := v.(string); isString {
			var decoded any
			if err := json.Unmarshal([]byte(s), &decoded); err != nil {
				continue
			}
			v = decoded
		}
		graph[key] = v
	}
	if graph["items"] == nil && graph["split"] == nil {
		return "", false
	}
	b, err := json.MarshalIndent(graph, "", "  ")
	if err != nil {
		return "", false
	}
	return "```json\n" + string(b) + "\n```", true
}

// callString is a work tool argument as a string ("" when it is not one).
func callString(v any) string {
	s, _ := v.(string)
	return s
}

// callList is a work tool argument as a list of objects — given as an
// array, or as an array encoded in a JSON string — its other entries
// dropped.
func callList(v any) []map[string]any {
	if s, ok := v.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(s), &decoded) != nil {
			return nil
		}
		v = decoded
	}
	list, _ := v.([]any)
	var out []map[string]any
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
