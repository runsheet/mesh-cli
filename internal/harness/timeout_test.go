package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A job's Timeout kills a session that runs past it.
func TestClaudeCodeJobTimeout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s-slow\"}'\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &ClaudeCode{Binary: bin}
	start := time.Now()
	_, err := h.Run(context.Background(), Job{Kind: KindEvaluate, Workspace: dir, Prompt: "review", Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the session was not killed at the timeout")
	}
}
