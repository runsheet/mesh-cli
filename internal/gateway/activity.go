package gateway

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ActivityRow is a MeshActivityIndex row (runsheet's activity feed): one
// thing that happened on an item — a progress note, a claim, a finish —
// with its time, kind, the actor and the summary in the words it was
// reported in. A hosted mesh_ask is recorded as a progress note opening
// `question: `.
type ActivityRow struct {
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	Summary    string    `json:"summary"`
	Actor      string    `json:"actor"`
	LogSeq     int64     `json:"logSeq"`
	WorkItemID string    `json:"workItemId"`
}

const activityFields = "id at kind summary actor logSeq workItemId"

// ItemActivity reads the item's MeshActivityIndex rows by work_item_id,
// oldest first (by at, then the log's sequence). A platform without
// meshActivityIndexs is Unsupported (IsUnsupported).
func (c *Client) ItemActivity(ctx context.Context, itemID string) ([]ActivityRow, error) {
	var out struct {
		Rows []ActivityRow `json:"meshActivityIndexs"`
	}
	q := fmt.Sprintf(`{ meshActivityIndexs(namespace: %q, where: [%s]) { %s } }`, NS, where("work_item_id", itemID), activityFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, unsupportedRead(err, "meshActivityIndexs")
	}
	sort.SliceStable(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.LogSeq < b.LogSeq
	})
	return out.Rows, nil
}
