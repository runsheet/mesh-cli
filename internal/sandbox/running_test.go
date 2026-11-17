package sandbox

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

// RunningContainers is one `podman ps` of the running containers, no
// --all and no inspect, read into each one's id, first name and host
// pid. The row is dl360's podman 4.9 shape: without --pod it leaves
// PodName empty, so a container is the lease's by its name alone.
func TestRunningContainers(t *testing.T) {
	var got [][]string
	p := &Podman{Exec: func(_ context.Context, args, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		got = append(got, args)
		io.WriteString(stdout, `[
 {"Id":"6ab9b060d3b9","Names":["mesh-v1-3da102f5-b10d-573c-b9ee-b5aec87bd10a-postgres"],"Pid":108719,"Pod":"71729ea7cbf3","PodName":"","IsInfra":false,"State":"running"},
 {"Id":"de66f3d7a74c","Names":["71729ea7cbf3-infra"],"Pid":108713,"Pod":"71729ea7cbf3","PodName":"","IsInfra":true,"State":"running"},
 {"Id":"0000","Names":[],"Pid":0}
]`)
		return nil
	}}
	cs, err := p.RunningContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !slices.Equal(got[0], []string{"ps", "--format", "json"}) {
		t.Fatalf("podman calls %q", got)
	}
	want := []RunningContainer{
		{ID: "6ab9b060d3b9", Name: "mesh-v1-3da102f5-b10d-573c-b9ee-b5aec87bd10a-postgres", PID: 108719},
		{ID: "de66f3d7a74c", Name: "71729ea7cbf3-infra", PID: 108713},
		{ID: "0000"},
	}
	if !slices.Equal(cs, want) {
		t.Fatalf("containers %+v, want %+v", cs, want)
	}

	if cs, err := ParseRunning("  \n"); err != nil || cs != nil {
		t.Fatalf("no output: %+v %v", cs, err)
	}
	if _, err := ParseRunning("not json"); err == nil {
		t.Fatal("output that is not JSON was read")
	}
	p.Exec = func(context.Context, []string, []string, io.Reader, io.Writer, io.Writer) error {
		return errors.New("exit status 125")
	}
	if _, err := p.RunningContainers(context.Background()); err == nil {
		t.Fatal("a podman ps that failed answered no error")
	}
}
