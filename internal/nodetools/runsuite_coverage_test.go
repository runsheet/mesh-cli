package nodetools

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

func TestNoteCoverageHearsTheCoverageStepsReport(t *testing.T) {
	var heard []string
	var logged []string
	s := &RunSuite{OnCoverage: func(c string) { heard = append(heard, c) },
		Logf: func(f string, a ...any) { logged = append(logged, f) }}
	steps := []validate.Step{
		{Name: "e2ecoverage", Reports: []string{CoverageReport}},
		{Name: "integration", Reports: []string{"integration.json"}},
		{Name: "results", Results: "stdout", Reports: []string{CoverageReport}},
		{Name: "big", Reports: []string{CoverageReport}},
		{Name: "bad", Reports: []string{CoverageReport}},
		{Name: "none", Reports: []string{CoverageReport}},
	}
	results := []validate.StepResult{
		{Name: "e2ecoverage", Report: ` {"tests":{}} ` + "\n"},
		{Name: "integration", Report: `{"x":1}`},
		{Name: "results", Report: `{"x":2}`},
		{Name: "big", Report: `"` + strings.Repeat("a", MaxCoverage) + `"`},
		{Name: "bad", Report: `{"cut`},
		{Name: "none"},
	}
	s.noteCoverage(steps, results, time.Now())
	if len(heard) != 1 || heard[0] != `{"tests":{}}` {
		t.Fatalf("heard %q", heard)
	}
	skips := 0
	for _, l := range logged {
		if strings.Contains(l, "not carried") {
			skips++
		}
	}
	if skips != 2 {
		t.Fatalf("logged %d skips, want 2 (big, bad): %q", skips, logged)
	}
}

// runsheet's e2ecoverage suite writes its map encoded (platform/coverage
// Encode: "gz64:" and base64 of gzip), not JSON, and on main's validation
// it runs after suites whose reports use up the run's report bound, so
// its step's Report is cut. The node reads the file whole from the
// checkout and carries the encoded map as written (dl360, 2026-11-23:
// every main validation logged "not carried: it is not JSON", runsheet
// never had a map, and every validation ran all four e2e shards).
func TestNoteCoverageCarriesTheEncodedMapWhole(t *testing.T) {
	ws := t.TempDir()
	encoded := "gz64:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("gzip bytes ", 6000)))
	start := time.Now().Add(-time.Second)
	if err := os.WriteFile(filepath.Join(ws, CoverageReport), []byte(encoded+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var heard []string
	var logged []string
	s := &RunSuite{Workspace: ws, OnCoverage: func(c string) { heard = append(heard, c) },
		Logf: func(f string, a ...any) { logged = append(logged, f) }}
	steps := []validate.Step{{Name: "e2ecoverage", Reports: []string{CoverageReport}}}
	// the Report as the bound left it: cut mid-way
	cut := []validate.StepResult{{Name: "e2ecoverage", Report: encoded[:1000]}}
	s.noteCoverage(steps, cut, start)
	if len(heard) != 1 || heard[0] != encoded {
		t.Fatalf("heard %d maps (%d bytes), want the whole encoded map of %d bytes; logged %q", len(heard), len(strings.Join(heard, "")), len(encoded), logged)
	}

	// a file an earlier run left, older than this run, is not this run's map
	heard = nil
	s.noteCoverage(steps, []validate.StepResult{{Name: "e2ecoverage"}}, time.Now().Add(time.Hour))
	if len(heard) != 0 {
		t.Fatalf("carried a map from before this run: %d bytes", len(heard[0]))
	}

	// a cut encoded map, with no file, is never carried
	heard, logged = nil, nil
	s.Workspace = t.TempDir()
	s.noteCoverage(steps, []validate.StepResult{{Name: "e2ecoverage", Report: "gz64:abc"}}, start)
	if len(heard) != 0 || len(logged) != 1 {
		t.Fatalf("a cut encoded map: heard %q, logged %q", heard, logged)
	}
}
