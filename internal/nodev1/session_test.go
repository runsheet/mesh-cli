package nodev1

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/harness/session/sessiontest"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// TestMain runs this test binary as the fake claude when a session
// starts it (sessiontest.Env=1).
func TestMain(m *testing.M) {
	sessiontest.Main()
	os.Exit(m.Run())
}

// fakeSession is a ClaudeSession on the fake claude, which writes its
// args and input beside out.
func fakeSession(t *testing.T) (*ClaudeSession, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "claude")
	return &ClaudeSession{Binary: self, Logf: t.Logf}, out
}

func fakeEnv(out string) []string {
	return []string{sessiontest.Env + "=1", "FAKE_CLAUDE_OUT=" + out}
}

// A session is `claude -p` stream-json in and out, the prompt its first
// message, the model and budget the brief's, every server proxied, no
// attribution; a one-shot job's input closes once its turn ends, and
// its end and spend by model are read from the stream.
func TestClaudeSession(t *testing.T) {
	c, out := fakeSession(t)
	c.ExtraArgs = []string{"--dangerously-skip-permissions"}
	var events []string
	var mu sync.Mutex
	spec := SessionSpec{
		Workspace: t.TempDir(), Prompt: "do the job", Model: "haiku", BudgetUSD: 1.5,
		Servers: map[string]any{"conform": harness.ProxyRelay("/bin/mesh", "/run/s", "conform")},
		Env:     fakeEnv(out), Mailbox: NewMailbox(),
		OnEvent: func(ev harness.Event) { mu.Lock(); events = append(events, ev.Type); mu.Unlock() },
	}
	res, err := c.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "fake-session" || res.Text != "got: do the job" || res.CostUSD != 0.01 || res.Turns != 1 {
		t.Fatalf("%+v", res)
	}
	if m := res.Models["claude-haiku"]; m.CostUSD != 0.01 || m.OutputTokens != 1 {
		t.Fatalf("models %+v", res.Models)
	}
	args, _ := os.ReadFile(out + ".args")
	stdin, _ := os.ReadFile(out + ".stdin")
	a := string(args)
	for _, want := range []string{"-p\n", "--input-format\nstream-json", "--output-format\nstream-json", "--model\nhaiku", "--max-budget-usd\n1.50", "--mcp-config", "mcp-relay", "mcp__conform__*", `"attribution"`, "--dangerously-skip-permissions"} {
		if !strings.Contains(a, want) {
			t.Errorf("args lack %q:\n%s", want, a)
		}
	}
	if strings.Count(string(stdin), "\n") != 1 || !strings.Contains(string(stdin), `"content":"do the job"`) || strings.Contains(a, "do the job") {
		t.Fatalf("the prompt is not the one message on stdin: %q", stdin)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(events, ",") != "system,assistant,result" {
		t.Fatalf("events %v", events)
	}
}

// A session whose stream ends in an error, or that cannot start, is an
// error carrying the reason.
func TestClaudeSessionErrors(t *testing.T) {
	c, out := fakeSession(t)
	_, err := c.Run(context.Background(), SessionSpec{Workspace: t.TempDir(), Prompt: "fail", Env: fakeEnv(out)})
	if err == nil || !strings.Contains(err.Error(), "got: fail") {
		t.Fatalf("an error result: %v", err)
	}
	c.Binary = filepath.Join(t.TempDir(), "no-claude")
	if _, err := c.Run(context.Background(), SessionSpec{Workspace: t.TempDir(), Prompt: "x"}); err == nil {
		t.Fatal("a missing claude was no error")
	}
}

// runAsync runs the session in the background.
func runAsync(c *ClaudeSession, spec SessionSpec) <-chan sessionDone {
	done := make(chan sessionDone, 1)
	go func() {
		res, err := c.Run(context.Background(), spec)
		done <- sessionDone{res, err}
	}()
	return done
}

type sessionDone struct {
	res SessionResult
	err error
}

func waitDone(t *testing.T, done <-chan sessionDone) sessionDone {
	t.Helper()
	select {
	case d := <-done:
		return d
	case <-time.After(20 * time.Second):
		t.Fatal("the session did not end")
		return sessionDone{}
	}
}

// waitFor polls cond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func resultsSeen(mu *sync.Mutex, n *int) func() bool {
	return func() bool { mu.Lock(); defer mu.Unlock(); return *n > 0 }
}

// A thread's input stays open between turns: a turn left in the mailbox
// reaches the live session, and the finish closes its input.
func TestAThreadTakesTurnsFromItsMailbox(t *testing.T) {
	c, out := fakeSession(t)
	mb := NewMailbox()
	var mu sync.Mutex
	results := 0
	done := runAsync(c, SessionSpec{Workspace: t.TempDir(), Prompt: "first", Env: fakeEnv(out), Mailbox: mb, Thread: true,
		OnEvent: func(ev harness.Event) {
			if ev.Type == "result" {
				mu.Lock()
				results++
				mu.Unlock()
			}
		}})
	waitFor(t, "the first turn", resultsSeen(&mu, &results))
	if err := mb.Post(Message{Kind: MsgTurn, Text: turnMessage("conform.turn", "turn-1", 1)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second turn", func() bool { mu.Lock(); defer mu.Unlock(); return results == 2 })
	mb.Finish()
	d := waitDone(t, done)
	if d.err != nil || d.res.Turns != 2 || !strings.Contains(d.res.Text, "turn_id turn-1 (seq 1)") || !strings.Contains(d.res.Text, "`conform.turn`") {
		t.Fatalf("%+v %v", d.res, d.err)
	}
}

// ladderOf is a kind's ladder: haiku, then sonnet, then opus, capped at
// sonnet, escalating on on.
func ladderOf(on ...string) *escalate.Ladder {
	return escalate.New(meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet", "opus"}, On: on, Max: "sonnet"})
}

// An escalation in the mailbox moves the live session to the next model
// with set_model; the turn in flight ends, and the next turn — on the
// new model, in the same session — says to carry on. The step is kept.
func TestAnEscalationMovesTheLiveSession(t *testing.T) {
	c, out := fakeSession(t)
	mb := NewMailbox()
	ladder := ladderOf(escalate.OnRed)
	var mu sync.Mutex
	var models []string
	assistants := 0
	done := runAsync(c, SessionSpec{Workspace: t.TempDir(), Prompt: "slow", Model: "haiku", Env: fakeEnv(out), Mailbox: mb, Ladder: ladder,
		OnEvent: func(ev harness.Event) {
			mu.Lock()
			defer mu.Unlock()
			if ev.Type == "system" {
				models = append(models, ev.Model)
			}
			if ev.Type == "assistant" {
				assistants++
			}
		}})
	waitFor(t, "the first turn to start", resultsSeen(&mu, &assistants))
	mb.Post(Message{Kind: MsgEscalate, On: escalate.OnRed, Text: "the suite went red"})
	d := waitDone(t, done)
	if d.err != nil || d.res.Turns != 2 || !strings.Contains(d.res.Text, "moved this session to the model sonnet") {
		t.Fatalf("%+v %v", d.res, d.err)
	}
	mu.Lock()
	if strings.Join(models, ",") != "claude-haiku,claude-sonnet" {
		t.Fatalf("models %v", models)
	}
	mu.Unlock()
	steps := ladder.Steps()
	if len(steps) != 1 || steps[0].From != "haiku" || steps[0].To != "sonnet" || steps[0].On != "red" || steps[0].Reason != "the suite went red" {
		t.Fatalf("steps %+v", steps)
	}
	if len(d.res.Models) != 2 {
		t.Fatalf("spend by model %+v", d.res.Models)
	}
}

// A turn quiet past StallAfter is interrupted and the session escalates
// on stall, then carries on; a kind that does not escalate on stall is
// left to the supervisor.
func TestAStalledTurnEscalates(t *testing.T) {
	c, out := fakeSession(t)
	c.StallAfter = 300 * time.Millisecond
	ladder := ladderOf(escalate.OnStall)
	d := waitDone(t, runAsync(c, SessionSpec{Workspace: t.TempDir(), Prompt: "hang", Model: "haiku", Env: fakeEnv(out), Mailbox: NewMailbox(), Ladder: ladder}))
	if d.err != nil || !strings.Contains(d.res.Text, "moved this session to the model sonnet") {
		t.Fatalf("%+v %v", d.res, d.err)
	}
	if steps := ladder.Steps(); len(steps) != 1 || steps[0].On != "stall" {
		t.Fatalf("steps %+v", steps)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ladder = ladderOf(escalate.OnRed)
	_, err := c.Run(ctx, SessionSpec{Workspace: t.TempDir(), Prompt: "hang", Env: fakeEnv(out), Mailbox: NewMailbox(), Ladder: ladder})
	if err == nil || len(ladder.Steps()) != 0 {
		t.Fatalf("a kind that does not escalate on stall: %v %+v", err, ladder.Steps())
	}
}

// A model the session refuses leaves it where it was, and the step is
// marked failed.
func TestARefusedEscalationIsMarkedFailed(t *testing.T) {
	c, out := fakeSession(t)
	mb := NewMailbox()
	ladder := escalate.New(meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"refuse"}, On: []string{escalate.OnAsked}})
	var mu sync.Mutex
	assistants := 0
	done := runAsync(c, SessionSpec{Workspace: t.TempDir(), Prompt: "slow", Env: fakeEnv(out), Mailbox: mb, Ladder: ladder,
		OnEvent: func(ev harness.Event) {
			if ev.Type == "assistant" {
				mu.Lock()
				assistants++
				mu.Unlock()
			}
		}})
	waitFor(t, "the turn to start", resultsSeen(&mu, &assistants))
	mb.Post(Message{Kind: MsgEscalate, On: escalate.OnAsked, Text: "too hard"})
	d := waitDone(t, done)
	if d.err != nil || d.res.Turns != 1 {
		t.Fatalf("%+v %v", d.res, d.err)
	}
	steps := ladder.Steps()
	if len(steps) != 1 || !strings.Contains(steps[0].Failed, "no such model") || ladder.Model() != "haiku" {
		t.Fatalf("steps %+v, model %q", steps, ladder.Model())
	}
}

// usageOf reports the lease's spend in Mesh's JobUsage shape: each
// model's tokens (input counting the cached), list cost in cents and
// turns, their sums, and the escalations; the actual cost is the list
// cost only on an api-key account.
func TestUsageOfAStreamingSession(t *testing.T) {
	esc := []meshapi.Escalation{{From: "haiku", To: "sonnet", On: "red"}}
	res := SessionResult{Turns: 3, Duration: 2 * time.Second, Models: map[string]harness.ModelSpend{
		"claude-sonnet": {InputTokens: 10, CacheReadInputTokens: 5, OutputTokens: 2, CostUSD: 0.2},
		"claude-haiku":  {InputTokens: 4, CacheCreationInputTokens: 1, OutputTokens: 1, CostUSD: 0.01},
	}, ModelTurns: map[string]int{"claude-haiku": 2, "claude-sonnet": 1}}
	for _, c := range []struct {
		name       string
		account    *sessionAccount
		ref        string
		actualCost int64
	}{
		{"the node's own login", &sessionAccount{}, "", 0},
		{"a subscription", &sessionAccount{cred: &modelCredential{AccountRef: "acct-a", Kind: "subscription"}}, "acct-a", 0},
		{"an api key", &sessionAccount{cred: &modelCredential{AccountRef: "acct-b", Kind: "api_key"}}, "acct-b", 21},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := usageOf(outcome{result: res, escalations: esc, account: c.account, harness: "claude-code", model: "sonnet"})
			want := &meshapi.UsageRequest{
				Harness: "claude-code", Model: "sonnet", AccountRef: c.ref,
				TokensInput: 20, TokensOutput: 3, TokensCacheRead: 5, TokensCacheWrite: 1,
				CostListCents: 21, CostActualCents: c.actualCost, DurationMS: 2000, Turns: 3,
				Models: []meshapi.ModelUsage{
					{Model: "claude-haiku", TokensInput: 5, TokensOutput: 1, TokensCacheWrite: 1, CostListCents: 1, CostActualCents: min(c.actualCost, 1), Turns: 2},
					{Model: "claude-sonnet", TokensInput: 15, TokensOutput: 2, TokensCacheRead: 5, CostListCents: 20, CostActualCents: min(c.actualCost, 20), Turns: 1},
				},
				Escalations: esc,
			}
			if !reflect.DeepEqual(u, want) {
				t.Fatalf("got  %+v\nwant %+v", u, want)
			}
		})
	}
	if usageOf(outcome{}) != nil {
		t.Fatal("nothing spent is no usage")
	}
}
