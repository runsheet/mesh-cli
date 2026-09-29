package node

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Writing past the cap rotates: node.log.1 appears, the active file
// stays under the cap, and no more than Keep rotated files are kept.
func TestRotatingFileRotatesAtTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.log")
	const max, keep = 1000, 3
	r, err := OpenRotating(path, max, keep)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	line := []byte(strings.Repeat("x", 99) + "\n") // 100 bytes
	for i := 0; i < 10; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("rotated at exactly the cap: %v", err)
	}
	if _, err := r.Write(line); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("no node.log.1 past the cap: %v", err)
	}
	if st.Size() != 1000 {
		t.Fatalf("node.log.1 is %d bytes, want 1000", st.Size())
	}
	if st, _ := os.Stat(path); st.Size() != 100 {
		t.Fatalf("node.log is %d bytes after rotating, want 100", st.Size())
	}
	// far past the cap: only keep rotated files, the active one small
	for i := 0; i < 100; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := os.Stat(path); st.Size() > max {
		t.Fatalf("node.log is %d bytes, over the cap %d", st.Size(), max)
	}
	for n := 1; n <= keep; n++ {
		if _, err := os.Stat(fmt.Sprintf("%s.%d", path, n)); err != nil {
			t.Fatalf("node.log.%d: %v", n, err)
		}
	}
	if _, err := os.Stat(fmt.Sprintf("%s.%d", path, keep+1)); !os.IsNotExist(err) {
		t.Fatalf("node.log.%d kept past Keep=%d: %v", keep+1, keep, err)
	}
	if got := LogFiles(path, keep); len(got) != keep+1 || got[0] != path+".3" || got[keep] != path {
		t.Fatalf("LogFiles: %v", got)
	}
}

// A log a service appended to unbounded is rotated when the node opens
// it: the new node.log starts empty.
func TestRotatingFileRotatesAnOversizedLogOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRotating(path, 1000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if st, _ := os.Stat(path); st.Size() != 0 {
		t.Fatalf("node.log is %d bytes, want 0", st.Size())
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != 5000 {
		t.Fatalf("node.log.1: %v %v", st, err)
	}
}

// The node's own lines go to node.log and a session's text to
// sessions.log, never the other.
func TestNodeLogsKeepSessionsApart(t *testing.T) {
	home := t.TempDir()
	logs, err := OpenNodeLogs(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs.Node.SetFlags(0)
	logs.Sessions.SetFlags(0)
	logs.Node.Printf("capacity 6 → 8")
	logs.Sessions.Printf("session: I'll start by reading the brief")
	fmt.Fprint(logs.SessionsOut, "$ git fetch origin\n")
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	nodeLog, _ := os.ReadFile(ServiceLog(home))
	sessLog, _ := os.ReadFile(SessionsLog(home))
	if got, want := string(nodeLog), "mesh node: capacity 6 → 8\n"; got != want {
		t.Fatalf("node.log = %q, want %q", got, want)
	}
	if got, want := string(sessLog), "mesh node: session: I'll start by reading the brief\n$ git fetch origin\n"; got != want {
		t.Fatalf("sessions.log = %q, want %q", got, want)
	}
}
