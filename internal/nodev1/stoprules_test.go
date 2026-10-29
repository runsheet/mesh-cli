package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// streamSession stands in for claude by printing stream-json: each line
// lines answers reaches the spec's OnLine and, as an event, its OnEvent,
// as the streaming session's reader hands them on. With hold it then
// waits for its context, as a session looping in one turn would.
type streamSession struct {
	lines func() []string
	hold  bool
	// text is the session's last message; "done" when empty
	text string

	mu   sync.Mutex
	spec SessionSpec
}

func (s *streamSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	s.mu.Lock()
	s.spec = spec
	s.mu.Unlock()
	for _, l := range s.lines() {
		if spec.OnLine != nil {
			spec.OnLine([]byte(l))
		}
		var ev harness.Event
		if json.Unmarshal([]byte(l), &ev) == nil && spec.OnEvent != nil {
			spec.OnEvent(ev)
		}
	}
	if s.hold {
		<-ctx.Done()
		return SessionResult{SessionID: "sess-ruled", Turns: 1}, ctx.Err()
	}
	text := s.text
	if text == "" {
		text = "done"
	}
	return SessionResult{SessionID: "sess-ruled", Turns: 1, Text: text}, nil
}

func toolUse(id, tool string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"id":"msg_%s","role":"assistant","content":[{"type":"tool_use","id":"%s","name":"%s","input":{"command":"make"}}]}}`, id, id, tool)
}

func toolError(id string) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"%s","content":"exit 2","is_error":true}]}}`, id)
}

// A session that loops on a failing tool is ended by its kind's
// tool_error_loop rule: the lease fails with cause stopped_by_rule and
// the rule's name first in the reason, and its usage report carries the
// counts and the rule that fired — nothing of the session's words.
func TestAToolErrorLoopEndsTheJob(t *testing.T) {
	var lines []string
	for i := range 5 {
		id := fmt.Sprintf("toolu_%d", i)
		lines = append(lines, toolUse(id, "Bash"), toolError(id))
	}
	sess := &streamSession{hold: true, lines: func() []string { return lines }}
	g := newRig(t, sess)
	job := g.offer(t, func(b *meshapi.Brief) {
		b.StopRules = []meshapi.StopRule{{Name: "error-loop", Action: meshapi.StopActionEnd, ToolErrorLoop: &meshapi.ToolErrorLoop{Tool: "Bash", Count: 3, WindowS: 300}}}
	})
	run(t, g, context.Background())
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "fail" {
		t.Fatalf("ended %q", g.fake.Ended(lease))
	}
	var f meshapi.FailRequest
	json.Unmarshal(g.fake.Calls("fail")[0].Body, &f)
	if f.Cause != meshapi.CauseStoppedByRule || f.Reason != "error-loop: 3 errors of Bash within 5m0s" {
		t.Fatalf("fail %+v", f)
	}
	usage := g.fake.Calls("usage")
	if len(usage) == 0 {
		t.Fatal("no usage report")
	}
	var u meshapi.UsageRequest
	json.Unmarshal(usage[len(usage)-1].Body, &u)
	if u.ToolCalls != 5 || u.ToolErrors != 5 || len(u.Tools) != 1 || u.Tools[0].Tool != "Bash" || len(u.StopRules) != 1 || u.StopRules[0] != "error-loop" ||
		u.FirstToolMS < 1 || u.Transcript != nil {
		t.Fatalf("usage %+v", u.Counts)
	}
	if strings.Contains(string(usage[len(usage)-1].Body), "make") || strings.Contains(string(usage[len(usage)-1].Body), "exit 2") {
		t.Fatalf("the usage carries the session's words: %s", usage[len(usage)-1].Body)
	}
}

// An escalate rule moves the live session up the ladder; with no model
// left, it ends the session as end would.
func TestAnEscalateRuleEndsAtTheTopOfTheLadder(t *testing.T) {
	sess := &streamSession{hold: true, lines: func() []string {
		return []string{toolUse("a", "Edit"), `{"type":"assistant","message":{"id":"msg_b","content":[{"type":"tool_use","id":"b","name":"Write","input":{"file_path":"protected/x.txt"}}]}}`}
	}}
	g := newRig(t, sess)
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Harness.Model = meshapi.ModelPolicy{Default: "sonnet", Max: "sonnet"}
		b.StopRules = []meshapi.StopRule{{Name: "protected", Action: meshapi.StopActionEscalate, ProtectedPaths: &meshapi.ProtectedPaths{Globs: []string{"protected/**"}}}}
	})
	run(t, g, context.Background())
	var f meshapi.FailRequest
	json.Unmarshal(g.fake.Calls("fail")[0].Body, &f)
	if g.fake.Ended(g.fake.LeaseOf(job)) != "fail" || f.Cause != meshapi.CauseStoppedByRule || !strings.HasPrefix(f.Reason, "protected: Write wrote to a protected path") {
		t.Fatalf("fail %+v", f)
	}
}

// uploads is a product's transcript location.
type uploads struct {
	mu     sync.Mutex
	bodies []string
	heads  []http.Header
}

func (u *uploads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, string(b))
	u.heads = append(u.heads, r.Header.Clone())
	u.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// The lease's own injected secrets never reach the product's transcript
// location (adr-001-amendment-learnings-and-transcripts.md): the
// session prints its Latchkey request credential, its session token,
// the git token the relay minted and the model credential, and the
// upload carries none of them; Mesh hears only the upload's bytes,
// digest and chunks, never the location.
func TestTheTranscriptReachesTheProductRedacted(t *testing.T) {
	up := &uploads{}
	dest := httptest.NewServer(up)
	t.Cleanup(dest.Close)
	var g *rig
	held := func() []string {
		g.latchkey.mu.Lock()
		defer g.latchkey.mu.Unlock()
		var out []string
		for c := range g.latchkey.creds {
			out = append(out, c)
		}
		for tok := range g.latchkey.tokens {
			out = append(out, tok)
		}
		out = append(out, g.latchkey.minted...)
		return append(out, fakeModelSecret)
	}
	var secrets []string
	// its last message is its result (settle.go): the transcript's job
	// finishes
	sess := &streamSession{text: `{"sha256":"5f0c"}`, lines: func() []string {
		secrets = held()
		var lines []string
		for _, s := range secrets {
			lines = append(lines, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"printenv: `+s+`"}]}}`)
		}
		return append(lines, `{"type":"result","subtype":"success","result":"done","session_id":"sess-ruled"}`)
	}}
	g = newRig(t, sess)
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Transcript = &meshapi.Transcript{UploadURL: dest.URL + "/t/1?sig=product-signed", Headers: map[string]string{"X-Product": "p"}, ExpiresAt: time.Now().Add(time.Hour)}
	})
	run(t, g, context.Background())
	if g.fake.Ended(g.fake.LeaseOf(job)) != "finish" {
		t.Fatalf("ended %q", g.fake.Ended(g.fake.LeaseOf(job)))
	}
	if len(secrets) < 4 || len(g.latchkey.minted) == 0 {
		t.Fatalf("the lease held %d secrets, minted %v", len(secrets), g.latchkey.minted)
	}
	up.mu.Lock()
	sent := strings.Join(up.bodies, "")
	heads := up.heads
	up.mu.Unlock()
	for _, s := range secrets {
		if strings.Contains(sent, s) {
			t.Fatalf("the upload carries the lease's secret %q:\n%s", s, sent)
		}
	}
	if strings.Count(sent, harness.Redacted) < len(secrets) || len(heads) == 0 || heads[len(heads)-1].Get("Mesh-Transcript-Final") != "true" ||
		heads[0].Get("X-Product") != "p" || heads[0].Get("Mesh-Lease") != g.fake.LeaseOf(job) {
		t.Fatalf("the upload %d chunks:\n%s", len(heads), sent)
	}
	usage := g.fake.Calls("usage")
	body := string(usage[len(usage)-1].Body)
	var u meshapi.UsageRequest
	json.Unmarshal(usage[len(usage)-1].Body, &u)
	if u.Transcript == nil || u.Transcript.Bytes != int64(len(sent)) || u.Transcript.Chunks != int64(len(heads)) || !strings.HasPrefix(u.Transcript.Digest, "sha256:") {
		t.Fatalf("usage's transcript %+v", u.Transcript)
	}
	for _, call := range []string{"usage", "finish"} {
		for _, c := range g.fake.Calls(call) {
			if strings.Contains(string(c.Body), "product-signed") || strings.Contains(string(c.Body), "printenv") {
				t.Fatalf("%s carries the location or the transcript: %s", call, c.Body)
			}
		}
	}
	_ = body
}
