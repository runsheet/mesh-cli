package node

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/jobresult"
	"projectmesh.io/mesh-cli/internal/mcptool"
)

const (
	turnChat     = "aaaaaaaa-7777-4777-8777-aaaaaaaaaaaa"
	turnWardroom = "aaaaaaaa-8888-4888-8888-aaaaaaaaaaaa"
)

// chatTranscript is a goal-chat turn's body: one message, waiting.
const chatTranscript = `{"product":"runsheet","role":"goal-chat","messages":[{"seq":1,"from":"person","body":"I want reports exported as PDF"}]}`

// chatGuidance is the goal-chat kind's guidance as runsheet publishes
// it (.mesh/agents/goal-chat.md), as the brief carries it.
const chatGuidance = "You are runsheet's goal assistant. A person is talking to you on a thread about a goal they want.\n\nHow to talk:\n- Short, plain sentences.\n- Never set, plan or change a goal yourself: you propose; the person sets it.\n"

// draftSchema is a finish_schema for the goal draft.
const draftSchema = `{"type":"object","required":["slug","title"],"properties":{"slug":{"type":"string"},"title":{"type":"string"}},"additionalProperties":false}`

func raiseChat(f *gatewaytest.Fake, guidance, schema string) {
	f.RaiseThreadTurn(gatewaytest.ThreadTurn{ID: turnChat, Product: "runsheet", ThreadKey: "chat-1#1", OrgID: orgID, ProjectID: projID,
		Role: "goal-chat", Kind: "runsheet/goal-chat", Body: chatTranscript, Ask: "thread-1#1",
		Credentials: []map[string]any{claudeCredential}, Guidance: guidance, FinishSchema: schema})
}

func threadRunner(t *testing.T, f *gatewaytest.Fake, h harness.Harness) (*ThreadRunner, Scope, *bytes.Buffer) {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	sc := connected(Scopes{{Login: "acme", ID: orgID, NodeID: nodeA}})[0]
	var logs bytes.Buffer
	return &ThreadRunner{GW: gw, Harness: h, Credentials: testCreds(gw), Layout: testLayout(t), WorkRoot: t.TempDir(),
		Log: log.New(&teeWriter{&logs}, "", 0)}, sc, &logs
}

// teeWriter keeps the log and shows it.
type teeWriter struct{ buf *bytes.Buffer }

func (w *teeWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	return os.Stderr.Write(p)
}

// callResult is a fake harness's session calling the node's mesh_result
// with args, as the real session does over the proxy: the reply, or the
// refusal.
func callResult(t *testing.T, job harness.Job, args map[string]any) (string, error) {
	t.Helper()
	tool, ok := mcptool.Find(job.NodeTools, jobresult.ResultTool)
	if !ok {
		t.Fatalf("the job carries no %s tool: %+v", jobresult.ResultTool, job.NodeTools)
	}
	return tool.Call(context.Background(), args)
}

// A goal-chat turn is claimed as its kind and run on the brief's
// guidance — first in the prompt, verbatim, with the transcript and the
// finish after it and none of the node's old role text — and answered
// with what the session handed mesh_result: its answer and the goal it
// drafted, as finishJob's answer and proposal.
func TestThreadRunnerAnswersAGoalChatTurnThroughMeshResult(t *testing.T) {
	f := seed(t)
	raiseChat(f, chatGuidance, "")
	h := &fakeHarness{text: "Here is a draft."}
	h.onRun = func(job harness.Job) {
		if _, err := callResult(t, job, map[string]any{"answer": "Here is a draft.", "proposal": map[string]any{"slug": "pdf-export", "title": "Reports export as PDF"}}); err != nil {
			t.Errorf("mesh_result: %v", err)
		}
	}
	r, sc, logs := threadRunner(t, f, h)
	if err := r.Run(context.Background(), gateway.ThreadTurn{ID: turnChat, Product: "runsheet", ThreadKey: "chat-1#1", OrgID: orgID, Role: "goal-chat"}, "runsheet/goal-chat", sc); err != nil {
		t.Fatal(err)
	}
	turn := f.Turn(turnChat)
	if turn.Status != "answered" || turn.Answer != "Here is a draft." || !strings.Contains(turn.Proposal, `"slug":"pdf-export"`) {
		t.Fatalf("turn %+v", turn)
	}
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindThread {
		t.Fatalf("jobs %+v", h.jobs)
	}
	prompt := h.jobs[0].Prompt
	if !strings.HasPrefix(prompt, strings.TrimSpace(chatGuidance)) {
		t.Fatalf("the prompt does not open with the brief's guidance:\n%s", prompt)
	}
	for _, want := range []string{"I want reports exported as PDF", "# The thread", "A thread of acme, project mesh on runsheet; you answer as its goal-chat", "# How this job finishes", "`mesh_result` tool once", "{answer and/or proposal}"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	for _, gone := range []string{"goal assistant for acme", "Use the runsheet tools before you answer", "End your reply with one JSON block", "```json"} {
		if strings.Contains(prompt, gone) {
			t.Fatalf("the prompt still carries the node's role text %q:\n%s", gone, prompt)
		}
	}
	if strings.Count(prompt, "How to talk") != 1 {
		t.Fatalf("the role text is in the prompt other than as the guidance:\n%s", prompt)
	}
	// the node's tools are served to the session as mesh-node, beside
	// the runsheet server, through a proxy of their own here (no
	// SessionProxies on this runner)
	if _, ok := h.jobs[0].MCPServers[harness.NodeWorkMCP]; !ok {
		t.Fatalf("no %s server in the job: %v", harness.NodeWorkMCP, h.jobs[0].MCPServers)
	}
	if strings.Contains(logs.String(), noGuidanceLine) {
		t.Fatalf("a brief with guidance was logged as without:\n%s", logs.String())
	}
	if !strings.HasPrefix(h.jobs[0].Workspace, r.Layout.JobsDir()) {
		t.Fatalf("the turn ran in %s, not its job", h.jobs[0].Workspace)
	}
	if m, err := ReadJob(strings.TrimSuffix(h.jobs[0].Workspace, "/work")); err != nil || m.Unit != JobThread || m.ThreadID != "thread-1" {
		t.Fatalf("job.json %+v %v", m, err)
	}
}

// A proposal that does not fit the kind's finish_schema is refused to
// the session with the field, before anything reaches the platform; the
// corrected call is the result.
func TestThreadRunnerRefusesAProposalOffTheSchemaUntilCorrected(t *testing.T) {
	f := seed(t)
	raiseChat(f, chatGuidance, draftSchema)
	h := &fakeHarness{}
	var refusal string
	h.onRun = func(job harness.Job) {
		_, err := callResult(t, job, map[string]any{"answer": "A draft.", "proposal": map[string]any{"slug": "pdf-export", "colour": "red"}})
		if err == nil {
			t.Error("a proposal off the schema was recorded")
			return
		}
		refusal = err.Error()
		if _, err := callResult(t, job, map[string]any{"answer": "A draft.", "proposal": map[string]any{"slug": "pdf-export", "title": "Reports export as PDF"}}); err != nil {
			t.Errorf("the corrected call: %v", err)
		}
	}
	r, sc, _ := threadRunner(t, f, h)
	if err := r.Run(context.Background(), gateway.ThreadTurn{ID: turnChat, Product: "runsheet", OrgID: orgID, Role: "goal-chat"}, "runsheet/goal-chat", sc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(refusal, "finish_schema") || !strings.Contains(refusal, "colour") {
		t.Fatalf("refusal %q", refusal)
	}
	if turn := f.Turn(turnChat); turn.Status != "answered" || !strings.Contains(turn.Proposal, `"title":"Reports export as PDF"`) {
		t.Fatalf("turn %+v", turn)
	}
	if !strings.Contains(h.jobs[0].Prompt, "The product's schema for its part is the tool's input schema") {
		t.Fatalf("the finish does not name the schema:\n%s", h.jobs[0].Prompt)
	}
}

// A turn whose session fails, gives up by mesh_fail, or hands nothing in
// is still answered, saying why, so the person is not left waiting on a
// node that gave up; nothing of the final message is read as the result.
func TestThreadRunnerAnswersWhyWhenItCannot(t *testing.T) {
	for name, h := range map[string]*fakeHarness{
		"failed":  {fail: true, text: "boom"},
		"no call": {text: "Here you go.\n```json\n{\"answer\": \"a text answer the node no longer reads\"}\n```"},
		"gave up": {onRun: func(job harness.Job) {
			tool, _ := mcptool.Find(job.NodeTools, jobresult.FailTool)
			_, _ = tool.Call(context.Background(), map[string]any{"error": "the thread names a project I cannot read"})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := seed(t)
			raiseChat(f, chatGuidance, "")
			r, sc, _ := threadRunner(t, f, h)
			if err := r.Run(context.Background(), gateway.ThreadTurn{ID: turnChat, Product: "runsheet", OrgID: orgID, Role: "goal-chat"}, "runsheet/goal-chat", sc); err != nil {
				t.Fatal(err)
			}
			turn := f.Turn(turnChat)
			if turn.Status != "answered" || !strings.HasPrefix(turn.Answer, "I couldn't answer this time") || turn.Proposal != "" {
				t.Fatalf("turn %+v", turn)
			}
			switch name {
			case "no call":
				if !strings.Contains(turn.Answer, "no mesh_result call") || strings.Contains(turn.Answer, "a text answer") {
					t.Fatalf("answer %q", turn.Answer)
				}
			case "gave up":
				if !strings.Contains(turn.Answer, "the thread names a project I cannot read") {
					t.Fatalf("answer %q", turn.Answer)
				}
			}
		})
	}
}

// A brief with no guidance — a platform before runsheet ADR 067 — runs
// the node's built-in goal-chat prompt, logged once, still finishing by
// mesh_result.
func TestThreadRunnerFallsBackToTheBuiltInPromptWithoutGuidance(t *testing.T) {
	f := seed(t)
	raiseChat(f, "", "")
	h := &fakeHarness{}
	h.onRun = func(job harness.Job) {
		_, _ = callResult(t, job, map[string]any{"answer": "What format?"})
	}
	r, sc, logs := threadRunner(t, f, h)
	for i := 0; i < 2; i++ {
		raiseChat(f, "", "")
		if err := r.Run(context.Background(), gateway.ThreadTurn{ID: turnChat, Product: "runsheet", OrgID: orgID, Role: "goal-chat"}, "runsheet/goal-chat", sc); err != nil {
			t.Fatal(err)
		}
	}
	if turn := f.Turn(turnChat); turn.Status != "answered" || turn.Answer != "What format?" {
		t.Fatalf("turn %+v", turn)
	}
	prompt := h.jobs[0].Prompt
	if !strings.Contains(prompt, "goal assistant for acme, project mesh") || !strings.Contains(prompt, "# How this job finishes") || strings.Contains(prompt, "End your reply with one JSON block") {
		t.Fatalf("the built-in prompt:\n%s", prompt)
	}
	if n := strings.Count(logs.String(), noGuidanceLine); n != 1 {
		t.Fatalf("%s logged %d times:\n%s", noGuidanceLine, n, logs.String())
	}
}

// The loop takes the raised turns of the kinds this node runs, each
// once, and leaves another product's thread kind alone.
func TestLoopTakesTheThreadTurnsItRuns(t *testing.T) {
	f := seed(t)
	raiseChat(f, chatGuidance, "")
	f.RaiseThreadTurn(gatewaytest.ThreadTurn{ID: turnWardroom, Product: "wardroom", ThreadKey: "general/1", OrgID: orgID, Role: "assistant", Kind: "wardroom/thread"})
	gw := gateway.New(f.URL(), "rs_test")
	var mu sync.Mutex
	took := map[string]string{}
	l := &Loop{GW: gw, Cfg: &Config{Slug: "n", Capacity: Fixed(2)}, Scopes: connected(Scopes{{Login: "acme", ID: orgID, NodeID: nodeA}}), Log: log.New(os.Stderr, "", 0),
		Thread: func(ctx context.Context, turn gateway.ThreadTurn, kind string, sc Scope) {
			mu.Lock()
			took[turn.ID] = kind
			mu.Unlock()
		}}
	l.threadsDue(context.Background())
	l.wg.Wait()
	if len(took) != 1 || took[turnChat] != "runsheet/goal-chat" {
		t.Fatalf("took %v", took)
	}
}
