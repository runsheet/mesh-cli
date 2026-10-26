package node

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// A plan release the gateway refuses is logged with the gateway's own
// answer, naming the goal and the call. The subtest drives the real
// planner path — a fetch that fails, which releases the plan claim —
// against the fake, which refuses releasePlan.
func TestHandInRefusalIsLoggedByThePlanner(t *testing.T) {
	t.Run("ReleasePlan", func(t *testing.T) {
		refusal := "the gateway says no to ReleasePlan (numbat-5)"
		f := widgetsAlone(t)
		f.Refuse["releasePlan"] = refusal
		var logs bytes.Buffer
		pl, ws, _, cfg := planTokenPlanner(f, t, &logs)
		ws.fetchErr = errors.New("gh repo clone acme/widgets: exit status 4")
		if err := pl.Plan(context.Background(), shipIt(), scopesOf(cfg)[0]); err == nil {
			t.Fatal("plan succeeded on a failing fetch")
		}
		var line string
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, "ReleasePlan refused: ") {
				line = l
			}
		}
		if !strings.Contains(line, "item ship-it: ReleasePlan refused: ") || !strings.Contains(line, refusal) {
			t.Fatalf("no refusal line naming ship-it, ReleasePlan and %q:\n%s", refusal, logs.String())
		}
	})
}
