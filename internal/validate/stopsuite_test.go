package validate

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// stoppableLanes is a LaneRunner whose every shard blocks until its own
// ctx is cancelled or a short timeout, recording which shard index (by
// MESH_SHARD_INDEX) saw its ctx end.
type stoppableLanes struct {
	mu        sync.Mutex
	cancelled map[string]bool
}

func shardIndexIn(cmd string) string {
	const p = ShardIndexEnv + "="
	i := strings.Index(cmd, p)
	if i < 0 {
		return ""
	}
	rest := cmd[i+len(p):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return rest[:j]
	}
	return rest
}

func (s *stoppableLanes) Run(ctx context.Context, cmd string, _ time.Duration, _, _ io.Writer) (int, error) {
	select {
	case <-ctx.Done():
		s.mu.Lock()
		s.cancelled[shardIndexIn(cmd)] = true
		s.mu.Unlock()
		return -1, ctx.Err()
	case <-time.After(300 * time.Millisecond):
		return 0, nil
	}
}

func (s *stoppableLanes) Lane(context.Context, string) (Runner, func(), error) {
	return s, func() {}, nil
}

// Stopping one lane's registered Options.OnLane stop func ends that
// lane's shard alone — its StepResult Stopped and non-zero, its command's
// ctx cancelled — while the other shards run to their own end untouched.
func TestStopOneLaneLeavesTheOthersRunning(t *testing.T) {
	dir := t.TempDir()
	steps := []Step{{Name: "e2e", Command: "true", Timeout: time.Minute, Shards: 4}}
	r := &stoppableLanes{cancelled: map[string]bool{}}

	var mu sync.Mutex
	stops := map[string]func(string){}
	onLane := func(name string, stop func(reason string)) {
		mu.Lock()
		stops[name] = stop
		mu.Unlock()
	}

	stoppedOnce := make(chan struct{})
	go func() {
		for {
			mu.Lock()
			stop, ok := stops["e2e-1"]
			mu.Unlock()
			if ok {
				stop("test: stopping shard 1")
				close(stoppedOnce)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	start := time.Now()
	ctx := WithOnLane(context.Background(), onLane)
	results, err := Run(ctx, steps, r, Options{Dir: dir, ContinueOnFailure: true})
	<-stoppedOnce
	if err != nil {
		t.Fatalf("Run() err = %v, want no runner error from a deliberate stop", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("stopping shard 1 did not return within the test's timeout")
	}
	if len(results) != 1 || len(results[0].Shards) != 4 {
		t.Fatalf("results %+v, want one step with 4 shards", results)
	}
	for _, sh := range results[0].Shards {
		switch sh.Index {
		case 1:
			if !sh.Stopped || sh.ExitCode == 0 || sh.StopReason != "test: stopping shard 1" {
				t.Fatalf("shard 1 = %+v, want stopped, non-zero exit, its reason", sh)
			}
		default:
			if sh.Stopped || sh.ExitCode != 0 {
				t.Fatalf("shard %d = %+v, want not stopped, exit 0", sh.Index, sh)
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cancelled["1"] {
		t.Fatalf("cancelled = %v, want shard 1's ctx seen cancelled", r.cancelled)
	}
	if r.cancelled["0"] || r.cancelled["2"] || r.cancelled["3"] {
		t.Fatalf("cancelled = %v, want only shard 1's ctx ended", r.cancelled)
	}
}

// twoLaneRunner is a LaneRunner whose commands block on ctx until
// stopped or released, recording which commands started and which saw
// their ctx cancelled.
type twoLaneRunner struct {
	mu        sync.Mutex
	started   map[string]bool
	cancelled map[string]bool
	proceedC  chan struct{} // closed to let "cmd-c" finish on its own
}

func (r *twoLaneRunner) Run(ctx context.Context, cmd string, _ time.Duration, _, _ io.Writer) (int, error) {
	r.mu.Lock()
	r.started[cmd] = true
	r.mu.Unlock()
	var done <-chan struct{}
	if cmd == "cmd-c" {
		done = r.proceedC
	}
	select {
	case <-ctx.Done():
		r.mu.Lock()
		r.cancelled[cmd] = true
		r.mu.Unlock()
		return -1, ctx.Err()
	case <-done:
		return 0, nil
	}
}

func (r *twoLaneRunner) Lane(context.Context, string) (Runner, func(), error) {
	return r, func() {}, nil
}

// Two unsharded steps naming no Group share one lane (ADR-049,
// Options.OnLane's doc comment): before the fix, Options.OnLane fired
// once for that whole lane, named by its group ("validate"), so
// stop_suite's lookup by the step's own name ("a") found nothing.
// Now it fires once per unit, so stopping "a" by its own name ends it
// alone — its still-queued lane-mate "b" never runs, the lane going as
// a failing exit would — while "c", naming its own Group and so its
// own lane, is untouched.
func TestStopOneUnsharedStepInASharedLaneByItsOwnName(t *testing.T) {
	dir := t.TempDir()
	steps := []Step{
		{Name: "a", Command: "cmd-a", Timeout: time.Minute},
		{Name: "b", Command: "cmd-b", Timeout: time.Minute},
		{Name: "c", Command: "cmd-c", Timeout: time.Minute, Group: "other"},
	}
	r := &twoLaneRunner{started: map[string]bool{}, cancelled: map[string]bool{}, proceedC: make(chan struct{})}

	var mu sync.Mutex
	stops := map[string]func(string){}
	var names []string
	onLane := func(name string, stop func(reason string)) {
		mu.Lock()
		stops[name] = stop
		names = append(names, name)
		mu.Unlock()
	}

	stoppedOnce := make(chan struct{})
	go func() {
		for {
			mu.Lock()
			stop, ok := stops["a"]
			mu.Unlock()
			if ok {
				stop("test: stopping a")
				close(stoppedOnce)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	go func() {
		// let "c" run a little before releasing it, so it is seen
		// running concurrently with "a" rather than finishing first
		time.Sleep(50 * time.Millisecond)
		close(r.proceedC)
	}()

	ctx := WithOnLane(context.Background(), onLane)
	results, err := Run(ctx, steps, r, Options{Dir: dir, ContinueOnFailure: true})
	<-stoppedOnce
	if err != nil {
		t.Fatalf("Run() err = %v, want no runner error from a deliberate stop", err)
	}

	mu.Lock()
	gotNames := append([]string(nil), names...)
	mu.Unlock()
	for _, want := range []string{"a", "c"} {
		if !slices.Contains(gotNames, want) {
			t.Fatalf("OnLane names %v, want %q named by its own step, not its shared lane", gotNames, want)
		}
	}
	if slices.Contains(gotNames, "validate") {
		t.Fatalf("OnLane names %v, want never the lane's own name", gotNames)
	}
	if slices.Contains(gotNames, "b") {
		t.Fatalf("OnLane names %v, want \"b\" never registered: its lane stopped before reaching it", gotNames)
	}

	byName := map[string]StepResult{}
	for _, sr := range results {
		byName[sr.Name] = sr
	}
	a, ok := byName["a"]
	if !ok || !a.Stopped || a.ExitCode == 0 || a.StopReason != "test: stopping a" {
		t.Fatalf("step a = %+v, ok=%v, want stopped, non-zero exit, its reason", a, ok)
	}
	if _, ok := byName["b"]; ok {
		t.Fatalf("step b present in %+v, want absent: never run once its lane stopped on a", results)
	}
	c, ok := byName["c"]
	if !ok || c.Stopped || c.ExitCode != 0 {
		t.Fatalf("step c = %+v, ok=%v, want run to completion, untouched by a's stop", c, ok)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started["cmd-b"] {
		t.Fatalf("started = %v, want cmd-b never run", r.started)
	}
	if !r.cancelled["cmd-a"] {
		t.Fatalf("cancelled = %v, want cmd-a's ctx seen cancelled", r.cancelled)
	}
	if r.cancelled["cmd-c"] {
		t.Fatalf("cancelled = %v, want cmd-c's ctx never cancelled", r.cancelled)
	}
}
