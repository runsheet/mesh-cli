package validate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseReadsShardsAndGroup(t *testing.T) {
	cfg, err := Parse([]byte(`suites:
  - name: lint
    kind: lint
    command: go vet ./...
    group: checks
  - name: e2e
    kind: integration
    command: go test ./e2e
    shards: 8
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Steps[0]; got.Group != "checks" || got.Shards != 0 {
		t.Fatalf("lint = group %q shards %d, want checks and 0", got.Group, got.Shards)
	}
	if got := cfg.Steps[1]; got.Shards != 8 || got.Group != "" {
		t.Fatalf("e2e = group %q shards %d, want no group and 8", got.Group, got.Shards)
	}
	for _, n := range []string{"-1", "33"} {
		_, err := Parse([]byte("suites:\n  - name: e2e\n    command: go test\n    shards: " + n + "\n"))
		if err == nil || !strings.Contains(err.Error(), "shards") {
			t.Fatalf("shards %s: err %v, want a refusal naming shards", n, err)
		}
	}
}

func TestShardsAreNotHashed(t *testing.T) {
	plain := "suites:\n  - name: e2e\n    kind: integration\n    command: go test ./e2e\n"
	sharded := plain + "    shards: 4\n    group: e2e\n"
	a, err := SuiteHash([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	b, err := SuiteHash([]byte(sharded))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("shards and group changed the suite hash: %s vs %s", a, b)
	}
}

// A runner with no lanes runs a sharded step's shards one after another
// in itself, each told its index and the total.
func TestRunShardsInOneRunnerOneAfterAnother(t *testing.T) {
	dir := t.TempDir()
	steps := []Step{{Name: "e2e", Command: `echo "shard $MESH_SHARD_INDEX of $MESH_SHARD_TOTAL"`, Timeout: time.Minute, Shards: 3}}
	results, err := Run(context.Background(), steps, HostRunner{Dir: dir}, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want one merged step", len(results))
	}
	r := results[0]
	if r.ExitCode != 0 || len(r.Shards) != 3 {
		t.Fatalf("exit %d with %d shards, want 0 and 3", r.ExitCode, len(r.Shards))
	}
	for k, s := range r.Shards {
		if want := fmt.Sprintf("shard %d of 3", k); !strings.Contains(s.Output, want) {
			t.Fatalf("shard %d output %q, want %q", k, s.Output, want)
		}
	}
	if !strings.Contains(r.Output, "shard 3/3: exit 0") {
		t.Fatalf("merged output %q lacks a line per shard", r.Output)
	}
}

// laneRunner is a LaneRunner over HostRunner that counts the lanes it
// made and released and holds every command until want of them run at
// once, so a test proves they ran beside each other.
type laneRunner struct {
	HostRunner
	mu       sync.Mutex
	made     []string
	released int
	failLane string
	running  int
	peak     int
	want     int
	all      chan struct{}
}

type heldRunner struct {
	r    *laneRunner
	next Runner
}

func (h heldRunner) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	h.r.mu.Lock()
	h.r.running++
	h.r.peak = max(h.r.peak, h.r.running)
	if h.r.running == h.r.want {
		close(h.r.all)
	}
	h.r.mu.Unlock()
	select {
	case <-h.r.all:
	case <-time.After(5 * time.Second):
	}
	defer func() { h.r.mu.Lock(); h.r.running--; h.r.mu.Unlock() }()
	return h.next.Run(ctx, cmd, timeout, stdout, stderr)
}

func (l *laneRunner) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	return heldRunner{r: l, next: l.HostRunner}.Run(ctx, cmd, timeout, stdout, stderr)
}

func (l *laneRunner) Lane(ctx context.Context, name string) (Runner, func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if name == l.failLane {
		return nil, nil, errors.New("no pod")
	}
	l.made = append(l.made, name)
	return heldRunner{r: l, next: l.HostRunner}, func() { l.mu.Lock(); l.released++; l.mu.Unlock() }, nil
}

// A LaneRunner runs each group and each shard beside the others: the
// first lane in the runner itself, the rest in lanes it makes and
// releases, and a failing shard fails its step.
func TestRunLanesRunGroupsAndShardsAtOnce(t *testing.T) {
	dir := t.TempDir()
	steps := []Step{
		{Name: "lint", Kind: KindLint, Command: "true", Timeout: time.Minute},
		{Name: "scan", Kind: KindLint, Command: "true", Timeout: time.Minute, Group: "checks"},
		{Name: "e2e", Command: `test "$MESH_SHARD_INDEX" != 2`, Timeout: time.Minute, Shards: 3},
	}
	lr := &laneRunner{HostRunner: HostRunner{Dir: dir}, want: 5, all: make(chan struct{})}
	results, err := Run(context.Background(), steps, lr, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if lr.peak != 5 {
		t.Fatalf("peak running = %d, want all five lanes at once", lr.peak)
	}
	// lint names no group while scan does, so lint is in its kind's
	wantLanes := []string{"checks", "e2e-0", "e2e-1", "e2e-2"}
	slices.Sort(lr.made)
	if !slices.Equal(lr.made, wantLanes) || lr.released != 4 {
		t.Fatalf("lanes made %v released %d, want %v released 4", lr.made, lr.released, wantLanes)
	}
	names := []string{}
	for _, r := range results {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"lint", "scan", "e2e"}) {
		t.Fatalf("results %v, want the file's order", names)
	}
	e2e := results[2]
	if e2e.ExitCode != 1 || !strings.Contains(e2e.Output, "shard 3/3 failed") {
		t.Fatalf("e2e exit %d output %q, want shard 3/3's failure", e2e.ExitCode, e2e.Output)
	}
	if e2e.DurationMs != max(e2e.Shards[0].DurationMs, e2e.Shards[1].DurationMs, e2e.Shards[2].DurationMs) {
		t.Fatalf("e2e duration %dms is not the longest shard's", e2e.DurationMs)
	}
}

// A lane that cannot be made is the run's error, and its shard never ran,
// so its step is no pass.
func TestRunALaneThatCannotOpenFailsTheStep(t *testing.T) {
	dir := t.TempDir()
	steps := []Step{{Name: "e2e", Command: "true", Timeout: time.Minute, Shards: 2}}
	lr := &laneRunner{HostRunner: HostRunner{Dir: dir}, want: 1, all: make(chan struct{}), failLane: "e2e-1"}
	results, err := Run(context.Background(), steps, lr, Options{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "lane e2e-1") {
		t.Fatalf("err %v, want the lane's", err)
	}
	if len(results) != 1 || results[0].ExitCode == 0 {
		t.Fatalf("results %+v, want e2e not passed", results)
	}
}

// A failing shard's redacted tail is its merged step's: the node logs
// the shard that failed, not the shards' summary lines alone.
func TestAFailingShardsTailIsTheSteps(t *testing.T) {
	dir := t.TempDir()
	steps := []Step{{Name: "e2e", Command: `echo "shard $MESH_SHARD_INDEX ghs_abcdefghijklmnopqrstuvwxyz0123"; test "$MESH_SHARD_INDEX" != 1`, Timeout: time.Minute, Shards: 2}}
	lr := &laneRunner{HostRunner: HostRunner{Dir: dir}, want: 2, all: make(chan struct{})}
	results, err := Run(context.Background(), steps, lr, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	e2e := results[0]
	if e2e.ExitCode != 1 || e2e.Tail != "shard 1 [redacted]" {
		t.Fatalf("e2e exit %d tail %q, want shard 2/2's redacted tail", e2e.ExitCode, e2e.Tail)
	}
	if strings.Contains(e2e.Output, "ghs_") {
		t.Fatalf("a token in the output: %q", e2e.Output)
	}
}
