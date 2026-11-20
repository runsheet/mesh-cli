package nodev1

import (
	"encoding/json"
	"testing"
)

func TestWithCoverageAddsTheMapAsAString(t *testing.T) {
	out := withCoverage(json.RawMessage(`{"green":true,"pin":{"ref":"x"}}`), `{"v":1}`)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["coverage"] != `{"v":1}` || obj["green"] != true || obj["pin"] == nil {
		t.Fatalf("got %s", out)
	}
}

func TestWithCoverageDropsASessionsKey(t *testing.T) {
	out := withCoverage(json.RawMessage(`{"green":true,"coverage":"forged"}`), "")
	if string(out) != `{"green":true}` {
		t.Fatalf("got %s", out)
	}
	out = withCoverage(json.RawMessage(`{"coverage":"forged"}`), `{"v":2}`)
	if string(out) != `{"coverage":"{\"v\":2}"}` {
		t.Fatalf("got %s", out)
	}
}

func TestWithCoverageNoMapLeavesFinish(t *testing.T) {
	in := json.RawMessage(`{"green":false}`)
	if out := withCoverage(in, ""); string(out) != string(in) {
		t.Fatalf("got %s", out)
	}
	if out := withCoverage(nil, ""); out != nil {
		t.Fatalf("got %s", out)
	}
}

func TestCoverageNoteKeepsTheNewest(t *testing.T) {
	var nilNote *coverageNote
	nilNote.set("x")
	if nilNote.get() != "" {
		t.Fatal("nil note kept a map")
	}
	c := &coverageNote{}
	c.set("a")
	c.set("b")
	if c.get() != "b" {
		t.Fatalf("got %q", c.get())
	}
}
