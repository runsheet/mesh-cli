package node

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

const (
	turnChat     = "aaaaaaaa-7777-4777-8777-aaaaaaaaaaaa"
	turnWardroom = "aaaaaaaa-8888-4888-8888-aaaaaaaaaaaa"
)

// chatTranscript is a goal-chat turn's body: one message, waiting.
const chatTranscript = `{"product":"runsheet","role":"goal-chat","messages":[{"seq":1,"from":"person","body":"I want reports exported as PDF"}]}`

func raiseChat(f *gatewaytest.Fake) {
	f.RaiseThreadTurn(gatewaytest.ThreadTurn{ID: turnChat, Product: "runsheet", ThreadKey: "chat-1#1", OrgID: orgID, ProjectID: projID,
		Role: "goal-chat", Kind: "runsheet/goal-chat", Body: chatTranscript, Ask: "thread-1#1",
		Credentials: []map[string]any{claudeCredential}})
}

func threadRunner(t *testing.T, f *gatewaytest.Fake, h harness.Harness) (*ThreadRunner, Scope) {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	sc := connected(Scopes{{Login: "acme", ID: orgID, NodeID: nodeA}})[0]
	return &ThreadRunner{GW: gw, Harness: h, Credentials: testCreds(gw), Layout: testLayout(t), WorkRoot: t.TempDir(),
		Log: log.New(os.Stderr, "", 0)}, sc
}

// A goal-chat turn is claimed as its kind, run with the role's prompt
// over the transcript, and answered with what the session said: its
// answer and the goal it drafted, as finishJob's answer and proposal.
func TestThreadRunnerAnswersAGoalChatTurn(t *testing.T) {
	f := seed(t)
	raiseChat(f)
	h := &fakeHarness{text: "Here you go.\n```json\n{\"answer\": \"Here is a draft.\", \"proposal\": {\"slug\": \"pdf-export\", \"title\": \"Reports export as PDF\"}}\n```"}
	r, sc := threadRunner(t, f, h)
	if err := r.Run(context.Background(), gateway.ThreadTurn{ID: turnChat, Product: "runsheet", ThreadKey: "chat-1#1", OrgID: orgID, Role: "goal-chat"}, "runsheet/goal-chat", sc); err != nil {
		t.Fatal(err)
	}
	turn := f.Turn(turnChat)
	if turn.Status != "answered" || turn.Answer != "Here is a draft." || !strings.Contains(turn.Proposal, `"slug": "pdf-export"`) {
		t.Fatalf("turn %+v", turn)
	}
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindThread || !strings.Contains(h.jobs[0].Prompt, "I want reports exported as PDF") ||
		!strings.Contains(h.jobs[0].Prompt, "goal assistant for acme, project mesh") {
		t.Fatalf("jobs %+v", h.jobs)
	}
	if !strings.HasPrefix(h.jobs[0].Workspace, r.Layout.JobsDir()) {
		t.Fatalf("the turn ran in %s, not its job", h.jobs[0].Workspace)
	}
	if m, err := ReadJob(strings.TrimSuffix(h.jobs[0].Workspace, "/work")); err != nil || m.Unit != JobThread || m.ThreadID != "thread-1" {
		t.Fatalf("job.json %+v %v", m, err)
	}
}

// A turn whose session fails is still answered, saying why, so the
// person is not left waiting on a node that gave up.
func TestThreadRunnerAnswersWhyWhenItCannot(t *testing.T) {
	f := seed(t)
	raiseChat(f)
	h := &fakeHarness{fail: true, text: "boom"}
	r, sc := threadRunner(t, f, h)
	if err := r.Run(context.Background(), gateway.ThreadTurn{ID: turnChat, Product: "runsheet", OrgID: orgID, Role: "goal-chat"}, "runsheet/goal-chat", sc); err != nil {
		t.Fatal(err)
	}
	if turn := f.Turn(turnChat); turn.Status != "answered" || !strings.HasPrefix(turn.Answer, "I couldn't answer this time") || turn.Proposal != "" {
		t.Fatalf("turn %+v", turn)
	}
}

// The loop takes the raised turns of the kinds this node runs, each
// once, and leaves another product's thread kind alone.
func TestLoopTakesTheThreadTurnsItRuns(t *testing.T) {
	f := seed(t)
	raiseChat(f)
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

// A turn's reply is its JSON block's answer and proposal; a message with
// no block is its own answer; an empty block is an error.
func TestParseThreadReply(t *testing.T) {
	r, err := harness.ParseThreadReply("hi\n```json\n{\"answer\": \"a\", \"proposal\": null}\n```")
	if err != nil || r.Answer != "a" || r.Proposal != "" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := harness.ParseThreadReply("just text"); err != nil || r.Answer != "just text" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := harness.ParseThreadReply("```json\n{}\n```"); err == nil {
		t.Fatal("an empty block was a reply")
	}
}
