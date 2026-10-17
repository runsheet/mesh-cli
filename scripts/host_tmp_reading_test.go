// Tests scripts/host-tmp-reading.sh on a fixture /tmp built in a temp
// dir, TMPDIR_ROOT pointed at it and STAT at a fake stat that names
// each entry's owner from a map file, so two users' files can be read
// without root (ADR-020, this is a `Measure:` item's reading, never a
// verdict on a build).
package scripts

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostTmpFixture builds the fixture root: alice holds runsheet-a (100
// bytes), runsheet-b/ (a dir with 50 bytes in it), mesh-home-x (30),
// mesh-eval-1 (7), go-build123 (11) and stray (5); bob holds
// runsheet-bob (1000) and latchkey-bob (2000).
func hostTmpFixture(t *testing.T) (root, stat string) {
	t.Helper()
	dir := t.TempDir()
	root = filepath.Join(dir, "tmp")
	if err := os.MkdirAll(filepath.Join(root, "runsheet-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{"runsheet-b": "alice"}
	files := []struct {
		name, owner string
		size        int
	}{
		{"runsheet-a", "alice", 100},
		{"runsheet-b/inner", "", 50},
		{"mesh-home-x", "alice", 30},
		{"mesh-eval-1", "alice", 7},
		{"go-build123", "alice", 11},
		{"stray", "alice", 5},
		{"runsheet-bob", "bob", 1000},
		{"latchkey-bob", "bob", 2000},
	}
	for _, f := range files {
		writeSized(t, filepath.Join(root, f.name), f.size)
		if f.owner != "" {
			owners[f.name] = f.owner
		}
	}
	var m strings.Builder
	for name, owner := range owners {
		fmt.Fprintf(&m, "%s %s\n", name, owner)
	}
	ownersFile := filepath.Join(dir, "owners")
	if err := os.WriteFile(ownersFile, []byte(m.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	stat = filepath.Join(dir, "stat")
	script := "#!/usr/bin/env bash\n" +
		"# fake stat: stat -c %U -- <path>, owner from the map, else alice (a file the test adds later)\n" +
		"p=${@: -1}; n=${p##*/}\n" +
		"o=$(awk -v n=\"$n\" '$1 == n { print $2 }' " + ownersFile + ")\n" +
		"echo \"${o:-alice}\"\n"
	if err := os.WriteFile(stat, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, stat
}

func writeSized(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runHostTmpReading(t *testing.T, root, stat string, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"host-tmp-reading.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "TMPDIR_ROOT="+root, "STAT="+stat)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("host-tmp-reading.sh %v: %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}

func TestHostTmpReadingSyntax(t *testing.T) {
	out, err := exec.Command("bash", "-n", "host-tmp-reading.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n host-tmp-reading.sh: %v\n%s", err, out)
	}
}

func TestHostTmpReadingHelp(t *testing.T) {
	out, err := exec.Command("bash", "host-tmp-reading.sh", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "usage: scripts/host-tmp-reading.sh") {
		t.Fatalf("--help printed no usage line:\n%s", out)
	}
}

func TestHostTmpReadingNamedUserOnly(t *testing.T) {
	root, stat := hostTmpFixture(t)
	out := runHostTmpReading(t, root, stat, "alice")

	// runsheet-b's size is du's: its 50 bytes plus the dir's own.
	dirBytes := duBytes(t, filepath.Join(root, "runsheet-b"))
	for _, want := range []string{
		fmt.Sprintf("alice\trunsheet-*\tbytes=%d\tentries=2", 100+dirBytes),
		"alice\tlatchkey-*\tbytes=0\tentries=0",
		"alice\tmesh-home-*\tbytes=30\tentries=1",
		"alice\tmesh-eval*\tbytes=7\tentries=1",
		"alice\tmesh-session-*\tbytes=0\tentries=0",
		"alice\tgo-build*\tbytes=11\tentries=1",
		"alice\tother\tbytes=5\tentries=1",
		fmt.Sprintf("total\tbytes=%d\tentries=6", 100+dirBytes+30+7+11+5),
	} {
		if !containsLine(out, want) {
			t.Errorf("expected line %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "bob") || strings.Contains(out, "bytes=1000") || strings.Contains(out, "bytes=2000") {
		t.Errorf("bob's files read for alice alone:\n%s", out)
	}

	both := runHostTmpReading(t, root, stat, "alice", "bob")
	for _, want := range []string{
		"bob\trunsheet-*\tbytes=1000\tentries=1",
		"bob\tlatchkey-*\tbytes=2000\tentries=1",
		"bob\tother\tbytes=0\tentries=0",
		"all\tlatchkey-*\tbytes=2000\tentries=1",
	} {
		if !containsLine(both, want) {
			t.Errorf("expected line %q, got:\n%s", want, both)
		}
	}
}

func TestHostTmpReadingBaseline(t *testing.T) {
	root, stat := hostTmpFixture(t)
	baseline := filepath.Join(t.TempDir(), "baseline")
	if err := os.WriteFile(baseline, []byte(runHostTmpReading(t, root, stat, "alice")), 0o644); err != nil {
		t.Fatal(err)
	}

	same := runHostTmpReading(t, root, stat, "--baseline", baseline, "alice")
	for _, want := range []string{"(no change)", "added\tbytes=+0\tentries=+0", "unchanged"} {
		if !containsLine(same, want) {
			t.Errorf("unchanged fixture: expected line %q, got:\n%s", want, same)
		}
	}

	writeSized(t, filepath.Join(root, "runsheet-new"), 4096)
	grown := runHostTmpReading(t, root, stat, "--baseline", baseline, "alice")
	for _, want := range []string{
		"alice\trunsheet-*\tbytes=+4096\tentries=+1",
		"all\trunsheet-*\tbytes=+4096\tentries=+1",
		"added\tbytes=+4096\tentries=+1",
		"grew",
	} {
		if !containsLine(grown, want) {
			t.Errorf("after adding a file: expected line %q, got:\n%s", want, grown)
		}
	}
}

func duBytes(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("du", "-sb", path).Output()
	if err != nil {
		t.Fatalf("du -sb %s: %v", path, err)
	}
	var n int
	if _, err := fmt.Sscan(strings.Fields(string(out))[0], &n); err != nil {
		t.Fatalf("du output %q: %v", out, err)
	}
	return n
}
