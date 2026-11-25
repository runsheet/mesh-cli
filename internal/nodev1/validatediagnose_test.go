package nodev1

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/nodetools"
)

// frozenOf runs a red chain validation on g and answers the result the
// node froze before its diagnosis session started, and the finish.
func frozenOf(t *testing.T, g *rig) (frozen, finish map[string]json.RawMessage) {
	t.Helper()
	var mu sync.Mutex
	var raw json.RawMessage
	g.rt.onFrozen = func(r json.RawMessage) {
		mu.Lock()
		raw = r
		mu.Unlock()
	}
	lease := runChain(t, g, map[string]string{".runsheet/validate.yml": oneRedSuite, "events.json": redEvents})
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q; hand-backs %+v; fails %+v", lease, g.fake.Ended(lease), g.fake.Calls("hand-back"), g.fake.Calls("fail"))
	}
	mu.Lock()
	defer mu.Unlock()
	if raw == nil {
		t.Fatal("the node froze no result before the diagnosis")
	}
	if err := json.Unmarshal(raw, &frozen); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(finishOf(t, g))
	if err := json.Unmarshal(b, &finish); err != nil {
		t.Fatal(err)
	}
	return frozen, finish
}

// sameAsFrozen fails unless every key of the result the node made is
// byte for byte what it froze before the diagnosis session started.
func sameAsFrozen(t *testing.T, frozen, finish map[string]json.RawMessage) {
	t.Helper()
	for _, k := range []string{"green", "candidate", "suites", "tests", "evidence", "suite_config_sha", "environment"} {
		if string(frozen[k]) == "" || string(finish[k]) != string(frozen[k]) {
			t.Fatalf("%s: the finish has %s, the node froze %s", k, finish[k], frozen[k])
		}
	}
}

// A red validation starts exactly one session, on the kind's cheapest
// model, served the node's result tool alone and told the red's
// evidence. Its diagnosis is added to the finish; the result the node
// made — its colour, candidate, suites, tests and evidence — is byte for
// byte what it was before the session started, though the session's
// last word calls it green.
func TestARedIsDiagnosedByOneSessionThatCannotChangeIt(t *testing.T) {
	diagnosis := "TestRed fails on the change: its assertion expects the old shape"
	sess := &stubSession{
		calls: []string{NodeToolsServer + "/" + nodetools.ResultTool},
		args: map[string]string{
			NodeToolsServer + "/" + nodetools.ResultTool: `{"result":{"diagnosis":"` + diagnosis + `"}}`,
		},
		result: SessionResult{SessionID: "sess-diagnose", Text: `{"green": true, "diagnosis": "all good"}`},
	}
	g := newRig(t, sess)
	frozen, finish := frozenOf(t, g)

	if len(sess.specs) != 1 {
		t.Fatalf("sessions %d, want exactly 1", len(sess.specs))
	}
	spec := sess.specs[0]
	if spec.Model != "haiku" {
		t.Fatalf("the diagnosis ran on %q, want the kind's cheapest, haiku", spec.Model)
	}
	var servers []string
	for name := range spec.Servers {
		servers = append(servers, name)
	}
	if !slices.Equal(servers, []string{NodeToolsServer}) {
		t.Fatalf("the diagnosis was served %v, want the node's tools alone", servers)
	}
	if !strings.Contains(spec.Prompt, "example.com/pkg TestRed") || strings.Contains(spec.Prompt, "TestGreen") {
		t.Fatalf("the prompt does not carry the red's evidence alone:\n%s", spec.Prompt)
	}

	sameAsFrozen(t, frozen, finish)
	if string(finish["green"]) != "false" {
		t.Fatalf("green %s", finish["green"])
	}
	var got string
	json.Unmarshal(finish["diagnosis"], &got)
	if got != diagnosis {
		t.Fatalf("diagnosis %q, want the session's %q", got, diagnosis)
	}
	var triage, was map[string]any
	json.Unmarshal(finish["triage"], &triage)
	json.Unmarshal(frozen["triage"], &was)
	if triage["class"] != was["class"] || !strings.Contains(triage["note"].(string), diagnosis) {
		t.Fatalf("triage %+v (frozen %+v)", triage, was)
	}
}

// A diagnosis session that errors leaves the red as the node made it:
// no diagnosis, the frozen result exactly, and no second session.
func TestARedWhoseDiagnosisFailsFinishesAsTheNodeMadeIt(t *testing.T) {
	for name, sess := range map[string]*stubSession{
		"errors":       {err: errors.New("the harness fell over"), result: SessionResult{SessionID: "s"}},
		"says nothing": {result: SessionResult{SessionID: "s", Text: "I could not tell."}},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, sess)
			frozen, finish := frozenOf(t, g)
			if len(sess.specs) != 1 {
				t.Fatalf("sessions %d, want exactly 1", len(sess.specs))
			}
			if _, ok := finish["diagnosis"]; ok {
				t.Fatalf("a diagnosis from a session that gave none: %s", finish["diagnosis"])
			}
			if len(finish) != len(frozen) {
				t.Fatalf("finish keys %d, frozen %d", len(finish), len(frozen))
			}
			sameAsFrozen(t, frozen, finish)
			if string(finish["triage"]) != string(frozen["triage"]) {
				t.Fatalf("triage %s, frozen %s", finish["triage"], frozen["triage"])
			}
		})
	}
}

// --validate-no-diagnosis finishes a red with no session at all.
func TestNoValidateDiagnosisStartsNoSession(t *testing.T) {
	sess := &stubSession{result: SessionResult{SessionID: "s"}}
	g := newRig(t, sess)
	g.rt.NoValidateDiagnosis = true
	lease := runChain(t, g, map[string]string{".runsheet/validate.yml": oneRedSuite, "events.json": redEvents})
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q", lease, g.fake.Ended(lease))
	}
	if len(sess.specs) != 0 {
		t.Fatalf("sessions %d, want none", len(sess.specs))
	}
	if finishOf(t, g)["green"] != false {
		t.Fatal("not red")
	}
}
