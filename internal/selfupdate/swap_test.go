package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSwap(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mesh")
	writeExe(t, exe, "old")

	if err := Swap(exe, []byte("new")); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := readFile(t, exe); got != "new" {
		t.Errorf("exe = %q, want new", got)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("exe mode = %v, want 0755", fi.Mode().Perm())
	}
	if got := readFile(t, exe+".prev"); got != "old" {
		t.Errorf(".prev = %q, want old", got)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Errorf(".new left behind: %v", err)
	}
}

func TestSwapReplacesAnOldPrev(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mesh")
	writeExe(t, exe, "v2")
	writeExe(t, exe+".prev", "v1")
	if err := Swap(exe, []byte("v3")); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := readFile(t, exe); got != "v3" {
		t.Errorf("exe = %q", got)
	}
	if got := readFile(t, exe+".prev"); got != "v2" {
		t.Errorf(".prev = %q", got)
	}
}

func TestSwapFailingMidwayLeavesTheOriginal(t *testing.T) {
	boom := errors.New("boom")
	for _, failAt := range []int{1, 2} {
		exe := filepath.Join(t.TempDir(), "mesh")
		writeExe(t, exe, "old")

		calls := 0
		rename = func(from, to string) error {
			calls++
			if calls == failAt {
				return boom
			}
			return os.Rename(from, to)
		}
		err := Swap(exe, []byte("new"))
		rename = os.Rename

		if !errors.Is(err, boom) {
			t.Fatalf("failAt %d: Swap = %v, want boom", failAt, err)
		}
		if got := readFile(t, exe); got != "old" {
			t.Errorf("failAt %d: exe = %q, want old", failAt, got)
		}
		fi, _ := os.Stat(exe)
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("failAt %d: exe mode = %v", failAt, fi.Mode().Perm())
		}
		if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
			t.Errorf("failAt %d: .new left behind", failAt)
		}
	}
}

func TestSwapMissingExe(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mesh")
	if err := Swap(exe, []byte("new")); err == nil {
		t.Fatal("Swap with no current executable succeeded")
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Error(".new left behind")
	}
}
