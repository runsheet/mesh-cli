package commands

import (
	"context"
	"fmt"
	"io"
	"log"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
)

// The loop builds an evaluator afresh for every item on every tick, and
// another for each adopted one: all of them share the node's one Blind,
// so an evaluation that saw no context is said once for the item, not
// once a tick — with or without a Codex binary.
func TestEvaluatorsShareOneBlind(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	loop := &node.Loop{}
	cfg := &node.Config{}
	cfg.Codex.Binary = "no-such-codex-binary"
	second := codexSecondOpinion(cfg, loop, t.TempDir(), logger)
	if second.blind == nil {
		t.Fatal("no Blind without a Codex binary")
	}
	gw := gateway.New("http://127.0.0.1:0", "rs_test")
	a := evaluator(context.Background(), gw, cfg, nil, second, loop, t.TempDir(), nil, nil, logger)
	b := evaluator(context.Background(), gw, cfg, nil, second, loop, t.TempDir(), nil, nil, logger)
	if a.Blind == nil || a.Blind != b.Blind {
		t.Fatalf("Blind %p and %p: each evaluator its own", a.Blind, b.Blind)
	}
	if !a.Blind.First("item") || b.Blind.First("item") {
		t.Fatal("the second evaluator said it again")
	}
}

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
