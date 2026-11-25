package nodev1

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// meshBodyLimit is Mesh's limit on a finish's body.
const meshBodyLimit = 1 << 20

// manyPassingEvents is go test's events for n passing tests.
func manyPassingEvents(n int) string {
	var sb strings.Builder
	for i := range n {
		fmt.Fprintf(&sb, `{"Action":"run","Package":"example.com/platform/some/longer/package","Test":"TestAPassingCaseNumber%05d"}`+"\n", i)
		fmt.Fprintf(&sb, `{"Action":"pass","Package":"example.com/platform/some/longer/package","Test":"TestAPassingCaseNumber%05d","Elapsed":0.01}`+"\n", i)
	}
	sb.WriteString(`{"Action":"pass","Package":"example.com/platform/some/longer/package"}` + "\n")
	return sb.String()
}

// A validation whose suites pass 10,000 tests finishes under Mesh's 1 MB
// body limit (2026-11-26: runsheet's 9,500-test chain validation's
// finish was refused, "the body is over 1048576 bytes", and its result
// lost): the passing tests are counted in their suite, never listed, and
// the finish still carries the candidate's sha and tree and every
// evidence id the lease's tools recorded.
func TestATenThousandTestValidationFinishesUnderTheBodyLimit(t *testing.T) {
	sess := &stubSession{result: SessionResult{SessionID: "unused"}}
	g := newRig(t, sess)
	suites := "suites:\n  - name: unit\n    kind: unit\n    results: stdout\n    command: cat events.json\n"
	lease := runChain(t, g, map[string]string{".runsheet/validate.yml": suites, "events.json": manyPassingEvents(10000)})
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q; fails %+v", lease, g.fake.Ended(lease), g.fake.Calls("fail"))
	}
	call := g.fake.Calls("finish")[0]
	if len(call.Body) >= meshBodyLimit {
		t.Fatalf("the finish's body is %d bytes, at or over Mesh's limit of %d", len(call.Body), meshBodyLimit)
	}
	var fr meshapi.FinishRequest
	if err := json.Unmarshal(call.Body, &fr); err != nil {
		t.Fatal(err)
	}
	finish := finishOf(t, g)
	if finish["green"] != true {
		t.Fatalf("green %v", finish["green"])
	}
	candidate, _ := finish["candidate"].(map[string]any)
	if sha, _ := candidate["sha"].(string); sha == "" || sha != g.github.ref("refs/heads/mesh/batch/candidate-1") {
		t.Fatalf("candidate %+v", candidate)
	}
	if tree, _ := candidate["tree"].(string); tree == "" {
		t.Fatalf("candidate %+v names no tree", candidate)
	}
	var ids []string
	for _, e := range finish["evidence"].([]any) {
		ids = append(ids, e.(string))
	}
	if len(fr.Evidence) < 3 || !slices.Equal(ids, fr.Evidence) {
		t.Fatalf("the result's evidence %v, the lease recorded %v", ids, fr.Evidence)
	}
	st := finish["suites"].([]any)[0].(map[string]any)
	if st["passed"] != float64(10000) {
		t.Fatalf("suite %+v, want 10000 passed", st)
	}
}

// A red with more failing tests than fit is cut, never refused: the
// failing tests' entries and names are halved until the result fits,
// the triage's note says how many were left out, and green, candidate,
// suites and evidence are untouched.
func TestBoundFinishCutsFailingTestsWithAMarker(t *testing.T) {
	var tests []any
	var failing []string
	for i := range 20000 {
		name := fmt.Sprintf("TestAFailingCaseNumber%05d", i)
		tests = append(tests, map[string]any{"name": name, "package": "example.com/pkg", "outcome": "fail", "duration_ms": 1})
		failing = append(failing, "example.com/pkg "+name)
	}
	finish := map[string]any{"green": false, "evidence": []string{"e1", "e2"}, "candidate": map[string]any{"sha": "abc", "tree": "def"},
		"suites": []any{map[string]any{"name": "unit", "exit": 1, "passed": 0, "failed": 20000}}, "tests": tests,
		"triage": map[string]any{"class": "code", "failing_tests": failing, "note": "1 of 1 steps failed"}, "learnings": []any{}}
	raw, err := boundFinish(finish, maxMachineFinish)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxMachineFinish {
		t.Fatalf("%d bytes, over %d", len(raw), maxMachineFinish)
	}
	var got map[string]any
	json.Unmarshal(raw, &got)
	note := got["triage"].(map[string]any)["note"].(string)
	if !strings.Contains(note, "left out to keep the finish under Mesh's 1 MB body limit") || !strings.HasSuffix(note, "1 of 1 steps failed") {
		t.Fatalf("note %q", note)
	}
	if got["candidate"].(map[string]any)["tree"] != "def" || len(got["evidence"].([]any)) != 2 || got["green"] != false {
		t.Fatalf("a bound key was cut: %v", got["candidate"])
	}
	if n := len(got["tests"].([]any)); n == 0 || n == 20000 {
		t.Fatalf("tests %d", n)
	}
	if len(tests) != 20000 || len(finish["triage"].(map[string]any)["failing_tests"].([]string)) != 20000 {
		t.Fatal("boundFinish changed its caller's finish")
	}
}

// The finish's last guard: a result over the body limit has its tests
// and then its coverage left out rather than being refused.
func TestWithinBodyLimitLeavesOutTestsThenCoverage(t *testing.T) {
	big := strings.Repeat("x", maxFinishResult)
	raw, _ := json.Marshal(map[string]any{"green": true, "evidence": []string{"e"}, "tests": []string{big}, "coverage": "small"})
	var said []string
	out := withinBodyLimit(raw, func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) })
	var got map[string]any
	json.Unmarshal(out, &got)
	if _, ok := got["tests"]; ok || got["coverage"] != "small" || got["green"] != true || len(said) != 1 {
		t.Fatalf("got %v; said %v", got, said)
	}
	small, _ := json.Marshal(map[string]any{"green": true})
	if string(withinBodyLimit(small, t.Logf)) != string(small) {
		t.Fatal("a result under the limit was changed")
	}
}
