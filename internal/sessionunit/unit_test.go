package sessionunit

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"
)

// SystemdScope's command line, exactly.
func TestSystemdScopeArgv(t *testing.T) {
	spec := Spec{Kind: "build", Subject: "item-1", Binary: "/usr/local/bin/claude", Args: []string{"-p", "--output-format", "stream-json", "--verbose", "--model", "sonnet"}}
	got := SystemdScope{}.Argv(spec, "mesh-session-build-item-1-0a1b2c3d")
	want := []string{
		"systemd-run", "--user", "--scope", "--unit=mesh-session-build-item-1-0a1b2c3d", "--collect", "--quiet", "--",
		"/usr/local/bin/claude", "-p", "--output-format", "stream-json", "--verbose", "--model", "sonnet",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	if got := (SystemdScope{SystemdRun: "/usr/bin/systemd-run"}).Argv(Spec{Binary: "claude"}, "u")[0]; got != "/usr/bin/systemd-run" {
		t.Fatalf("binary %q", got)
	}
}

func TestUnitName(t *testing.T) {
	re := regexp.MustCompile(`^mesh-session-plan-goal_x_y-[0-9a-f]{8}$`)
	if n := UnitName("plan", "goal:x/y"); !re.MatchString(n) {
		t.Fatalf("UnitName = %q", n)
	}
	if n := UnitName("", ""); !regexp.MustCompile(`^mesh-session-session-none-[0-9a-f]{8}$`).MatchString(n) {
		t.Fatalf("UnitName = %q", n)
	}
	if UnitName("build", "i") == UnitName("build", "i") {
		t.Fatal("two sessions of one item share a unit name")
	}
}

func TestDir(t *testing.T) {
	if got := Dir("/w/goal/1/"); got != "/w/goal/1.session" {
		t.Fatalf("Dir = %q", got)
	}
}

// A Follower waits for a file still being written and ends once the
// writer is gone and the file has stopped growing; a last line without
// its newline is still read.
func TestFollowWhileWritten(t *testing.T) {
	PollInterval = 10 * time.Millisecond
	path := filepath.Join(t.TempDir(), OutputFile)
	w, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for _, l := range []string{"one\n", "two\n", "thr", "ee\n", "four"} {
			time.Sleep(30 * time.Millisecond)
			w.WriteString(l)
		}
		w.Close()
		close(done)
	}()
	fl, err := Follow(t.Context(), path, 0, func() bool {
		select {
		case <-done:
			return false
		default:
			return true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()
	var reads []string
	buf := make([]byte, 1024)
	for {
		n, err := fl.Read(buf)
		if n > 0 {
			reads = append(reads, string(buf[:n]))
		}
		if err != nil {
			break
		}
	}
	if want := []string{"one\n", "two\n", "three\n", "four"}; !reflect.DeepEqual(reads, want) {
		t.Fatalf("reads %q, want %q", reads, want)
	}
	if fl.Offset() != int64(len("one\ntwo\nthree\nfour")) {
		t.Fatalf("offset %d", fl.Offset())
	}
}
