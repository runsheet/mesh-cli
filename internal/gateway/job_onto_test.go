package gateway_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A brief's onto fields decode under runsheet's names (ADR 077), and a
// brief without them reads and writes as it did before they were added.
func TestJobCodeOntoFields(t *testing.T) {
	raw := `{"form":"commit","repo":"o/r","branch":"mesh/g-1",` +
		`"onto":{"ref":"main","sha":"aaa"},` +
		`"apply":[{"sha":"bbb","ref":"refs/pull/7/head","base":"ccc"}],` +
		`"applyMode":"merge","applyAs":{"name":"Mesh","email":"m@example.com","date":"2026-10-29T01:02:03Z"},` +
		`"pushTo":"refs/runsheet/onto/v1"}`
	var c gateway.JobCode
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	want := gateway.JobCode{Form: "commit", Repo: "o/r", Branch: "mesh/g-1",
		Onto:      &gateway.JobOnto{Ref: "main", SHA: "aaa"},
		Apply:     []gateway.JobApply{{SHA: "bbb", Ref: "refs/pull/7/head", Base: "ccc"}},
		ApplyMode: gateway.ApplyMerge,
		ApplyAs:   &gateway.JobApplyAs{Name: "Mesh", Email: "m@example.com", Date: "2026-10-29T01:02:03Z"},
		PushTo:    "refs/runsheet/onto/v1"}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("decoded %+v, want %+v", c, want)
	}

	old := `{"form":"commit","repoId":"r1","repo":"o/r","baseRef":"main","branch":"mesh/g-1","baseSha":"aaa","sha":"bbb"}`
	var o gateway.JobCode
	if err := json.Unmarshal([]byte(old), &o); err != nil {
		t.Fatal(err)
	}
	if o.Onto != nil || o.Apply != nil || o.ApplyMode != "" || o.ApplyAs != nil || o.PushTo != "" {
		t.Errorf("a brief without onto decoded with it: %+v", o)
	}
	back, _ := json.Marshal(o)
	if string(back) != old {
		t.Errorf("re-encoded %s, want %s", back, old)
	}
}
