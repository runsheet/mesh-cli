package validate

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// failFastLanes is a LaneRunner whose every lane runs shard 1 to a quick
// exit 1 and every other command until its ctx ends.
type failFastLanes struct{}

func (failFastLanes) Run(ctx context.Context, cmd string, _ time.Duration, _, _ io.Writer) (int, error) {
	if strings.Contains(cmd, ShardIndexEnv+"=1 ") {
		return 1, nil
	}
	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case <-time.After(10 * time.Second):
		return 0, nil
	}
}

func (f failFastLanes) Lane(context.Context, string) (Runner, func(), error) {
	return f, func() {}, nil
}

// With fail fast the first failing shard stops the run: the shards still
// running are cancelled at once and named not run, never a runner's
// error, and the step is the failing shard's.
func TestFailFastStopsTheOtherShards(t *testing.T) {
	steps := []Step{{Name: "e2e", Command: "go test ./e2e", Timeout: time.Minute, Shards: 4}}
	start := time.Now()
	res, err := Run(WithFailFast(context.Background()), steps, failFastLanes{}, Options{Dir: t.TempDir(), ContinueOnFailure: true})
	if err != nil {
		t.Fatalf("a fail-fast stop is no runner error: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("fail fast waited for the other shards")
	}
	if len(res) != 1 || res[0].ExitCode != 1 || len(res[0].Shards) != 1 || res[0].Shards[0].Index != 1 {
		t.Fatalf("results %+v", res)
	}
	for _, k := range []string{"shard 1/4: not run", "shard 3/4: not run", "shard 4/4: not run"} {
		if !strings.Contains(res[0].Output, k) {
			t.Fatalf("output %q does not say %q", res[0].Output, k)
		}
	}
}

// Without fail fast every shard runs to its end.
func TestWithoutFailFastEveryShardRuns(t *testing.T) {
	steps := []Step{{Name: "e2e", Command: "x", Timeout: time.Minute, Shards: 3}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, _ := Run(ctx, steps, failFastLanes{}, Options{Dir: t.TempDir(), ContinueOnFailure: true})
	if len(res) != 1 || len(res[0].Shards) != 3 {
		t.Fatalf("results %+v, want every shard's", res)
	}
}

// budget: is the repository's bound on a validation's wall time; a
// budget that is no positive duration is refused.
func TestParseReadsTheBudget(t *testing.T) {
	cfg, err := Parse([]byte("budget: 80m\nsuites:\n  - name: u\n    command: true\n"))
	if err != nil || cfg.Budget != 80*time.Minute {
		t.Fatalf("budget %s, %v", cfg.Budget, err)
	}
	if cfg, err := Parse([]byte("suites:\n  - name: u\n    command: true\n")); err != nil || cfg.Budget != 0 {
		t.Fatalf("no budget: %s, %v", cfg.Budget, err)
	}
	for _, b := range []string{"soon", "-5m", "0s"} {
		if _, err := Parse([]byte("budget: " + b + "\nsuites:\n  - name: u\n    command: true\n")); err == nil || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("budget %s: %v", b, err)
		}
	}
}
