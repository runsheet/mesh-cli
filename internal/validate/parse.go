package validate

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"time"
)

// TestResult is one test's outcome, read from a go test -json stream:
// Action is pass, fail, skip, or timeout for a test the stream never
// closed — still "run" when the step was killed at its timeout.
// ElapsedMs is the terminating event's own elapsed time, or, for a
// timeout, the stream's last event's time less the test's "run" event's
// (never zero, so a timed-out test is told apart from one that genuinely
// ran for 0ms). Output is the test's own accumulated "output" events'
// text, kept only for a fail or a timeout — never for a pass or a skip,
// where it is never needed — so it survives a step's capped Report.
type TestResult struct {
	Name      string `json:"name"`
	Package   string `json:"package"`
	Action    string `json:"action"`
	ElapsedMs int64  `json:"elapsedMs"`
	Output    string `json:"output,omitempty"`
}

// ParseGoTestJSON counts `go test -json` events: each Action of pass,
// fail or skip that carries a Test name is one test; run is their total,
// passed and failed the pass and fail counts (skip counts toward run
// only). Lines that are not a test event, or not JSON at all, are
// ignored.
func ParseGoTestJSON(r io.Reader) (run, passed, failed int, err error) {
	run, passed, failed, _, err = ParseGoTestJSONTests(r)
	return run, passed, failed, err
}

// ParseGoTestJSONTests is ParseGoTestJSON's counts, plus every test's
// own TestResult, read from the same stream — the full, untruncated one
// a caller must give it: a test's Output and whether it is still
// running (Action "timeout") can only be read this way, since a step's
// Report may be cut by reportCap before it carries them.
func ParseGoTestJSONTests(r io.Reader) (run, passed, failed int, tests []TestResult, err error) {
	type pending struct {
		pkg, name string
		start     time.Time
		output    strings.Builder
	}
	open := map[string]*pending{}
	var order []string
	var last time.Time
	key := func(pkg, test string) string { return pkg + "\x00" + test }

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Time    time.Time `json:"Time"`
			Action  string    `json:"Action"`
			Package string    `json:"Package"`
			Test    string    `json:"Test"`
			Output  string    `json:"Output"`
			Elapsed float64   `json:"Elapsed"`
		}
		if jerr := json.Unmarshal(line, &ev); jerr != nil {
			continue
		}
		if ev.Time.After(last) {
			last = ev.Time
		}
		if ev.Test == "" {
			continue
		}
		k := key(ev.Package, ev.Test)
		switch ev.Action {
		case "run":
			if _, ok := open[k]; !ok {
				open[k] = &pending{pkg: ev.Package, name: ev.Test, start: ev.Time}
				order = append(order, k)
			}
		case "output":
			if p, ok := open[k]; ok {
				p.output.WriteString(ev.Output)
			}
		case "pass", "fail", "skip":
			run++
			if ev.Action == "pass" {
				passed++
			} else if ev.Action == "fail" {
				failed++
			}
			tr := TestResult{Name: ev.Test, Package: ev.Package, Action: ev.Action, ElapsedMs: int64(ev.Elapsed * 1000)}
			if p, ok := open[k]; ok {
				if ev.Action == "fail" {
					tr.Output = p.output.String()
				}
				delete(open, k)
			}
			tests = append(tests, tr)
		}
	}
	if sc.Err() != nil {
		return 0, 0, 0, nil, sc.Err()
	}
	for _, k := range order {
		p, ok := open[k]
		if !ok {
			continue
		}
		elapsed := last.Sub(p.start)
		if elapsed <= 0 {
			elapsed = time.Millisecond
		}
		tests = append(tests, TestResult{Name: p.name, Package: p.pkg, Action: "timeout", ElapsedMs: elapsed.Milliseconds(), Output: p.output.String()})
	}
	return run, passed, failed, tests, nil
}

// ParseJUnit sums the tests/failures/errors/skipped attributes of every
// <testsuite> element (whether it is the root or nested under
// <testsuites>): run is the tests total, failed is failures plus errors,
// passed is what's left.
func ParseJUnit(r io.Reader) (run, passed, failed int, err error) {
	dec := xml.NewDecoder(r)
	for {
		tok, terr := dec.Token()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return 0, 0, 0, terr
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "testsuite" {
			continue
		}
		var tests, failures, errs, skipped int
		for _, a := range se.Attr {
			switch a.Name.Local {
			case "tests":
				tests, _ = strconv.Atoi(a.Value)
			case "failures":
				failures, _ = strconv.Atoi(a.Value)
			case "errors":
				errs, _ = strconv.Atoi(a.Value)
			case "skipped":
				skipped, _ = strconv.Atoi(a.Value)
			}
		}
		run += tests
		failed += failures + errs
		passed += tests - failures - errs - skipped
	}
	return run, passed, failed, nil
}
