package session

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/harness/session/sessiontest"
)

// TestMain runs this test binary as the fake claude when the session
// starts it (sessiontest.Env=1).
func TestMain(m *testing.M) {
	sessiontest.Main()
	os.Exit(m.Run())
}

func opts(t *testing.T, onEvent func(harness.Event)) Options {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Options{Binary: self, Args: []string{"--model", "haiku"}, Dir: t.TempDir(),
		Env: append(os.Environ(), sessiontest.Env+"=1"), OnEvent: onEvent}
}

// events collects a session's events.
type events struct {
	mu  sync.Mutex
	all []harness.Event
	ch  chan harness.Event
}

func newEvents() *events { return &events{ch: make(chan harness.Event, 64)} }

func (e *events) on(ev harness.Event) {
	e.mu.Lock()
	e.all = append(e.all, ev)
	e.mu.Unlock()
	e.ch <- ev
}

// next waits for the next event of type typ.
func (e *events) next(t *testing.T, typ string) harness.Event {
	t.Helper()
	for {
		select {
		case ev := <-e.ch:
			if ev.Type == typ {
				return ev
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s event", typ)
		}
	}
}

// The first message goes in when the session starts and the input stays
// open: a second turn goes in after the first ended, set_model moves the
// next turn to another model in the same session, and closing the input
// ends it. The result sums the turns and keeps the spend by model.
func TestASessionTakesTurnsAndControlWhileItRuns(t *testing.T) {
	ev := newEvents()
	s, err := Start(context.Background(), opts(t, ev.on), "first")
	if err != nil {
		t.Fatal(err)
	}
	if r := ev.next(t, "result"); r.Result != "got: first" {
		t.Fatalf("first turn: %+v", r)
	}
	model, err := s.SetModel(context.Background(), "sonnet")
	if err != nil || model != "claude-sonnet" {
		t.Fatalf("set_model: %q %v", model, err)
	}
	if err := s.Send("second"); err != nil {
		t.Fatal(err)
	}
	if init := ev.next(t, "system"); init.Model != "claude-sonnet" {
		t.Fatalf("the second turn runs on %q", init.Model)
	}
	ev.next(t, "result")
	s.CloseInput()
	res, err := s.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || res.Text != "got: second" || res.Turns != 2 || res.Results != 2 || res.SessionID != "fake-session" {
		t.Fatalf("%+v", res)
	}
	if len(res.Models) != 2 || res.Models["claude-haiku"].OutputTokens != 1 || res.Models["claude-sonnet"].CostUSD != 0.01 || res.CostUSD != 0.02 {
		t.Fatalf("models %+v cost %v", res.Models, res.CostUSD)
	}
	if err := s.Send("late"); err != ErrClosed {
		t.Fatalf("a turn after the end: %v", err)
	}
}

// Closing the input while a turn runs lets that turn finish first.
func TestClosingTheInputLetsTheTurnFinish(t *testing.T) {
	s, err := Start(context.Background(), opts(t, nil), "slow")
	if err != nil {
		t.Fatal(err)
	}
	s.CloseInput()
	s.CloseInput()
	res, err := s.Wait()
	if err != nil || res.Text != "got: slow" || !res.Success {
		t.Fatalf("%+v %v", res, err)
	}
}

// An interrupt ends the turn in flight in an error result and the
// session takes the next turn.
func TestAnInterruptEndsTheTurnInFlight(t *testing.T) {
	ev := newEvents()
	s, err := Start(context.Background(), opts(t, ev.on), "hang")
	if err != nil {
		t.Fatal(err)
	}
	ev.next(t, "assistant")
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := ev.next(t, "result"); r.Subtype != "error_during_execution" || !r.IsError {
		t.Fatalf("the interrupted turn: %+v", r)
	}
	s.Send("after")
	ev.next(t, "result")
	s.CloseInput()
	res, _ := s.Wait()
	if !res.Success || res.Text != "got: after" {
		t.Fatalf("%+v", res)
	}
}

// A control request the CLI refuses is an error naming why, and the
// session carries on.
func TestARefusedControlRequestIsAnError(t *testing.T) {
	ev := newEvents()
	s, err := Start(context.Background(), opts(t, ev.on), "first")
	if err != nil {
		t.Fatal(err)
	}
	ev.next(t, "result")
	if _, err := s.SetModel(context.Background(), "refuse"); err == nil || !strings.Contains(err.Error(), "no such model") {
		t.Fatalf("a refused set_model: %v", err)
	}
	s.CloseInput()
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetModel(context.Background(), "sonnet"); err != ErrClosed {
		t.Fatalf("set_model after the end: %v", err)
	}
}

// Cancelling the session's ctx kills it, and a control request waiting
// on it returns.
func TestCancellingKillsTheSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ev := newEvents()
	s, err := Start(ctx, opts(t, ev.on), "hang")
	if err != nil {
		t.Fatal(err)
	}
	ev.next(t, "assistant")
	cancel()
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the session outlived its ctx")
	}
	if _, err := s.Wait(); err == nil {
		t.Fatal("a killed session ended cleanly")
	}
}
