package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
)

// streamingRunner streams ten tests' go-test-json events to stdout, one
// test at a time, recording each write so the test can check OnTest
// fired before the later writes happened — never by sleeping.
type streamingRunner struct {
	record func(string)
}

func (r streamingRunner) Run(_ context.Context, _ string, _ time.Duration, stdout, _ io.Writer) (int, error) {
	write := func(action, test, output string, elapsed float64) {
		line, _ := json.Marshal(struct {
			Action  string  `json:"Action"`
			Package string  `json:"Package"`
			Test    string  `json:"Test"`
			Output  string  `json:"Output"`
			Elapsed float64 `json:"Elapsed"`
		}{action, "pkg", test, output, elapsed})
		stdout.Write(append(line, '\n'))
	}
	for i := 1; i <= 10; i++ {
		name := fmt.Sprintf("Test%d", i)
		write("run", name, "", 0)
		if i == 3 {
			write("output", name, "it broke\n", 0)
			write("fail", name, "", 0.02)
		} else {
			write("pass", name, "", 0.01)
		}
		r.record(fmt.Sprintf("wrote-%s", name))
	}
	return 0, nil
}

// OnTest fires with a test's result the instant that test's stream ends
// — here, the third test's fail — strictly before the run goes on to
// write the tenth test's events, let alone returns: a caller watching a
// long suite sees a failure as it happens, not only at the end.
func TestOnTestFiresBeforeTheStepEnds(t *testing.T) {
	var mu sync.Mutex
	var log []string
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		log = append(log, s)
	}

	var thirdFail TestResult
	onTest := func(tr TestResult) {
		record("ontest-" + tr.Name)
		if tr.Name == "Test3" {
			thirdFail = tr
		}
	}

	steps := []Step{{Name: "unit", Command: "ignored", Timeout: time.Minute}}
	results, err := Run(context.Background(), steps, streamingRunner{record: record}, Options{OnTest: onTest})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if len(results) != 1 || results[0].TestsRun != 10 || results[0].TestsFailed != 1 {
		t.Fatalf("results %+v, want 10 run, 1 failed", results)
	}

	if thirdFail.Action != "fail" || thirdFail.Output != "it broke\n" {
		t.Fatalf("OnTest's Test3 = %+v, want a fail carrying its output", thirdFail)
	}

	onTestAt := slices.Index(log, "ontest-Test3")
	wroteTenAt := slices.Index(log, "wrote-Test10")
	if onTestAt < 0 || wroteTenAt < 0 {
		t.Fatalf("log %v missing ontest-Test3 or wrote-Test10", log)
	}
	if onTestAt >= wroteTenAt {
		t.Fatalf("OnTest(Test3) at %d, wrote-Test10 at %d: want OnTest to fire first", onTestAt, wroteTenAt)
	}
}
