package validate

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
)

// retryKey is a test's key into a unit's retried outcomes: its package
// and name, as TestResult carries them.
func retryKey(pkg, name string) string { return pkg + "\x00" + name }

// retryCommand is a step's Retry template with "{package}" and "{test}"
// replaced by one failing test's own package and name — as "{shard}" is
// replaced in a step's Results and Reports.
func retryCommand(tmpl, pkg, test string) string {
	cmd := strings.ReplaceAll(tmpl, "{package}", pkg)
	cmd = strings.ReplaceAll(cmd, "{test}", test)
	return cmd
}

// shardSupervisor watches one unit's own go-test-json stream (through
// Options.OnTest, which it stands in for) for a failing test and, since
// its step names a Retry command, retries that test by name alone on
// the same runner — never the whole unit — while the unit's own run is
// still going. A retry that passes marks the test flaky: the unit runs
// on untouched. A retry that fails too confirms it and, unless the step
// runs to its end (Step.RunToEnd), stops the unit (the stop func the
// unit was given for Options.OnLane), naming the test, so whatever the
// unit would have streamed after the failure never runs.
type shardSupervisor struct {
	step   Step
	runner Runner
	ctx    context.Context
	stop   func(reason string)
	onTest func(TestResult)

	mu       sync.Mutex
	outcomeM map[string]TestResult
}

func newShardSupervisor(ctx context.Context, step Step, runner Runner, stop func(string), onTest func(TestResult)) *shardSupervisor {
	return &shardSupervisor{step: step, runner: runner, ctx: ctx, stop: stop, onTest: onTest, outcomeM: map[string]TestResult{}}
}

// onStream is the unit's own Options.OnTest, forwarding to the caller's
// first, then retrying a failing test by name and deciding the unit's
// fate on the retry's own outcome.
func (s *shardSupervisor) onStream(tr TestResult) {
	if s.onTest != nil {
		s.onTest(tr)
	}
	if tr.Action != "fail" {
		return
	}
	retry := s.runRetry(tr)
	s.mu.Lock()
	s.outcomeM[retryKey(tr.Package, tr.Name)] = retry
	s.mu.Unlock()
	if retry.Action != "pass" && !s.step.RunToEnd {
		s.stop(fmt.Sprintf("%s failed again on retry", tr.Name))
	}
}

// runRetry runs the test's by-name retry command to completion on the
// unit's own runner and reads its own outcome from its go-test-json
// stream; a retry whose stream never named the test, or whose stream
// could not be read, reads as a failure (confirmed, not flaky).
func (s *shardSupervisor) runRetry(tr TestResult) TestResult {
	cmd := retryCommand(s.step.Retry, tr.Package, tr.Name)
	var stdout, stderr bytes.Buffer
	_, _ = s.runner.Run(s.ctx, cmd, s.step.Timeout, &stdout, &stderr)
	_, _, _, tests, err := ParseGoTestJSONTests(bytes.NewReader(stdout.Bytes()))
	if err == nil {
		for _, t := range tests {
			if t.Name == tr.Name && t.Package == tr.Package {
				return t
			}
		}
	}
	return TestResult{Name: tr.Name, Package: tr.Package, Action: "fail"}
}

// outcomes is every test this supervisor retried, by retryKey.
func (s *shardSupervisor) outcomes() map[string]TestResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]TestResult, len(s.outcomeM))
	for k, v := range s.outcomeM {
		out[k] = v
	}
	return out
}
