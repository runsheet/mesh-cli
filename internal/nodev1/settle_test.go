package nodev1

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// stopSocketOf is the gate socket a session's --settings Stop hook names.
func stopSocketOf(t *testing.T, settings map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(settings)
	var s struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if json.Unmarshal(raw, &s) != nil || len(s.Hooks.Stop) != 1 || len(s.Hooks.Stop[0].Hooks) != 1 {
		t.Fatalf("the session's settings name no Stop hook: %s", raw)
	}
	cmd := s.Hooks.Stop[0].Hooks[0].Command
	_, sock, ok := strings.Cut(cmd, StopHookCommand+" --socket ")
	if !ok {
		t.Fatalf("the Stop hook %q is not %s", cmd, StopHookCommand)
	}
	return strings.Trim(sock, "'")
}

// A session that never calls its finish tool is refused its stop three
// times, then its lease fails no_finish: never finished with prose
// (mesh-specs gap 1).
func TestASessionThatNeverFinishesFailsNoFinish(t *testing.T) {
	var answers []string
	sess := &stubSession{calls: []string{"conform/brief"}, result: SessionResult{SessionID: "sess-prose", Text: "I think the job is done."}}
	sess.inspect = func(spec SessionSpec) {
		sock := stopSocketOf(t, spec.Settings)
		for range 4 {
			var out strings.Builder
			if err := RunStopHook(context.Background(), sock, strings.NewReader(`{}`), &out); err != nil {
				t.Errorf("the hook: %v", err)
			}
			answers = append(answers, out.String())
		}
	}
	g := newRig(t, sess)
	job := g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, a := range answers {
		if blocked := strings.Contains(a, `"decision":"block"`); blocked != (i < 3) {
			t.Fatalf("stop %d answered %q", i+1, a)
		}
	}
	if !strings.Contains(answers[0], "mcp__conform__submit") || !strings.Contains(answers[0], "mcp__conform__learn") {
		t.Fatalf("the refusal does not name the finish and learnings tools: %s", answers[0])
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "fail" || len(g.fake.Calls("finish")) != 0 {
		t.Fatalf("lease ended %q", g.fake.Ended(lease))
	}
	var req meshapi.FailRequest
	json.Unmarshal(g.fake.Calls("fail")[0].Body, &req)
	if req.Cause != meshapi.CauseNoFinish || !strings.Contains(req.Reason, "after 3 refused stops") {
		t.Fatalf("fail %+v", req)
	}
}

// A session whose last message is its result, fitting the kind's
// finish_schema, finishes with it — without its learnings, which the
// node hands to the product's learnings tool as the session.
func TestALastMessageThatFitsFinishesAndItsLearningsGoToTheProduct(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/brief"}, result: SessionResult{SessionID: "sess-msg",
		Text: `{"sha256":"5f0c","learnings":[{"lesson":"the fixture is one file","kind":"doc"}]}`}}
	g := newRig(t, sess)
	job := g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease ended %q", g.fake.Ended(lease))
	}
	var req meshapi.FinishRequest
	json.Unmarshal(g.fake.Calls("finish")[0].Body, &req)
	if string(req.Result) != `{"sha256":"5f0c"}` {
		t.Fatalf("finish result %s", req.Result)
	}
	calls, failures := g.product.snapshot()
	if len(failures) > 0 || !slices.Contains(calls, "tools/call brief") || !slices.Contains(calls, "tools/call learn") || calls[len(calls)-1] != "tools/call learn" {
		t.Fatalf("product calls %v, failures %v", calls, failures)
	}
	if len(g.product.learned) != 1 || g.product.learned[0] != `[{"lesson":"the fixture is one file","kind":"doc"}]` {
		t.Fatalf("learned %v", g.product.learned)
	}
}

// The learnings a finish call carried reach the learnings tool when the
// session did not call it; a session that did is not called for again.
func TestAFinishsLearningsReachTheLearningsToolOnce(t *testing.T) {
	learnings := `[{"lesson":"run brief first","kind":"process","area":"echo"}]`
	for _, c := range []struct {
		name  string
		calls []string
	}{
		{"the node hands them in", []string{"conform/brief", "conform/submit"}},
		{"the session handed them in", []string{"conform/brief", "conform/learn", "conform/submit"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sess := &stubSession{calls: c.calls, args: map[string]string{
				"conform/submit": `{"sha256":"5f0c","learnings":` + learnings + `}`,
				"conform/learn":  `{"learnings":` + learnings + `}`,
			}, result: SessionResult{SessionID: "sess-l", Text: "submitted"}}
			g := newRig(t, sess)
			job := g.offer(t, nil)
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			if g.fake.Ended(g.fake.LeaseOf(job)) != "finish" {
				t.Fatalf("lease ended %q", g.fake.Ended(g.fake.LeaseOf(job)))
			}
			if len(g.product.learned) != 1 || g.product.learned[0] != learnings {
				t.Fatalf("learned %v", g.product.learned)
			}
			var req meshapi.FinishRequest
			json.Unmarshal(g.fake.Calls("finish")[0].Body, &req)
			if strings.Contains(string(g.fake.Calls("finish")[0].Body), "learnings") {
				t.Fatalf("the finish carries learnings to Mesh: %s", g.fake.Calls("finish")[0].Body)
			}
		})
	}
}
