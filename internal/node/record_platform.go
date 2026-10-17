package node

import (
	"context"
	"encoding/json"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// activityReader is the gateway's MeshActivityIndex read by work item
// (gateway.Client.ItemActivity); a Gateway without it has no platform
// record to give.
type activityReader interface {
	ItemActivity(ctx context.Context, itemID string) ([]gateway.ActivityRow, error)
}

// questionPrefix opens the progress note a hosted mesh_ask records its
// question as (gatewaytest's mcp.go, runsheet's mesh_ask).
const questionPrefix = "question: "

// answerPrefix opens a row that carries an ask's answer, should the
// platform record one in the activity feed.
const answerPrefix = "answer: "

// itemRecord is the item's record for a fresh session: the node's own
// record.jsonl when it has one (ItemRecord), else the same entries built
// from the platform's activity rows for the item (recordFromActivity),
// held under RecordBudget the same way and logged `item <label>: record
// read from the platform (N entries)`. ok is false when neither has one.
func (r *Runner) itemRecord(ctx context.Context, orgID, itemID, label string) (entries []RecordEntry, lines []string, dropped int, ok bool) {
	if store := r.resumes(orgID); store != nil {
		if entries, lines, dropped, ok = ItemRecord(*store, itemID); ok {
			return entries, lines, dropped, true
		}
	}
	ar, has := r.GW.(activityReader)
	if !has {
		return nil, nil, 0, false
	}
	rows, err := ar.ItemActivity(ctx, itemID)
	if err != nil {
		if !gateway.IsUnsupported(err) {
			r.logf("item %s: record: activity read: %v", label, err)
		}
		return nil, nil, 0, false
	}
	all := recordFromActivity(rows)
	if len(all) == 0 {
		return nil, nil, 0, false
	}
	raw := make([]string, len(all))
	for i, e := range all {
		data, err := json.Marshal(e)
		if err != nil {
			return nil, nil, 0, false
		}
		raw[i] = string(data)
	}
	entries, lines, dropped = clipRecord(all, raw)
	r.logf("item %s: record read from the platform (%d entries)", label, len(entries))
	return entries, lines, dropped, true
}

// recordFromActivity maps an item's activity rows, oldest first, to
// record entries in the words the platform kept: a progress note is a
// note, one opening `question: ` the question it asked (the prefix
// dropped, the kind carrying it), one opening `answer: ` or of an answer
// kind its answer. Rows of other kinds — a claim, a finish — and rows
// with no summary are not the session's words and are left out.
//
// The platform does not serve an ask's answer yet (the draft goal
// the-node-resumes-a-session-when-its-question-is-answered adds it), so
// today a question is recorded alone; the answer rows are read the day
// it lands.
func recordFromActivity(rows []gateway.ActivityRow) []RecordEntry {
	var out []RecordEntry
	for _, row := range rows {
		if row.Summary == "" {
			continue
		}
		kind := strings.ToLower(row.Kind)
		var e RecordEntry
		switch {
		case strings.Contains(kind, "answer"):
			e = RecordEntry{Kind: "answer", Text: strings.TrimPrefix(row.Summary, answerPrefix)}
		case strings.Contains(kind, "question"):
			e = RecordEntry{Kind: "question", Text: strings.TrimPrefix(row.Summary, questionPrefix)}
		case strings.Contains(kind, "progress") || strings.Contains(kind, "note"):
			switch {
			case strings.HasPrefix(row.Summary, questionPrefix):
				e = RecordEntry{Kind: "question", Text: strings.TrimPrefix(row.Summary, questionPrefix)}
			case strings.HasPrefix(row.Summary, answerPrefix):
				e = RecordEntry{Kind: "answer", Text: strings.TrimPrefix(row.Summary, answerPrefix)}
			default:
				e = RecordEntry{Kind: "note", Text: row.Summary}
			}
		default:
			continue
		}
		e.At = row.At
		out = append(out, e)
	}
	return out
}
