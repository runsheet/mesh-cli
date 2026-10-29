package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// toolCall is one call a toolSession makes on the node's own server.
type toolCall struct {
	tool string
	args map[string]any
}

// toolSession stands in for claude: it lists the node's tools, calls
// each of calls on the node's own server through the lease's
// sessionproxy, as the session's mcp-relay would, keeping each answer,
// and says whether the node closed its input.
type toolSession struct {
	calls  []toolCall
	result SessionResult
	err    error

	mu       sync.Mutex
	listed   []string
	answers  []string
	refused  []bool
	finished bool
}

func (s *toolSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	entry := spec.Servers[NodeToolsServer]
	list, _, err := rpcThroughProxy(ctx, entry, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if err != nil {
		return SessionResult{}, err
	}
	var tools struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(list), &tools)
	s.mu.Lock()
	for _, tl := range tools.Result.Tools {
		s.listed = append(s.listed, tl.Name)
	}
	s.mu.Unlock()
	for _, c := range s.calls {
		args, _ := json.Marshal(c.args)
		text, isErr, err := rpcThroughProxy(ctx, entry, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, c.tool, args))
		if err != nil {
			return SessionResult{}, err
		}
		s.mu.Lock()
		s.answers, s.refused = append(s.answers, text), append(s.refused, isErr)
		s.mu.Unlock()
	}
	select {
	case <-spec.Mailbox.Finished():
		s.mu.Lock()
		s.finished = true
		s.mu.Unlock()
	default:
	}
	return s.result, s.err
}

// rpcThroughProxy posts one JSON-RPC request to the entry's socket and
// server and answers its first text content and isError.
func rpcThroughProxy(ctx context.Context, entry any, body string) (string, bool, error) {
	sock, name, ok := harness.ProxiedOf(entry)
	if !ok {
		return "", false, fmt.Errorf("not a proxied entry: %v", entry)
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://proxy/mcp/"+name, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", false, fmt.Errorf("proxy answered %d: %s", resp.StatusCode, raw)
	}
	var ans struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return "", false, err
	}
	if ans.Result == nil || len(ans.Result.Content) == 0 {
		return string(raw), false, nil
	}
	return ans.Result.Content[0].Text, ans.Result.IsError, nil
}

var digest = strings.Repeat("0f", 32)

// A kind whose finish is Mesh's own: the session sees result, hand_back
// and progress on the node's server; progress posts its note on the
// lease; a result that does not fit the kind's finish_schema is refused
// to the session; the one that fits closes its input, and the node
// finishes the lease with it (projectmesh-io/api ADR 001 amendment, node
// tools).
func TestResultFinishesTheLeaseWithTheSessionsValue(t *testing.T) {
	sess := &toolSession{
		calls: []toolCall{
			{"progress", map[string]any{"note": "read the README"}},
			{"result", map[string]any{"result": map[string]any{"wrong": 1}}},
			{"result", map[string]any{"result": map[string]any{"sha256": digest}}},
		},
		result: SessionResult{SessionID: "sess-r", Text: "done"},
	}
	g := newRig(t, sess)
	job := g.offer(t, func(b *meshapi.Brief) { b.FinishTool = "" })
	run(t, g, context.Background())
	lease := g.fake.LeaseOf(job)

	if strings.Join(sess.listed, ",") != "result,hand_back,progress" {
		t.Fatalf("the session's node tools %v", sess.listed)
	}
	if len(sess.refused) != 3 || sess.refused[0] || !sess.refused[1] || sess.refused[2] || !strings.Contains(sess.answers[1], "finish schema") {
		t.Fatalf("answers %q refused %v", sess.answers, sess.refused)
	}
	if !sess.finished {
		t.Fatal("an accepted result did not close the session's input")
	}
	progress := g.fake.Calls("progress")
	var note meshapi.ProgressRequest
	if len(progress) != 1 || progress[0].LeaseID != lease || json.Unmarshal(progress[0].Body, &note) != nil || note.Note != "read the README" ||
		progress[0].Auth != "Bearer machine" {
		t.Fatalf("progress posts %+v", progress)
	}
	var p meshapi.Progressed
	if json.Unmarshal([]byte(sess.answers[0]), &p) != nil || p.Progress == nil || p.Progress.Note != "read the README" {
		t.Fatalf("progress answered %s", sess.answers[0])
	}
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("the lease ended %q", g.fake.Ended(lease))
	}
	var fin meshapi.FinishRequest
	json.Unmarshal(g.fake.Calls("finish")[0].Body, &fin)
	if string(fin.Result) != `{"sha256":"`+digest+`"}` || fin.SessionRef != "sess-r" {
		t.Fatalf("finish %+v (%s)", fin, fin.Result)
	}
}

// A session that gives the job back with hand_back is handed back with
// the cause it claimed and its reason, even after a result, and a cause
// only the node may say is refused to it.
func TestHandBackHandsTheLeaseBackWithTheSessionsCause(t *testing.T) {
	sess := &toolSession{
		calls: []toolCall{
			{"hand_back", map[string]any{"cause": "stalled", "reason": "x"}},
			{"result", map[string]any{"result": map[string]any{"sha256": digest}}},
			{"hand_back", map[string]any{"cause": "product", "reason": "the brief tool answers 500"}},
		},
		result: SessionResult{SessionID: "sess-h"},
	}
	g := newRig(t, sess)
	job := g.offer(t, nil)
	run(t, g, context.Background())
	lease := g.fake.LeaseOf(job)

	if len(sess.refused) != 3 || !sess.refused[0] || sess.refused[2] || !sess.finished {
		t.Fatalf("answers %q refused %v finished %v", sess.answers, sess.refused, sess.finished)
	}
	if g.fake.Ended(lease) != "hand-back" || len(g.fake.Calls("finish")) != 0 {
		t.Fatalf("the lease ended %q", g.fake.Ended(lease))
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(g.fake.Calls("hand-back")[0].Body, &hb)
	if hb.Cause != meshapi.CauseProduct || hb.Reason != "the brief tool answers 500" || hb.SessionRef != "sess-h" {
		t.Fatalf("hand-back %+v", hb)
	}
}

// A session's hand-back is honoured however its session then exits: an
// error after it is not taken for the node's own cannot_run.
func TestHandBackStandsWhenTheSessionThenFails(t *testing.T) {
	sess := &toolSession{
		calls: []toolCall{{"hand_back", map[string]any{"cause": "cannot_run", "reason": "the job needs a GPU"}}},
		err:   fmt.Errorf("claude exited 1"),
	}
	g := newRig(t, sess)
	job := g.offer(t, nil)
	run(t, g, context.Background())
	var hb meshapi.HandBackRequest
	json.Unmarshal(g.fake.Calls("hand-back")[0].Body, &hb)
	if g.fake.Ended(g.fake.LeaseOf(job)) != "hand-back" || hb.Cause != meshapi.CauseCannotRun || hb.Reason != "the job needs a GPU" {
		t.Fatalf("hand-back %+v", hb)
	}
}
