package commands

import (
	"fmt"
	"testing"

	"projectmesh.io/mesh-cli/internal/node"
)

// The loop leaves to the evaluator the errors it says once per item.
func TestSaidByEvaluator(t *testing.T) {
	for _, c := range []struct {
		err  error
		said bool
	}{
		{node.ErrFamilyWaits, true},
		{fmt.Errorf("evaluate: %w", node.ErrNoContext), true},
		{fmt.Errorf("record verdict: refused"), false},
	} {
		if got := saidByEvaluator(c.err); got != c.said {
			t.Errorf("saidByEvaluator(%v) = %v", c.err, got)
		}
	}
}
