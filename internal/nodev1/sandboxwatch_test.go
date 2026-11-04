package nodev1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

func TestReadSandboxLimitsConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")

	if _, ok, err := ReadSandboxLimitsConfig(path); err != nil || ok {
		t.Fatalf("a missing file: ok %v, err %v, want false, nil", ok, err)
	}

	if err := os.WriteFile(path, []byte("sandbox:\n  mode: on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadSandboxLimitsConfig(path); err != nil || ok {
		t.Fatalf("a file naming no sandbox limits: ok %v, err %v, want false, nil", ok, err)
	}

	if err := os.WriteFile(path, []byte("sandbox:\n  cpus: 4\n  memory: 8g\n  pids: 256\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, ok, err := ReadSandboxLimitsConfig(path)
	if err != nil || !ok {
		t.Fatalf("a file naming cpus/memory/pids: ok %v, err %v, want true, nil", ok, err)
	}
	if l != (sandbox.Limits{CPUs: 4, Memory: "8g", Pids: 256}) {
		t.Fatalf("read %+v", l)
	}
}

// WatchSandboxLimits applies a change from its read function on its
// poll, with no reload, and keeps polling once it has.
func TestWatchSandboxLimitsAppliesOnEachPoll(t *testing.T) {
	pods := &sandbox.Sandbox{}
	r := &Runtime{Pods: pods}

	var current sandbox.Limits
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.WatchSandboxLimits(ctx, func() (sandbox.Limits, bool, error) { return current, true, nil }, 10*time.Millisecond, nil)
		close(done)
	}()

	current = sandbox.Limits{CPUs: 4, Memory: "8g", Pids: 256}
	deadline := time.Now().Add(2 * time.Second)
	for pods.AgentLimits() != current {
		if time.Now().After(deadline) {
			t.Fatalf("agent limits never reached %+v: got %+v", current, pods.AgentLimits())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// A reload signal applies a changed read at once, without waiting for
// the (here, hour-long) poll.
func TestWatchSandboxLimitsAppliesAtOnceOnReload(t *testing.T) {
	pods := &sandbox.Sandbox{}
	r := &Runtime{Pods: pods}

	var mu sync.Mutex
	current := sandbox.Limits{}
	read := func() (sandbox.Limits, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, true, nil
	}

	reload := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.WatchSandboxLimits(ctx, read, time.Hour, reload)
		close(done)
	}()

	want := sandbox.Limits{CPUs: 2, Memory: "4g", Pids: 128}
	mu.Lock()
	current = want
	mu.Unlock()

	select {
	case reload <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("reload was never read")
	}

	deadline := time.Now().Add(2 * time.Second)
	for pods.AgentLimits() != want {
		if time.Now().After(deadline) {
			t.Fatalf("reload never applied %+v within the hour-long poll: got %+v", want, pods.AgentLimits())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// With no change from the read, WatchSandboxLimits never calls
// SetAgentLimits (each call logs on sb.Logf, so no log line is proof).
func TestWatchSandboxLimitsSkipsUnchanged(t *testing.T) {
	limits := sandbox.Limits{CPUs: 2, Memory: "4g", Pids: 100}
	var mu sync.Mutex
	var logs int
	pods := &sandbox.Sandbox{
		Config: sandbox.Config{Limits: limits},
		Logf: func(string, ...any) {
			mu.Lock()
			logs++
			mu.Unlock()
		},
	}
	r := &Runtime{Pods: pods}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.WatchSandboxLimits(ctx, func() (sandbox.Limits, bool, error) { return limits, true, nil }, 10*time.Millisecond, nil)
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done

	if got := pods.AgentLimits(); got != limits {
		t.Fatalf("agent limits: %+v, want unchanged %+v", got, limits)
	}
	mu.Lock()
	defer mu.Unlock()
	if logs != 0 {
		t.Fatalf("SetAgentLimits logged %d time(s) though the read never changed from AgentLimits", logs)
	}
}

// A read error changes nothing and is logged, and the watcher keeps
// polling afterward.
func TestWatchSandboxLimitsLogsAndSkipsOnReadError(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	pods := &sandbox.Sandbox{}
	r := &Runtime{
		Pods: pods,
		// Runtime.logf always calls Logf("%s", <the formatted, redacted
		// line>), so the line is in args, never in format itself.
		Logf: func(format string, args ...any) {
			mu.Lock()
			for _, a := range args {
				if s, ok := a.(string); ok {
					lines = append(lines, s)
				}
			}
			mu.Unlock()
		},
	}

	errAt := make(chan struct{}, 1)
	errAt <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.WatchSandboxLimits(ctx, func() (sandbox.Limits, bool, error) {
			select {
			case <-errAt:
				return sandbox.Limits{}, false, os.ErrPermission
			default:
				return sandbox.Limits{CPUs: 1, Memory: "1g", Pids: 10}, true, nil
			}
		}, 10*time.Millisecond, nil)
		close(done)
	}()

	want := sandbox.Limits{CPUs: 1, Memory: "1g", Pids: 10}
	deadline := time.Now().Add(2 * time.Second)
	for pods.AgentLimits() != want {
		if time.Now().After(deadline) {
			t.Fatalf("the watcher never recovered after the read error: got %+v, want %+v", pods.AgentLimits(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range lines {
		if strings.Contains(l, "sandbox limits") && strings.Contains(l, os.ErrPermission.Error()) {
			found = true
		}
	}
	if !found {
		t.Fatalf("node.log did not record the read error: %v", lines)
	}
}
