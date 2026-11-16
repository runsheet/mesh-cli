package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The test binary is mesh itself when MESH_TEST_MAIN is set, so a test
// runs the command line as an operator — or a session — would.
func TestMain(m *testing.M) {
	if os.Getenv("MESH_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// `echo <90KB> | mesh tool-trim --max-kb 16` prints the head and tail
// with the trim note between.
func TestToolTrim(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 90*1024; i++ {
		fmt.Fprintf(&b, "=== RUN TestSomething/%05d --- PASS (0.00s)\n", i)
	}
	in := b.String()
	cmd := exec.Command("bash", "-c", `echo "$INPUT" | "$MESH" tool-trim --max-kb 16`)
	cmd.Env = append(os.Environ(), "MESH_TEST_MAIN=1", "MESH="+os.Args[0], "INPUT="+strings.TrimSuffix(in, "\n"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("tool-trim: %v", err)
	}
	got := string(out)
	if len(got) >= 17*1024 {
		t.Errorf("printed %d bytes, want under 17 KB", len(got))
	}
	if !strings.HasPrefix(got, "=== RUN TestSomething/00000 ") || !strings.Contains(got, "\n[mesh: trimmed ") {
		t.Errorf("not the trimmed text: %q", got[:200])
	}
	lines := strings.Split(strings.TrimSuffix(in, "\n"), "\n")
	if !strings.HasSuffix(got, lines[len(lines)-1]+"\n") {
		t.Errorf("does not end with the last line: %q", got[len(got)-100:])
	}
}

// `mesh node status` with no node running prints the v1 node's own
// not-running line and nothing of the legacy node's.
func TestNodeStatusWithNoNodeRunning(t *testing.T) {
	cmd := exec.Command(os.Args[0], "node", "status")
	cmd.Env = append(os.Environ(), "MESH_TEST_MAIN=1", "HOME="+t.TempDir(), "PATH="+t.TempDir(), "MESH_TOKEN=rs_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mesh node status: %v\n%s", err, out)
	}
	if want := "This machine: no Mesh v1 node running (mesh node run).\n"; string(out) != want {
		t.Fatalf("mesh node status printed %q, want %q", out, want)
	}
}

// `mesh docs-check DIR` prints each problem and a summary, and exits 1
// when there are problems, 0 when there are none: the command the docs
// suite of a repository that holds only documents names.
func TestDocsCheck(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/README.md", []byte("# Handbook\n\nSee [the plan](plan.md).\n"), 0o644)
	cmd := exec.Command(os.Args[0], "docs-check", dir)
	cmd.Env = append(os.Environ(), "MESH_TEST_MAIN=1")
	out, err := cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("docs-check on a broken link: err %v, want exit 1\n%s", err, out)
	}
	if !strings.Contains(string(out), `README.md:3: the link to "plan.md"`) || !strings.Contains(string(out), "1 problems") {
		t.Fatalf("printed %q", out)
	}
	os.WriteFile(dir+"/plan.md", []byte("# Plan\n"), 0o644)
	cmd = exec.Command(os.Args[0], "docs-check", dir)
	cmd.Env = append(os.Environ(), "MESH_TEST_MAIN=1")
	if out, err = cmd.Output(); err != nil || !strings.Contains(string(out), "no problems") {
		t.Fatalf("docs-check on a good tree: err %v\n%s", err, out)
	}
}
