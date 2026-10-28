package validate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What `go test ./e2e -json` printed on dl360 (runsheet/runsheet#5663,
// 2026-10-29), a slice per shard: start/run/output events, a test's
// "--- PASS:" line as an Output event (not a pass of its own), a
// subtest, a skip, a package-level result with no Test, and a line that
// is not JSON at all.
var shardJSON = []string{
	`{"Time":"2026-10-29T03:01:00Z","Action":"start","Package":"github.com/runsheet/runsheet/e2e"}
{"Time":"2026-10-29T03:01:00Z","Action":"run","Package":"github.com/runsheet/runsheet/e2e","Test":"TestGateway"}
{"Time":"2026-10-29T03:01:00Z","Action":"output","Package":"github.com/runsheet/runsheet/e2e","Test":"TestGateway","Output":"=== RUN   TestGateway\n"}
{"Time":"2026-10-29T03:04:47Z","Action":"output","Package":"github.com/runsheet/runsheet/e2e","Test":"TestGateway","Output":"--- PASS: TestGateway (227.41s)\n"}
{"Time":"2026-10-29T03:04:47Z","Action":"pass","Package":"github.com/runsheet/runsheet/e2e","Test":"TestGateway","Elapsed":227.41}
{"Time":"2026-10-29T03:04:47Z","Action":"run","Package":"github.com/runsheet/runsheet/e2e","Test":"TestContracts"}
{"Time":"2026-10-29T03:04:47Z","Action":"run","Package":"github.com/runsheet/runsheet/e2e","Test":"TestContracts/reads"}
{"Time":"2026-10-29T03:05:01Z","Action":"output","Package":"github.com/runsheet/runsheet/e2e","Test":"TestContracts/reads","Output":"    --- PASS: TestContracts/reads (14.02s)\n"}
{"Time":"2026-10-29T03:05:01Z","Action":"pass","Package":"github.com/runsheet/runsheet/e2e","Test":"TestContracts/reads","Elapsed":14.02}
{"Time":"2026-10-29T03:05:01Z","Action":"pass","Package":"github.com/runsheet/runsheet/e2e","Test":"TestContracts","Elapsed":14.02}
{"Time":"2026-10-29T03:05:01Z","Action":"output","Package":"github.com/runsheet/runsheet/e2e","Output":"PASS\n"}
{"Time":"2026-10-29T03:05:01Z","Action":"pass","Package":"github.com/runsheet/runsheet/e2e","Elapsed":241.1}
`,
	`go: downloading github.com/jackc/pgx/v5 v5.7.1
{"Time":"2026-10-29T03:01:00Z","Action":"run","Package":"github.com/runsheet/runsheet/e2e","Test":"TestLoomBumpLedger"}
{"Time":"2026-10-29T03:01:00Z","Action":"run","Package":"github.com/runsheet/runsheet/e2e","Test":"TestRunbooks"}
{"Time":"2026-10-29T03:01:02Z","Action":"output","Package":"github.com/runsheet/runsheet/e2e","Test":"TestRunbooks","Output":"--- SKIP: TestRunbooks (0.00s)\n"}
{"Time":"2026-10-29T03:01:02Z","Action":"skip","Package":"github.com/runsheet/runsheet/e2e","Test":"TestRunbooks","Elapsed":0}
{"Time":"2026-10-29T03:59:25Z","Action":"output","Package":"github.com/runsheet/runsheet/e2e","Test":"TestLoomBumpLedger","Output":"--- FAIL: TestLoomBumpLedger (3564.95s)\n"}
{"Time":"2026-10-29T03:59:25Z","Action":"fail","Package":"github.com/runsheet/runsheet/e2e","Test":"TestLoomBumpLedger","Elapsed":3564.95}
{"Time":"2026-10-29T03:59:25Z","Action":"output","Package":"github.com/runsheet/runsheet/e2e","Output":"FAIL\n"}
{"Time":"2026-10-29T03:59:25Z","Action":"fail","Package":"github.com/runsheet/runsheet/e2e","Elapsed":3565.2}
`,
}

// A sharded suite running `go test -json` with no results: line — as
// runsheet's integration does — counts each shard's tests from its own
// stdout, and the step's are their sum; the report stays its reports:.
func TestShardsCountGoTestJSONOnStdoutWithNoResultsLine(t *testing.T) {
	dir := t.TempDir()
	for k, s := range shardJSON {
		if err := os.WriteFile(filepath.Join(dir, "shard"+string(rune('0'+k))+".json"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "integration.json"), []byte("report"), 0o644); err != nil {
		t.Fatal(err)
	}
	steps := []Step{{
		Name:    "integration",
		Kind:    KindIntegration,
		Command: `cat "shard$MESH_SHARD_INDEX.json"; ! grep -q '"Action":"fail"' "shard$MESH_SHARD_INDEX.json"`,
		Timeout: time.Minute,
		Shards:  2,
		Reports: []string{"integration.json"},
	}}
	lr := &laneRunner{HostRunner: HostRunner{Dir: dir}, want: 2, all: make(chan struct{})}
	results, err := Run(context.Background(), steps, lr, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	want := []struct{ run, passed, failed int }{{3, 3, 0}, {2, 0, 1}}
	for k, s := range r.Shards {
		if s.TestsRun != want[k].run || s.TestsPassed != want[k].passed || s.TestsFailed != want[k].failed {
			t.Fatalf("shard %d counted %d run %d passed %d failed, want %+v", k+1, s.TestsRun, s.TestsPassed, s.TestsFailed, want[k])
		}
	}
	if r.TestsRun != 5 || r.TestsPassed != 3 || r.TestsFailed != 1 {
		t.Fatalf("step counted %d run %d passed %d failed, want 5, 3, 1", r.TestsRun, r.TestsPassed, r.TestsFailed)
	}
	if r.ExitCode == 0 {
		t.Fatal("step passed though shard 2 failed")
	}
	for _, line := range []string{"shard 1/2: exit 0 in", "3 tests (0 failed)", "shard 2/2: exit 1 in", "2 tests (1 failed)"} {
		if !strings.Contains(r.Output, line) {
			t.Fatalf("output %q lacks %q", r.Output, line)
		}
	}
	if r.Report != "reportreport" {
		t.Fatalf("report %q, want each shard's integration.json", r.Report)
	}
	// the counts are the attestation's, not the log line's alone
	if err := os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte("suites:\n  - name: integration\n    command: go test ./e2e -json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	att, err := BuildWithTree(dir, "095fc8cd", "tree", results, Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if s := att.Steps[0]; s.TestsRun != 5 || s.TestsPassed != 3 || s.TestsFailed != 1 {
		t.Fatalf("attestation step counted %d run %d passed %d failed, want 5, 3, 1", s.TestsRun, s.TestsPassed, s.TestsFailed)
	}
}

// A step with no results: whose output is not go test -json counts none.
func TestNoResultsLineAndNoJSONCountsNoTests(t *testing.T) {
	dir := t.TempDir()
	steps := []Step{{Name: "lint", Kind: KindLint, Command: `echo 'ok  	github.com/x/y	0.1s'; echo '{"Action":"pass"}'`, Timeout: time.Minute}}
	results, err := Run(context.Background(), steps, HostRunner{Dir: dir}, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.TestsRun != 0 || r.ExitCode != 0 {
		t.Fatalf("lint counted %d tests exit %d, want 0 and 0", r.TestsRun, r.ExitCode)
	}
}
