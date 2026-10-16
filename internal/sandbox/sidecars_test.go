package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestContainerPorts(t *testing.T) {
	got := ContainerPorts([]string{"5432:5432", "6379", "8080:80/tcp", "127.0.0.1:9000:9001", "80", "x"})
	if want := []int{5432, 6379, 80, 9001}; !slices.Equal(got, want) {
		t.Fatalf("ports = %v, want %v", got, want)
	}
}

// The services' budget is split evenly between the sidecars, each with
// its own pids cap.
func TestSplitLimits(t *testing.T) {
	for _, tc := range []struct {
		budget Limits
		n      int
		want   Limits
	}{
		{Limits{CPUs: 2, Memory: "4g"}, 2, Limits{CPUs: 1, Memory: "2048m", Pids: SidecarPids}},
		{Limits{CPUs: 2, Memory: "4g"}, 1, Limits{CPUs: 2, Memory: "4096m", Pids: SidecarPids}},
		{Limits{CPUs: 3, Memory: "1536m"}, 3, Limits{CPUs: 1, Memory: "512m", Pids: SidecarPids}},
		{Limits{}, 2, Limits{Pids: SidecarPids}},
	} {
		if got := SplitLimits(tc.budget, tc.n); got != tc.want {
			t.Errorf("SplitLimits(%+v, %d) = %+v, want %+v", tc.budget, tc.n, got, tc.want)
		}
	}
}

// A required service refused for its registry makes no pod: nothing is
// pulled, and the others are not started.
func TestStartRefusedRequiredMakesNoPod(t *testing.T) {
	rt := &Fake{}
	sb := &Sandbox{Runtime: rt, TempDir: t.TempDir()}
	sess, err := sb.Open("g/1", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sts, err := sess.Start(context.Background(), []Service{
		{Name: "cache", Image: "redis:7", Ports: []string{"6379"}},
		{Name: "db", Image: "evil.example/pg", Required: true, Refused: true, Notes: []string{"image evil.example/pg refused: registry evil.example is not allowed (allowed: docker.io)"}},
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Calls) != 0 {
		t.Fatalf("calls: %v", rt.Calls)
	}
	if sts[1].Ready || sts[1].Why != "image evil.example/pg refused: registry evil.example is not allowed (allowed: docker.io)" {
		t.Fatalf("db = %+v", sts[1])
	}
	if sts[0].Ready || sts[0].Why != "not started: the required service db was refused" {
		t.Fatalf("cache = %+v", sts[0])
	}
}

// A declared health check runs in the sidecar until it passes; with
// none, the first port is probed over TCP; a check that never passes
// fails after its retries, or at the bound.
func TestStartWaitsForHealth(t *testing.T) {
	rt := &Fake{}
	tries := 0
	rt.Health = func(sidecar string, cmd []string, port int) error {
		switch sidecar {
		case "pg":
			if !slices.Equal(cmd, []string{"sh", "-c", "pg_isready -U postgres"}) {
				t.Errorf("cmd = %q", cmd)
			}
			if tries++; tries < 2 {
				return errors.New("no")
			}
			return nil
		case "bad":
			return errors.New("unhealthy")
		}
		if port == 6379 {
			return nil
		}
		return errors.New("connection refused")
	}
	sb := &Sandbox{Runtime: rt, TempDir: t.TempDir(), Config: Config{Image: "node-image"}}
	sess, err := sb.Open("g/1", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sts, err := sess.Start(context.Background(), []Service{
		{Name: "pg", Image: "postgres:16", Ports: []string{"5432:5432"}, Health: &HealthCheck{Cmd: "pg_isready -U postgres", Interval: 5 * time.Millisecond}},
		{Name: "redis", Image: "redis:7", Ports: []string{"6379"}},
		{Name: "bad", Image: "x:1", Health: &HealthCheck{Cmd: "false", Interval: time.Millisecond, Retries: 2}},
		{Name: "slow", Image: "y:1", Ports: []string{"9999"}},
	}, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !sts[0].Ready || !sts[1].Ready {
		t.Fatalf("pg, redis: %+v", sts[:2])
	}
	if sts[2].Why != "health check failed 2 times: unhealthy" {
		t.Fatalf("bad = %+v", sts[2])
	}
	if sts[3].Why != "TCP connect to 127.0.0.1:9999 did not pass within 100ms: connection refused" {
		t.Fatalf("slow = %+v", sts[3])
	}
	if err := sess.Run(context.Background(), RunSpec{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if len(rt.Pods) != 1 || rt.Calls[len(rt.Calls)-1] != "remove "+rt.Pods[0].Name {
		t.Fatalf("the agent runs in the services' pod, removed after: %v", rt.Calls)
	}
	if !strings.HasPrefix(rt.Calls[len(rt.Calls)-2], "agent "+rt.Pods[0].Name) {
		t.Fatalf("calls: %v", rt.Calls)
	}
}

func TestServicesLineAndListing(t *testing.T) {
	sts := []ServiceStatus{
		{Name: "postgres", Image: "postgres:16", Ports: []int{5432}, Ready: true},
		{Name: "minio", Image: "minio/minio", Ports: []int{9000, 9001}, Ready: true},
		{Name: "redis", Image: "redis:7", Ports: []int{6379}, Why: "did not start: pull denied"},
	}
	want := "The services this repository declares run beside you and are ready on 127.0.0.1 (also listed in `$HOME/.mesh-services`): postgres (postgres:16) at 127.0.0.1:5432; minio (minio/minio) at 127.0.0.1:9000, 127.0.0.1:9001. Missing — it could not start, so a test that needs it fails here for that reason, not your code's: redis (redis:7): did not start: pull denied."
	if got := ServicesLine(sts); got != want {
		t.Fatalf("line =\n%s\nwant\n%s", got, want)
	}
	wantFile := "postgres\tpostgres:16\t127.0.0.1:5432\tready\nminio\tminio/minio\t127.0.0.1:9000,127.0.0.1:9001\tready\nredis\tredis:7\t127.0.0.1:6379\tfailed: did not start: pull denied\n"
	if got := string(ServicesListing(sts)); got != wantFile {
		t.Fatalf("listing = %q", got)
	}
	if ServicesLine(nil) != "" {
		t.Fatal("no services, no line")
	}
}

func TestSidecarAndProbeArgs(t *testing.T) {
	pod := Pod{Name: "mesh-g-1-ab12cd34-0"}
	args := strings.Join(SidecarRunArgs(pod, SidecarSpec{Name: "Postgres", Image: "postgres:16", Env: []string{"POSTGRES_PASSWORD=pw"}, Command: []string{"-c", "fsync=off"}, Limits: Limits{CPUs: 1, Memory: "2048m", Pids: 1024}}), " ")
	want := "run -d --pod mesh-g-1-ab12cd34-0 --name mesh-g-1-ab12cd34-0-postgres --security-opt no-new-privileges --pull missing --cpus 1 --memory 2048m --memory-swap 2048m --pids-limit 1024 --env POSTGRES_PASSWORD --env GOMAXPROCS --env OMP_NUM_THREADS --env MAKEFLAGS postgres:16 -c fsync=off"
	if args != want {
		t.Fatalf("sidecar args =\n%s\nwant\n%s", args, want)
	}
	if strings.Contains(args, "pw") {
		t.Fatal("an env value is on the command line")
	}
	probe := strings.Join(ProbeTCPArgs(pod, "img", 5432), " ")
	if probe != "run --rm --pod mesh-g-1-ab12cd34-0 --cap-drop all --security-opt no-new-privileges --pull missing --unsetenv-all img bash -c exec 3<>/dev/tcp/127.0.0.1/5432" {
		t.Fatalf("probe = %s", probe)
	}
}
