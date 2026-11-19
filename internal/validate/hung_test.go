package validate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const panicOutput = `=== RUN   TestOther
panic: test timed out after 8m0s
	running tests:
		TestProtectedPaths (7m53s)
		TestProtectedPaths/a_sub (7m50s)
		TestRunsheetDeploy (7m52s)

goroutine 1 [running]:
testing.(*M).startAlarm.func1()
`

func TestHungTestsReadsTheTimeoutPanic(t *testing.T) {
	names, hung := hungTests([]byte(panicOutput))
	if !hung || strings.Join(names, ",") != "TestProtectedPaths,TestRunsheetDeploy" {
		t.Fatalf("hungTests = %v, %v", names, hung)
	}
	// go test -json carries the panic as output events
	var js bytes.Buffer
	for _, l := range strings.SplitAfter(panicOutput, "\n") {
		if l == "" {
			continue
		}
		fmt.Fprintf(&js, "{\"Action\":\"output\",\"Package\":\"p\",\"Output\":%q}\n", l)
	}
	names, hung = hungTests(js.Bytes())
	if !hung || strings.Join(names, ",") != "TestProtectedPaths,TestRunsheetDeploy" {
		t.Fatalf("hungTests(json) = %v, %v", names, hung)
	}
	if _, hung := hungTests([]byte("--- FAIL: TestX\nFAIL\n")); hung {
		t.Fatal("a plain failure read as a hang")
	}
}

func TestCapLogKeepsTheTail(t *testing.T) {
	data := bytes.Repeat([]byte("x"), MaxLogBytes+1000)
	copy(data[len(data)-4:], "END!")
	got := capLog(data)
	if len(got) != MaxLogBytes || !bytes.HasSuffix(got, []byte("END!")) {
		t.Fatalf("capLog: %d bytes, suffix %q", len(got), got[len(got)-4:])
	}
}

// scriptLanes answers each shard's runs from a script: run n of shard k
// prints script[k][n] (the last entry again past its end) and exits 1
// when it holds a panic or FAIL.
type scriptLanes struct {
	mu     sync.Mutex
	script map[string][]string
	calls  map[string]int
	lanes  []string
}

func (s *scriptLanes) Run(ctx context.Context, cmd string, _ time.Duration, stdout, _ io.Writer) (int, error) {
	shard := "0"
	if i := strings.Index(cmd, ShardIndexEnv+"="); i >= 0 {
		shard = strings.Fields(cmd[i+len(ShardIndexEnv)+1:])[0]
	}
	s.mu.Lock()
	n := s.calls[shard]
	s.calls[shard]++
	runs := s.script[shard]
	s.mu.Unlock()
	out := runs[min(n, len(runs)-1)]
	io.WriteString(stdout, out)
	if strings.Contains(out, "panic:") || strings.Contains(out, "FAIL") {
		return 1, nil
	}
	return 0, nil
}

func (s *scriptLanes) Lane(_ context.Context, name string) (Runner, func(), error) {
	s.mu.Lock()
	s.lanes = append(s.lanes, name)
	s.mu.Unlock()
	return s, func() {}, nil
}

func TestAHungShardIsRerunOnceInAFreshLane(t *testing.T) {
	steps := []Step{{Name: "integration", Command: "true", Shards: 2, Retries: 1}}
	for _, tc := range []struct {
		name     string
		second   string
		exit     int
		hung     bool
		rerunHas string
		wantLogs int
	}{
		{"passes the second time", "ok\n", 0, false, "hung once (TestProtectedPaths, TestRunsheetDeploy), then re-run in a fresh lane: exit 0", 1},
		{"hangs again", panicOutput, 1, true, "hung twice", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptLanes{calls: map[string]int{}, script: map[string][]string{"0": {"ok\n"}, "1": {panicOutput, tc.second}}}
			res, err := Run(context.Background(), steps, s, Options{Dir: t.TempDir(), ContinueOnFailure: true})
			if err != nil {
				t.Fatal(err)
			}
			r := res[0]
			if r.ExitCode != tc.exit || r.Hung != tc.hung {
				t.Fatalf("exit %d hung %v, want %d %v (output %q)", r.ExitCode, r.Hung, tc.exit, tc.hung, r.Output)
			}
			sh := r.Shards[1]
			if !strings.Contains(sh.Rerun, tc.rerunHas) {
				t.Fatalf("shard 2's rerun %q lacks %q", sh.Rerun, tc.rerunHas)
			}
			if s.calls["1"] != 2 {
				t.Fatalf("shard 2 ran %d times, want 2 (once, then once in a fresh lane)", s.calls["1"])
			}
			if len(s.lanes) != 2 || s.lanes[1] != "integration-1" {
				t.Fatalf("lanes opened %v, want integration-1 and a fresh integration-1", s.lanes)
			}
			if len(sh.Logs) != tc.wantLogs || !strings.Contains(string(sh.Logs[0].Data), "goroutine 1 [running]") {
				t.Fatalf("shard 2 kept %d logs, want %d holding the dump", len(sh.Logs), tc.wantLogs)
			}
			if tc.hung && strings.Join(r.HungTests, ",") != "TestProtectedPaths,TestRunsheetDeploy" {
				t.Fatalf("hung tests %v", r.HungTests)
			}
		})
	}
}
