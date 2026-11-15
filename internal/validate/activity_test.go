package validate

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type activity struct {
	mu   sync.Mutex
	what []string
}

func (a *activity) saw(what string) {
	a.mu.Lock()
	a.what = append(a.what, what)
	a.mu.Unlock()
}

func (a *activity) count(what string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, w := range a.what {
		if w == what {
			n++
		}
	}
	return n
}

type writes struct{ out, err int }

func (w writes) Run(_ context.Context, _ string, _ time.Duration, stdout, stderr io.Writer) (int, error) {
	for range w.out {
		io.WriteString(stdout, "a line\n")
	}
	for range w.err {
		io.WriteString(stderr, "a warning\n")
	}
	return 0, nil
}

// Every write a unit's command makes, to stdout or stderr, is the run
// showing it works; a command that writes nothing shows nothing.
func TestRunTellsOfEachWriteAndOfNoneWhenSilent(t *testing.T) {
	steps := []Step{{Name: "unit", Command: "go test ./..."}}
	var loud activity
	if _, err := Run(WithOnActivity(context.Background(), loud.saw), steps, writes{out: 3, err: 2}, Options{}); err != nil {
		t.Fatal(err)
	}
	if n := loud.count("output"); n != 5 {
		t.Fatalf("heard %d writes of 5: %v", n, loud.what)
	}
	var silent activity
	if _, err := Run(WithOnActivity(context.Background(), silent.saw), steps, writes{}, Options{}); err != nil {
		t.Fatal(err)
	}
	if len(silent.what) != 0 {
		t.Fatalf("a silent command was heard: %v", silent.what)
	}
}

// Busy says its work is under way at once and again while it goes, and
// nothing once it is done.
func TestBusySaysSoUntilDone(t *testing.T) {
	old := busyEvery
	busyEvery = 5 * time.Millisecond
	defer func() { busyEvery = old }()
	var a activity
	done := Busy(WithOnActivity(context.Background(), a.saw), "opening lane e2e-1")
	deadline := time.Now().Add(10 * time.Second)
	for a.count("opening lane e2e-1") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("heard %v", a.what)
		}
		time.Sleep(time.Millisecond)
	}
	done()
	done() // again is nothing
	time.Sleep(20 * time.Millisecond)
	n := a.count("opening lane e2e-1")
	time.Sleep(30 * time.Millisecond)
	if m := a.count("opening lane e2e-1"); m != n {
		t.Fatalf("heard %d more after done", m-n)
	}
	Busy(context.Background(), "nothing listens")()
}
