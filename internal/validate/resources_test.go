package validate

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseReadsCPUsAndMemory(t *testing.T) {
	cfg, err := Parse([]byte(`suites:
  - name: lint
    kind: lint
    command: go vet ./...
  - name: integration
    kind: integration
    command: go test ./e2e
    cpus: 16
    memory: " 24g "
    shards: 4
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Steps[0].Resources(); !got.IsZero() {
		t.Fatalf("lint resources = %+v, want none of its own", got)
	}
	if got := cfg.Steps[1].Resources(); got != (Resources{CPUs: 16, Memory: "24g"}) {
		t.Fatalf("integration resources = %+v, want 16 cpus and 24g", got)
	}
	for _, sz := range []string{"512m", "1.5g", "2048", "24gb", "100b"} {
		if _, err := Parse([]byte("suites:\n  - name: e2e\n    command: go test\n    memory: " + sz + "\n")); err != nil {
			t.Errorf("memory %s: %v", sz, err)
		}
	}
	for doc, field := range map[string]string{
		"suites:\n  - name: e2e\n    command: go test\n    cpus: -1\n":      "cpus",
		"suites:\n  - name: e2e\n    command: go test\n    memory: lots\n":  "memory",
		"suites:\n  - name: e2e\n    command: go test\n    memory: 24gig\n": "memory",
		"suites:\n  - name: e2e\n    command: go test\n    cpus: sixteen\n": "",
	} {
		_, err := Parse([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%q: err %v, want a refusal naming %q", doc, err, field)
		}
	}
}

// How much room a suite runs with is not what it runs: cpus and memory
// leave the suite hash as it was.
func TestCPUsAndMemoryAreNotHashed(t *testing.T) {
	plain := "suites:\n  - name: e2e\n    kind: integration\n    command: go test ./e2e\n"
	a, err := SuiteHash([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	b, err := SuiteHash([]byte(plain + "    cpus: 16\n    memory: 24g\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("cpus and memory changed the suite hash: %s vs %s", a, b)
	}
}

// resourceRunner records the Resources each command ran with.
type resourceRunner struct {
	mu  sync.Mutex
	got map[string][]Resources
}

func (r *resourceRunner) Run(ctx context.Context, cmd string, _ time.Duration, _, _ io.Writer) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = map[string][]Resources{}
	}
	key := cmd
	if i := strings.LastIndex(cmd, "; "); i >= 0 {
		key = cmd[i+2:]
	}
	r.got[key] = append(r.got[key], ResourcesOf(ctx))
	return 0, nil
}

type resourceLanes struct{ *resourceRunner }

func (l resourceLanes) Lane(context.Context, string) (Runner, func(), error) {
	return l.resourceRunner, func() {}, nil
}

// Each step's runner is told the step's own cpus and memory, every
// shard alike, in one runner or in lanes; a step naming neither tells it
// nothing.
func TestRunTellsTheRunnerEachStepsResources(t *testing.T) {
	steps := []Step{
		{Name: "lint", Command: "lint", Timeout: time.Minute},
		{Name: "e2e", Command: "e2e", Timeout: time.Minute, Shards: 3, CPUs: 16, Memory: "24g"},
	}
	want := Resources{CPUs: 16, Memory: "24g"}
	for name, runner := range map[string]func(*resourceRunner) Runner{
		"one runner": func(r *resourceRunner) Runner { return r },
		"lanes":      func(r *resourceRunner) Runner { return resourceLanes{r} },
	} {
		rr := &resourceRunner{}
		if _, err := Run(context.Background(), steps, runner(rr), Options{Dir: t.TempDir()}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := rr.got["lint"]; len(got) != 1 || !got[0].IsZero() {
			t.Fatalf("%s: lint ran with %+v, want the node's", name, got)
		}
		got := rr.got["e2e"]
		if len(got) != 3 {
			t.Fatalf("%s: e2e ran %d times, want 3 shards", name, len(got))
		}
		for k, r := range got {
			if r != want {
				t.Fatalf("%s: shard %d ran with %+v, want %+v", name, k, r, want)
			}
		}
	}
}
