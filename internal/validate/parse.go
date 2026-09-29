package validate

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"io"
	"strconv"
)

// ParseGoTestJSON counts `go test -json` events: each Action of pass,
// fail or skip that carries a Test name is one test; run is their total,
// passed and failed the pass and fail counts (skip counts toward run
// only). Lines that are not a test event, or not JSON at all, are
// ignored.
func ParseGoTestJSON(r io.Reader) (run, passed, failed int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Action string `json:"Action"`
			Test   string `json:"Test"`
		}
		if jerr := json.Unmarshal(line, &ev); jerr != nil {
			continue
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "pass":
			run++
			passed++
		case "fail":
			run++
			failed++
		case "skip":
			run++
		}
	}
	if sc.Err() != nil {
		return 0, 0, 0, sc.Err()
	}
	return run, passed, failed, nil
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
