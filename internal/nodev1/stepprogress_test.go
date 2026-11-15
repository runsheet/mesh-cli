package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/validate"
)

// fakeClock is a clock a test moves.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// tenMinuteSuite is a run_suite command that takes ten minutes of the
// clock, thirty seconds at a time, writing a line at each when loud and
// nothing when not. seen is called after every thirty seconds.
type tenMinuteSuite struct {
	clock *fakeClock
	loud  bool
	seen  func()
}

func (s tenMinuteSuite) Run(_ context.Context, _ string, _ time.Duration, stdout, _ io.Writer) (int, error) {
	for i := range 20 {
		s.clock.add(30 * time.Second)
		if s.loud {
			fmt.Fprintf(stdout, "ok  \tpkg/%d\t0.5s\n", i)
		}
		s.seen()
	}
	return 0, nil
}

// stepRig is a runtime holding one lease, on a clock the test moves,
// and the lease's run_suite over cmd.
func stepRig(t *testing.T, clock *fakeClock, cmd func(rt *Runtime) validate.Runner) (*Runtime, mcptool.Tool) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	yml := "suites:\n  - name: integration\n    kind: integration\n    command: go test ./e2e\n"
	if err := os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{now: clock.now, SuiteWait: 20 * time.Second}
	rt.leases.Put("lease-1", &running{progressAt: clock.now()})
	suites := func(ctx context.Context, workspace string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		return validate.Run(ctx, steps, cmd(rt), validate.Options{Dir: workspace, ContinueOnFailure: true})
	}
	b := &meshapi.Brief{JobID: "job-1", LeaseID: "lease-1", LeaseN: 1, NodeTools: []string{nodetools.RunSuiteTool}}
	tools, rec := rt.nodeTools(b, nil, dir, nil, nil, nil, suites, nil, leaseClock{ctx: context.Background(), began: clock.now()}, nil, nil)
	if len(tools) != 1 {
		t.Fatalf("%d node tools, want run_suite", len(tools))
	}
	rec.Mesh = nil // the record is not what is tested
	return rt, tools[0]
}

func progressOf(rt *Runtime) time.Time {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.lease("lease-1").progressAt
}

// A lease running a long command step is working, and Mesh's stall
// limit must see it: a ten-minute run_suite that writes output moves
// the lease's progress_at all the way through, never more than a
// minute (and one write) behind the clock, and no more often than once
// a minute.
func TestARunSuiteWithOutputReportsProgressThroughout(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	start := clock.now()
	moves, last := 0, start
	rt, tool := stepRig(t, clock, func(rt *Runtime) validate.Runner {
		return tenMinuteSuite{clock: clock, loud: true, seen: func() {
			at := progressOf(rt)
			if behind := clock.now().Sub(at); behind > stepProgressEvery {
				t.Errorf("at %s the lease's progress is %s old", clock.now().Sub(start), behind)
			}
			if at.After(last) {
				if gap := at.Sub(last); moves > 0 && gap < stepProgressEvery {
					t.Errorf("progress moved twice in %s", gap)
				}
				moves, last = moves+1, at
			}
		}}
	})
	text, err := tool.Call(context.Background(), map[string]any{"suite": "integration"})
	if err != nil {
		t.Fatal(err)
	}
	var ans map[string]any
	json.Unmarshal([]byte(text), &ans)
	if ans["status"] != "finished" || ans["exit"] != float64(0) {
		t.Fatalf("answer %s", text)
	}
	if moves < 9 || moves > 11 {
		t.Fatalf("progress moved %d times in ten minutes, want about once a minute", moves)
	}
	if behind := clock.now().Sub(progressOf(rt)); behind > stepProgressEvery {
		t.Fatalf("the run ended with progress %s old", behind)
	}
}

// A run_suite that writes nothing is what a hung one looks like: its
// call is in flight and its command alive for ten minutes, and the
// lease's progress_at stays where it was, so Mesh's stall limit can
// hand it back.
func TestASilentRunSuiteReportsNoProgress(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	start := clock.now()
	rt, tool := stepRig(t, clock, func(rt *Runtime) validate.Runner {
		return tenMinuteSuite{clock: clock, seen: func() {
			if at := progressOf(rt); !at.Equal(start) {
				t.Errorf("a silent run moved the lease's progress to %s", at.Sub(start))
			}
		}}
	})
	if _, err := tool.Call(context.Background(), map[string]any{"suite": "integration"}); err != nil {
		t.Fatal(err)
	}
	if at := progressOf(rt); !at.Equal(start) {
		t.Fatalf("progress moved %s over a silent ten minutes", at.Sub(start))
	}
	if clock.now().Sub(start) != 10*time.Minute {
		t.Fatalf("the run took %s of the clock", clock.now().Sub(start))
	}
}

// A run that goes quiet stops reporting: progress follows the output
// and stays at the last write.
func TestARunSuiteThatGoesQuietStopsReporting(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	var lastWrite time.Time
	rt, tool := stepRig(t, clock, func(rt *Runtime) validate.Runner {
		return runnerFunc(func(stdout io.Writer) {
			for range 6 {
				clock.add(30 * time.Second)
				io.WriteString(stdout, "working\n")
			}
			lastWrite = clock.now()
			clock.add(20 * time.Minute) // hung
		})
	})
	if _, err := tool.Call(context.Background(), map[string]any{"suite": "integration"}); err != nil {
		t.Fatal(err)
	}
	at := progressOf(rt)
	if at.After(lastWrite) || lastWrite.Sub(at) > stepProgressEvery {
		t.Fatalf("progress is at %s, the last write at %s", at, lastWrite)
	}
	if behind := clock.now().Sub(at); behind < 20*time.Minute {
		t.Fatalf("progress is only %s old after 20 silent minutes", behind)
	}
}

type runnerFunc func(stdout io.Writer)

func (f runnerFunc) Run(_ context.Context, _ string, _ time.Duration, stdout, _ io.Writer) (int, error) {
	f(stdout)
	return 0, nil
}

// A node tool's call in flight moves the lease's progress — "still
// running push" — until it answers, and not after; run_suite's call is
// not one of them, since its run's output speaks for it.
func TestANodeToolCallInFlightReportsProgressUntilItAnswers(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	rt := &Runtime{stepBeat: 5 * time.Millisecond}
	// every look at the clock is a minute on, so each beat may move
	rt.now = func() time.Time { clock.add(time.Minute); return clock.now() }
	rt.leases.Put("lease-1", &running{progressAt: clock.now()})
	release, entered := make(chan struct{}), make(chan struct{})
	held := func(ctx context.Context, _ map[string]any) (string, error) {
		close(entered)
		<-release
		return "ok", nil
	}
	tools := rt.reportsWhileRunning(context.Background(), "lease-1", []mcptool.Tool{
		{Name: nodetools.PushTool, Call: held},
		{Name: nodetools.RunSuiteTool, Call: func(context.Context, map[string]any) (string, error) { return "ok", nil }},
	})
	step := func() (time.Time, string) {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		l := rt.lease("lease-1")
		return l.progressAt, l.step
	}
	if _, err := tools[1].Call(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, what := step(); what != "" {
		t.Fatalf("run_suite's call moved progress: %q", what)
	}
	done := make(chan error, 1)
	go func() { _, err := tools[0].Call(context.Background(), nil); done <- err }()
	<-entered
	first, _ := step()
	waitFor(t, "progress to move while the call is in flight", func() bool {
		at, what := step()
		return what == "still running push" && at.Sub(first) >= 3*time.Minute
	})
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, _ := step()
	time.Sleep(50 * time.Millisecond)
	if at, _ := step(); !at.Equal(after) {
		t.Fatalf("progress moved %s after the call answered", at.Sub(after))
	}
}

// A lease the node no longer holds has no progress to move.
func TestStepProgressOfALeaseTheNodeDoesNotHold(t *testing.T) {
	rt := &Runtime{}
	if rt.stepProgress("gone", "run_suite: output") {
		t.Fatal("moved the progress of a lease the node does not hold")
	}
}
