package session

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness/session/sessiontest"
)

// A session given Spawn runs its process through it — a v1 node's lease
// pod — on the session's own pipes: the argv is the binary, the stream
// flags and the args; turns and control go in and the output comes back
// as on the host; the spawned process's end is the session's.
func TestASpawnedSessionRunsThroughSpawn(t *testing.T) {
	o := opts(t, nil)
	var argv []string
	o.Spawn = func(ctx context.Context, a []string, stdin io.Reader, stdout, stderr io.Writer) error {
		argv = a
		cmd := exec.CommandContext(ctx, a[0], a[1:]...)
		cmd.Env = append(os.Environ(), sessiontest.Env+"=1")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		return cmd.Run()
	}
	o.Env = nil
	ev := newEvents()
	o.OnEvent = ev.on
	s, err := Start(context.Background(), o, "first")
	if err != nil {
		t.Fatal(err)
	}
	ev.next(t, "result")
	if _, err := s.SetModel(context.Background(), "sonnet"); err != nil {
		t.Fatal(err)
	}
	s.CloseInput()
	res, err := s.Wait()
	if err != nil || !res.Success || res.Text != "got: first" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(argv) < 3 || argv[0] != o.Binary || argv[1] != "-p" || argv[len(argv)-1] != "haiku" {
		t.Fatalf("argv %q", argv)
	}
}

// A spawned process that ends at once — a pod killed before the first
// message — is the session's end: Wait answers its error and a turn sent
// after it fails rather than blocking.
func TestASpawnedSessionThatEndsAtOnce(t *testing.T) {
	o := opts(t, nil)
	killed := errors.New("sandbox: agent exited 137")
	o.Spawn = func(context.Context, []string, io.Reader, io.Writer, io.Writer) error { return killed }
	s, _ := Start(context.Background(), o, "first")
	if s == nil {
		t.Fatal("no session")
	}
	if _, err := s.Wait(); !errors.Is(err, killed) {
		t.Fatalf("wait: %v", err)
	}
	if err := s.Send("late"); err == nil {
		t.Fatal("a turn after the end went in")
	}
}
