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

// An oversized log whose rotation fails on open is an error naming the
// log, never a panic, and the file is left as it was.
func TestRotatingFileOpenFailsWhenAnOversizedLogCannotRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	// node.log.1 a non-empty directory: the rename cannot happen
	if err := os.MkdirAll(filepath.Join(path+".1", "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRotating(path, 1000, 1)
	if err == nil || r != nil {
		t.Fatalf("OpenRotating = %v, %v; want nil and an error", r, err)
	}
	if want := "log " + path + ": rotate on open (5000 bytes, cap 1000): "; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error = %q, want it to start %q", err, want)
	}
	if st, err := os.Stat(path); err != nil || st.Size() != 5000 {
		t.Fatalf("node.log after the failed open: %v %v, want 5000 bytes", st, err)
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

// One write larger than the cap is cut to it, in a fresh file, and says
// so: the log never passes the cap however large a line is.
func TestRotatingFileCutsAWriteLargerThanTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.log")
	const max = 1000
	r, err := OpenRotating(path, max, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Write([]byte("before\n")); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("z"), 5000)
	n, err := r.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(big))
	}
	got, _ := os.ReadFile(path)
	if len(got) != max {
		t.Fatalf("node.log is %d bytes, want the cap %d", len(got), max)
	}
	note := "\n[log: a 5000-byte write cut to the 1000-byte cap]\n"
	if !strings.HasSuffix(string(got), note) || strings.Trim(strings.TrimSuffix(string(got), note), "z") != "" {
		t.Fatalf("node.log = %q", got)
	}
	if prev, _ := os.ReadFile(path + ".1"); string(prev) != "before\n" {
		t.Fatalf("node.log.1 = %q, want the line before", prev)
	}
	// and the next line rotates the cut one away
	if _, err := r.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() != int64(len("after\n")) {
		t.Fatalf("node.log is %d bytes after the cut write, want %d", st.Size(), len("after\n"))
	}
}

// A rotation that fails refuses the write with the error, says so once
// on Errors, and never appends past the cap; once the rotation works
// again the lines flow and that is said too.
func TestRotatingFileRefusesAWriteItCannotRotateFor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.log")
	const max = 300
	var said bytes.Buffer
	r, err := OpenRotating(path, max, 1)
	if err != nil {
		t.Fatal(err)
	}
	r.Errors = &said
	defer r.Close()
	line := []byte(strings.Repeat("x", 99) + "\n")
	for i := 0; i < 3; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	// node.log.1 a non-empty directory: the rename cannot happen
	if err := os.MkdirAll(filepath.Join(path+".1", "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		n, err := r.Write(line)
		if err == nil || n != 0 {
			t.Fatalf("write %d past the cap with rotation failing = %d, %v; want 0 and an error", i, n, err)
		}
		if !strings.Contains(err.Error(), "log "+path+": rotate: ") {
			t.Fatalf("error = %v", err)
		}
	}
	if st, _ := os.Stat(path); st.Size() != max {
		t.Fatalf("node.log is %d bytes while rotation fails, want %d", st.Size(), max)
	}
	lines := strings.Split(strings.TrimSuffix(said.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "mesh node: log "+path+": rotate: ") ||
		!strings.HasSuffix(lines[0], "; the line is dropped rather than written past the 300-byte cap") {
		t.Fatalf("Errors got %q, want one rotate line", said.String())
	}
	if err := os.RemoveAll(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write(line); err != nil {
		t.Fatalf("write once rotation works again: %v", err)
	}
	if st, _ := os.Stat(path); st.Size() != int64(len(line)) {
		t.Fatalf("node.log is %d bytes after rotating, want %d", st.Size(), len(line))
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != max {
		t.Fatalf("node.log.1: %v %v", st, err)
	}
	lines = strings.Split(strings.TrimSuffix(said.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "mesh node: log "+path+": rotating again, after dropping lines for: log "+path+": rotate: ") {
		t.Fatalf("Errors got %q, want the recovery said", said.String())
	}
}
