package validate

import (
	"context"
	"io"
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
