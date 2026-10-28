package node

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingStep is a validation step that runs until release is closed,
// saying when it started.
type blockingStep struct {
	started chan struct{}
	release chan struct{}
}

func (b blockingStep) Run(context.Context, string, time.Duration, io.Writer, io.Writer) (int, error) {
	close(b.started)
	<-b.release
	return 0, nil
}

// The hourly trim never cleans the Go cache under a running step (dl360,
// 2026-10-28: `go clean -cache` under compiling steps failed every one):
// it waits for the step holding the cache to end, says so, and cleans
// after.
func TestATrimWaitsForARunningStep(t *testing.T) {
	root := t.TempDir()
	goDir := filepath.Join(root, "go")
	if err := os.MkdirAll(goDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goDir, "big"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	var cleaned atomic.Bool
	var logs syncBuf
	c := &SharedCache{Root: root, MaxBytes: 1024,
		Logf: func(f string, a ...any) { _, _ = logs.Write([]byte(strings.TrimSpace(fmt.Sprintf(f, a...)) + "\n")) },
		CleanGo: func(_ context.Context, dir string) error {
			cleaned.Store(true)
			return os.Remove(filepath.Join(dir, "big"))
		}}

	step := blockingStep{started: make(chan struct{}), release: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = goCacheRunner{step}.Run(context.Background(), "go test ./...", 0, io.Discard, io.Discard)
	}()
	<-step.started

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, _, err := c.Trim(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	if cleaned.Load() {
		t.Fatal("the Go cache was cleaned under a running step")
	}
	select {
	case <-done:
		t.Fatal("the trim ended with the step still running")
	default:
	}
	close(step.release)
	wg.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the trim did not finish once the step ended")
	}
	if !cleaned.Load() {
		t.Fatal("the Go cache was never cleaned")
	}
	if !strings.Contains(logs.String(), "go clean -cache waits for the validation steps") {
		t.Fatalf("no wait line:\n%s", logs.String())
	}
}
