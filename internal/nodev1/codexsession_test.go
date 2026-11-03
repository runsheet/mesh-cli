package nodev1

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// fakeAppServer is `codex app-server` enough to drive one thread,
// mirroring internal/harness/codexsession's own fake: thread/start
// answers a thread id, turn/start emits item.started then two
// agentMessage deltas followed by turn.completed (with usage, ending
// the turn with no interrupt needed), turn/steer emits one more
// agentMessage, and turn/interrupt answers its own result followed by
// a turn.completed naming it interrupted. It runs as a SessionSpec's
// InPod.
type fakeAppServer struct {
	mu      sync.Mutex
	methods []string
}

func (f *fakeAppServer) run(_ context.Context, _, _ []string, stdin io.Reader, stdout, _ io.Writer) error {
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(stdout, string(b))
	}
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		f.mu.Lock()
		f.methods = append(f.methods, m.Method)
		f.mu.Unlock()
		var id int
		_ = json.Unmarshal(m.ID, &id)
		switch m.Method {
		case "thread/start":
			emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"threadId": "fake-thread"}})
		case "turn/start":
			emit(map[string]any{"jsonrpc": "2.0", "method": "item.started", "params": map[string]any{"item": map[string]any{"type": "agentMessage"}}})
			emit(map[string]any{"jsonrpc": "2.0", "method": "agentMessage", "params": map[string]any{"text": "working"}})
			emit(map[string]any{"jsonrpc": "2.0", "method": "agentMessage", "params": map[string]any{"text": " on it"}})
			emit(map[string]any{"jsonrpc": "2.0", "method": "turn.completed", "params": map[string]any{"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}})
		case "turn/steer":
			emit(map[string]any{"jsonrpc": "2.0", "method": "agentMessage", "params": map[string]any{"text": "steered"}})
		case "turn/interrupt":
			emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
			emit(map[string]any{"jsonrpc": "2.0", "method": "turn.completed", "params": map[string]any{"interrupted": true}})
		}
	}
	return nil
}

func (f *fakeAppServer) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

// A CodexSession's stream carries the same content stream-json would:
// item.started answers no line, agentMessage deltas become assistant
// events with the running text, and turn.completed becomes a result
// event carrying the turn's text, cost and usage — and the lease
// finishes with that result.
func TestCodexSessionNormalisesAppServerNotifications(t *testing.T) {
	fake := &fakeAppServer{}
	c := &CodexSession{Binary: "codex", Logf: t.Logf}

	var mu sync.Mutex
	var lines []string
	var events []string
	spec := SessionSpec{
		Workspace: t.TempDir(), Prompt: "do the job", Model: "gpt-6-sol",
		InPod: fake.run,
		OnLine: func(line []byte) {
			mu.Lock()
			lines = append(lines, string(line))
			mu.Unlock()
		},
		OnEvent: func(ev harness.Event) {
			mu.Lock()
			events = append(events, ev.Type)
			mu.Unlock()
		},
	}

	res, err := c.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "working on it" || res.Turns != 1 {
		t.Fatalf("%+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 {
		t.Fatalf("usage %+v", res.Usage)
	}
	if res.CostUSD <= 0 {
		t.Fatalf("cost %v", res.CostUSD)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(events, ",") != "assistant,assistant,result" {
		t.Fatalf("events %v", events)
	}
	if len(lines) != 3 {
		t.Fatalf("lines %v", lines)
	}
	for i, want := range []string{`"type":"assistant"`, `"type":"assistant"`, `"type":"result"`} {
		if !strings.Contains(lines[i], want) {
			t.Fatalf("line %d is %q, want it to contain %q", i, lines[i], want)
		}
	}
	last := lines[len(lines)-1]
	for _, want := range []string{`"subtype":"success"`, `"is_error":false`, `"num_turns":1`, `"result":"working on it"`, `"input_tokens":10`, `"output_tokens":5`} {
		if !strings.Contains(last, want) {
			t.Fatalf("result line lacks %q: %s", want, last)
		}
	}

	wantSent := []string{"thread/start", "turn/start"}
	if got := fake.sent(); !equalStrings(got, wantSent) {
		t.Fatalf("the fake received %v, want %v", got, wantSent)
	}
}

// A steer sent while a turn is in flight, then an interrupt, reach the
// fake app server as turn/steer and turn/interrupt.
func TestCodexSessionSteersThenInterrupts(t *testing.T) {
	fake := newStallingAppServer()
	c := &CodexSession{Binary: "codex", Logf: t.Logf}
	mb := NewMailbox()

	spec := SessionSpec{
		Workspace: t.TempDir(), Prompt: "do the job", Model: "gpt-6-sol",
		InPod: fake.run, Mailbox: mb,
	}

	done := make(chan sessionDone, 1)
	go func() {
		res, err := c.Run(context.Background(), spec)
		done <- sessionDone{res, err}
	}()

	fake.waitStarted(t)
	mb.Post(Message{Kind: MsgTurn, Text: "mid"})
	fake.waitSteered(t)
	mb.Post(Message{Kind: MsgInterrupt})
	mb.Finish()

	if d := waitDone(t, done); d.err != nil {
		t.Fatal(d.err)
	}

	wantSent := []string{"thread/start", "turn/start", "turn/steer", "turn/interrupt"}
	if got := fake.sent(); !equalStrings(got, wantSent) {
		t.Fatalf("the fake received %v, want %v", got, wantSent)
	}
}

// stallingAppServer is turn/start with no turn.completed of its own —
// the turn stays in flight until an interrupt ends it — so a steer
// reaches it mid-turn, as a real one does.
type stallingAppServer struct {
	mu      sync.Mutex
	methods []string
	started chan struct{}
	steered chan struct{}
	once1   sync.Once
	once2   sync.Once
}

func newStallingAppServer() *stallingAppServer {
	return &stallingAppServer{started: make(chan struct{}), steered: make(chan struct{})}
}

func (f *stallingAppServer) run(_ context.Context, _, _ []string, stdin io.Reader, stdout, _ io.Writer) error {
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(stdout, string(b))
	}
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		f.mu.Lock()
		f.methods = append(f.methods, m.Method)
		f.mu.Unlock()
		var id int
		_ = json.Unmarshal(m.ID, &id)
		switch m.Method {
		case "thread/start":
			emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"threadId": "fake-thread"}})
		case "turn/start":
			emit(map[string]any{"jsonrpc": "2.0", "method": "item.started", "params": map[string]any{"item": map[string]any{"type": "agentMessage"}}})
			f.once1.Do(func() { close(f.started) })
		case "turn/steer":
			emit(map[string]any{"jsonrpc": "2.0", "method": "agentMessage", "params": map[string]any{"text": "steered"}})
			f.once2.Do(func() { close(f.steered) })
		case "turn/interrupt":
			emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
			emit(map[string]any{"jsonrpc": "2.0", "method": "turn.completed", "params": map[string]any{"interrupted": true}})
		}
	}
	return nil
}

func (f *stallingAppServer) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func (f *stallingAppServer) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(10 * time.Second):
		t.Fatal("turn/start never reached the fake")
	}
}

func (f *stallingAppServer) waitSteered(t *testing.T) {
	t.Helper()
	select {
	case <-f.steered:
	case <-time.After(10 * time.Second):
		t.Fatal("turn/steer never reached the fake")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
