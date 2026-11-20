package nodetools

import (
	"strings"
	"testing"

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
	s.noteCoverage(steps, results)
	if len(heard) != 1 || heard[0] != `{"tests":{}}` {
		t.Fatalf("heard %q", heard)
	}
	if len(logged) != 2 {
		t.Fatalf("logged %d skips, want 2 (big, bad)", len(logged))
	}
}
