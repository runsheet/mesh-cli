package codexsession

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeAppServer is `codex app-server` enough to drive one thread: it
// answers thread/start with a thread id, a turn/start with
// item.started then agentMessage, a turn/steer with another
// agentMessage (never ending the turn itself — only an interrupt
// does), and a turn/interrupt with its own result followed by a
// turn.completed naming it interrupted. It runs as an Options.Spawn.
type fakeAppServer struct {
	mu      sync.Mutex
	methods []string
}

func (f *fakeAppServer) run(ctx context.Context, argv []string, stdin io.Reader, stdout, _ io.Writer) error {
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

// notifications collects a session's notifications in order and lets
// a test wait for the next of a given method.
type notifications struct {
	mu  sync.Mutex
	all []Notification
	ch  chan Notification
}

func newNotifications() *notifications { return &notifications{ch: make(chan Notification, 64)} }

func (n *notifications) on(ev Notification) {
	n.mu.Lock()
	n.all = append(n.all, ev)
	n.mu.Unlock()
	n.ch <- ev
}

func (n *notifications) next(t *testing.T, method string) Notification {
	t.Helper()
	for {
		select {
		case ev := <-n.ch:
			if ev.Method == method {
				return ev
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s notification", method)
		}
	}
}

func (n *notifications) methods() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.all))
	for i, ev := range n.all {
		out[i] = ev.Method
	}
	return out
}

// A session opens its thread with thread/start, sends its first turn
// with turn/start, adds to the turn in flight with turn/steer when a
// second message is sent before it ends, and ends it with
// turn/interrupt — in that order on the fake's side — and every
// notification the fake sends back reaches OnEvent and OnLine, in
// order, on the caller's.
func TestASessionStartsSteersAndInterruptsATurn(t *testing.T) {
	fake := &fakeAppServer{}
	notifs := newNotifications()
	var linesMu sync.Mutex
	var lines []string
	opts := Options{
		Binary:  "codex",
		Spawn:   fake.run,
		OnEvent: notifs.on,
		OnLine: func(line []byte) {
			linesMu.Lock()
			lines = append(lines, string(line))
			linesMu.Unlock()
		},
	}

	s, err := Start(context.Background(), opts, "first")
	if err != nil {
		t.Fatal(err)
	}

	notifs.next(t, "item.started")
	notifs.next(t, "agentMessage")

	if err := s.Send("mid"); err != nil {
		t.Fatal(err)
	}
	notifs.next(t, "agentMessage")

	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	notifs.next(t, "turn.completed")

	s.CloseInput()
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}

	wantSent := []string{"thread/start", "turn/start", "turn/steer", "turn/interrupt"}
	if got := fake.sent(); !equal(got, wantSent) {
		t.Fatalf("the fake received %v, want %v", got, wantSent)
	}

	wantSeen := []string{"item.started", "agentMessage", "agentMessage", "turn.completed"}
	if got := notifs.methods(); !equal(got, wantSeen) {
		t.Fatalf("OnEvent saw %v, want %v", got, wantSeen)
	}

	linesMu.Lock()
	gotLines := append([]string(nil), lines...)
	linesMu.Unlock()
	if len(gotLines) != len(wantSeen) {
		t.Fatalf("OnLine saw %d line(s), want %d: %v", len(gotLines), len(wantSeen), gotLines)
	}
	for i, line := range gotLines {
		var m struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if m.Method != wantSeen[i] {
			t.Fatalf("OnLine line %d is %q, want %q", i, m.Method, wantSeen[i])
		}
	}
}

func equal(a, b []string) bool {
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
