package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// shortDir is a directory short enough for a unix socket's path: a
// lease socket directory, as the node makes one — never under TMPDIR,
// which on a validating node is a job's deep scratch directory.
func shortDir(t *testing.T) string {
	t.Helper()
	r := &Runtime{}
	d, err := r.leaseSocketDir(fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// The stop gate refuses a session's stop while its finish is owed,
// naming the finish tool and asking for learnings by name while the
// session has not handed them in, up to its refusals; then, or once the
// finish has answered, it lets the session stop (ADR 001 amendment,
// finish enforcement).
func TestStopGateRefusesWhileTheFinishIsOwed(t *testing.T) {
	finished, learned := false, false
	g := &stopGate{finishTool: "mcp__conform__submit", learningsTool: "mcp__conform__learn", max: 3,
		finished: func() bool { return finished }, learned: func() bool { return learned }}
	for n := 1; n <= 3; n++ {
		a := g.decide()
		if a.Decision != "block" || !strings.Contains(a.Reason, "mcp__conform__submit") || !strings.Contains(a.Reason, "Refusal "+string(rune('0'+n))+" of 3") {
			t.Fatalf("stop %d: %+v", n, a)
		}
		if strings.Contains(a.Reason, "mcp__conform__learn") == learned {
			t.Fatalf("stop %d asks for learnings %v with learned %v: %s", n, !learned, learned, a.Reason)
		}
		if !strings.Contains(a.Reason, `"learnings": []`) && !learned {
			t.Fatalf("stop %d does not say [] is an answer: %s", n, a.Reason)
		}
		learned = n >= 2
	}
	if a := g.decide(); a.Decision != "" || g.Refused() != 3 {
		t.Fatalf("the fourth stop: %+v after %d refusals", a, g.Refused())
	}

	g2 := &stopGate{finishTool: "submit", max: 3, finished: func() bool { return finished }, learned: func() bool { return false }}
	if a := g2.decide(); a.Decision != "block" || strings.Contains(a.Reason, "learnings") {
		t.Fatalf("a kind with no learnings tool is asked for them: %+v", a)
	}
	finished = true
	if a := g2.decide(); a.Decision != "" || g2.Refused() != 1 {
		t.Fatalf("a finished session's stop: %+v", a)
	}
}

// The hook asks the gate over its socket and prints the refusal as claude
// reads it; a finished session's hook prints nothing; a hook that cannot
// reach its gate errs and prints nothing, which lets the session stop.
func TestStopHookAsksTheGateOverItsSocket(t *testing.T) {
	finished := false
	g := &stopGate{finishTool: "mcp__conform__submit", max: 3, finished: func() bool { return finished }, learned: func() bool { return false }}
	if err := g.open(shortDir(t)); err != nil {
		t.Fatal(err)
	}
	defer g.close()
	in := `{"session_id":"s","hook_event_name":"Stop","stop_hook_active":false}`
	var out bytes.Buffer
	if err := RunStopHook(context.Background(), g.path, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var a stopAnswer
	if err := json.Unmarshal(out.Bytes(), &a); err != nil || a.Decision != "block" || !strings.Contains(a.Reason, "mcp__conform__submit") {
		t.Fatalf("the hook printed %q (%v)", out.String(), err)
	}
	finished = true
	out.Reset()
	if err := RunStopHook(context.Background(), g.path, strings.NewReader(in), &out); err != nil || out.Len() != 0 {
		t.Fatalf("a finished session's hook: %q %v", out.String(), err)
	}
	out.Reset()
	if err := RunStopHook(context.Background(), filepath.Join(shortDir(t), "none.sock"), strings.NewReader(in), &out); err == nil || out.Len() != 0 {
		t.Fatalf("a hook with no gate: %q %v", out.String(), err)
	}

	// the settings run the hook command on the gate's socket, quoted
	s, _ := json.Marshal(g.settings("/opt/mesh bin/mesh"))
	if !strings.Contains(string(s), `"Stop"`) || !strings.Contains(string(s), `'/opt/mesh bin/mesh' session-stop-hook --socket '`+g.path+`'`) {
		t.Fatalf("settings %s", s)
	}
}

// settle takes the finish tool's answer; else the last message when it
// fits the kind's finish_schema, its learnings set aside; else no
// finish. A kind with neither a finish tool nor a schema keeps its last
// message as text.
func TestSettleNeverFinishesWithProse(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["sha256"],"additionalProperties":false,"properties":{"sha256":{"type":"string","pattern":"^[0-9a-f]{4}$"}}}`)
	brief := func() *meshapi.Brief {
		return &meshapi.Brief{FinishTool: "submit", FinishSchema: schema, LearningsTool: "learn"}
	}
	answered := &finishCapture{tool: "submit"}
	answered.got, answered.learnings = json.RawMessage(`{"sha256":"5f0c"}`), json.RawMessage(`[{"lesson":"l","kind":"doc"}]`)
	for _, c := range []struct {
		name          string
		brief         *meshapi.Brief
		fin           *finishCapture
		last          string
		result, learn string
		noFinish      string
	}{
		{"the finish tool answered", brief(), answered, "done", `{"sha256":"5f0c"}`, `[{"lesson":"l","kind":"doc"}]`, ""},
		{"the last message fits", brief(), &finishCapture{}, `{"sha256":"5f0c","learnings":[{"lesson":"l","kind":"trap"}]}`, `{"sha256":"5f0c"}`, `[{"lesson":"l","kind":"trap"}]`, ""},
		{"a fenced last message fits", brief(), &finishCapture{}, "Here it is:\n```json\n{\"sha256\":\"5f0c\"}\n```", `{"sha256":"5f0c"}`, "", ""},
		{"prose", brief(), &finishCapture{}, "I think it is done.", "", "", "is not a JSON object"},
		{"JSON that does not fit", brief(), &finishCapture{}, `{"hash":"5f0c"}`, "", "", "does not fit the kind's finish_schema"},
		{"a finish tool and no schema", &meshapi.Brief{FinishTool: "submit"}, &finishCapture{}, `{"sha256":"5f0c"}`, "", "", "has no finish_schema"},
		{"no finish tool and no schema", &meshapi.Brief{}, &finishCapture{}, "the verdict is green", `{"text":"the verdict is green"}`, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := settle(c.brief, c.fin, c.last, 3)
			if string(st.result) != c.result || string(st.learnings) != c.learn {
				t.Fatalf("result %s learnings %s", st.result, st.learnings)
			}
			if (c.noFinish == "") != (st.noFinish == "") || !strings.Contains(st.noFinish, c.noFinish) {
				t.Fatalf("no finish %q, want %q", st.noFinish, c.noFinish)
			}
			if st.noFinish != "" && !strings.Contains(st.noFinish, "after 3 refused stops") {
				t.Fatalf("no finish %q does not say the stops were refused", st.noFinish)
			}
		})
	}
}

// The finish capture keeps what the finish call carried beside the
// result, and knows a call of the learnings tool.
func TestFinishCaptureKeepsTheFinishsLearnings(t *testing.T) {
	f := &finishCapture{tool: "submit", learningsTool: "learn"}
	ok := func(id string) []byte {
		return []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"content":[{"type":"text","text":"{\"sha256\":\"5f0c\"}"}]}}`)
	}
	f.observe("conform", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit","arguments":{"sha256":"5f0c","learnings":[{"lesson":"l","kind":"tool"}]}}}`), ok("1"))
	if string(f.result()) != `{"sha256":"5f0c"}` || string(f.carried()) != `[{"lesson":"l","kind":"tool"}]` || f.learnedAlready() {
		t.Fatalf("result %s carried %s learned %v", f.result(), f.carried(), f.learnedAlready())
	}
	f.observe("conform", []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"learn","arguments":{"learnings":[]}}}`),
		[]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"learned 0"}],"isError":true}}`))
	if f.learnedAlready() {
		t.Fatal("a refused learn call counted")
	}
	f.observe("conform", []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"learn","arguments":{"learnings":[]}}}`),
		[]byte(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"learned 0"}]}}`))
	if !f.learnedAlready() || string(f.result()) != `{"sha256":"5f0c"}` {
		t.Fatal("a learn call answered was not counted, or changed the result")
	}
}

// A session that gave its job back with hand_back owes no finish, and a
// kind that finishes through the node's result tool owes that tool.
func TestStopGateOwesWhatTheKindFinishesWith(t *testing.T) {
	sock := filepath.Join(shortDir(t), "proxy.sock")
	r := &Runtime{Logf: t.Logf}
	fin, hb := &finishCapture{}, &sessionHandBack{}
	b := &meshapi.Brief{LeaseID: "l", Product: "conform", Servers: []meshapi.Server{{Name: "conform"}}, NodeTools: []string{"result"}}
	g := r.openStopGate(b, sock, fin, hb)
	if g == nil {
		t.Fatal("a kind finishing through result has no stop gate")
	}
	defer g.close()
	if a := g.decide(); a.Decision != "block" || !strings.Contains(a.Reason, "mcp__"+NodeToolsServer+"__result") {
		t.Fatalf("the refusal %+v", a)
	}
	hb.keep("cannot_run", "no podman")
	if a := g.decide(); a.Decision != "" {
		t.Fatalf("a handed-back session's stop: %+v", a)
	}
	if g := r.openStopGate(&meshapi.Brief{Thread: &meshapi.Thread{}, FinishTool: "submit"}, sock, fin, hb); g != nil {
		g.close()
		t.Fatal("a thread has a stop gate")
	}
}
