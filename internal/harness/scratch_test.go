//go:build !windows

package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// sessionDirsIn lists the mesh-session-* dirs under root.
func sessionDirsIn(root string) []string {
	m, _ := filepath.Glob(filepath.Join(root, "mesh-session-*"))
	return m
}

// A job with no work dir keeps its session dir under its own TMPDIR —
// else the harness's ScratchRoot — never the process's TMPDIR, and the
// dir is gone once the session ends.
func TestNoWorkspaceSessionDirIsNotInProcessTmp(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	bin := slowClaude(t)
	procTmp := t.TempDir()
	t.Setenv("TMPDIR", procTmp)
	for _, tc := range []struct {
		name        string
		jobTmp      string
		scratchRoot string
	}{
		{name: "job TMPDIR", jobTmp: filepath.Join(t.TempDir(), "scratch-build-1")},
		{name: "scratch root", scratchRoot: filepath.Join(t.TempDir(), ".scratch")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.jobTmp + tc.scratchRoot
			env := []string{"SLOW_EVENTS=2", "SLOW_SLEEP=0.05"}
			if tc.jobTmp != "" {
				env = append(env, "TMPDIR="+tc.jobTmp)
			}
			h := &ClaudeCode{Binary: bin, Unit: sessionunit.Detached{}, ScratchRoot: tc.scratchRoot}
			var during, inProc []string
			res, err := h.Run(context.Background(), Job{Kind: KindPlan, Env: env}, func(p Progress) {
				if p.Text == "step 1" {
					during, inProc = sessionDirsIn(want), sessionDirsIn(procTmp)
				}
			})
			if err != nil || !res.Success {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			if len(during) != 1 || len(inProc) != 0 {
				t.Fatalf("session dir in %s: %v; in the process TMPDIR: %v", want, during, inProc)
			}
			if _, err := os.Stat(during[0]); !os.IsNotExist(err) {
				t.Fatalf("session dir %s outlived the session: %v", during[0], err)
			}
			if ents, _ := os.ReadDir(procTmp); len(ents) != 0 {
				t.Fatalf("the process TMPDIR holds %v", ents)
			}
		})
	}
}

// RemoveSession removes the state's temp dirs even when the agent left a
// read-only tree in them, as a Go module cache is.
func TestRemoveSessionRemovesAReadOnlyScratch(t *testing.T) {
	ws, scratch := t.TempDir(), filepath.Join(t.TempDir(), "build-1")
	ro := filepath.Join(scratch, "mod", "pkg")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "x.go"), []byte("package x"), 0o400); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{ro, filepath.Dir(ro)} {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	dir := sessionunit.Dir(ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sessionunit.WriteState(dir, sessionunit.State{TempDirs: []string{scratch}}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSession(ws); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, scratch} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("%s left: %v", d, err)
		}
	}
}

// A Codex session's CODEX_HOME is gone once Run returns — after a
// session that ended, one that failed, and one the node gave up on (its
// ctx cancelled mid-session).
func TestCodexHomeIsGoneAfterTheSession(t *testing.T) {
	t.Setenv("MESH_TEST_CODEX_KEY", "sk-x")
	for _, tc := range []struct {
		name    string
		fixture string
		cancel  bool
	}{
		{name: "ended", fixture: "codex_exec_apikey.jsonl"},
		{name: "failed", fixture: "codex_exec_error.jsonl"},
		{name: "given up", fixture: "codex_exec_apikey.jsonl", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, root, ws := t.TempDir(), t.TempDir(), t.TempDir()
			bin := fakeCodex(t, out, tc.fixture)
			if tc.cancel {
				// the agent never finishes on its own: the node stops it
				script := "#!/bin/sh\necho \"$CODEX_HOME\" > " + out + "/home\nexec sleep 30\n"
				if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			c := &Codex{Binary: bin, HomeRoot: root, Account: CodexAccount{Name: "api", APIKeyEnv: "MESH_TEST_CODEX_KEY"}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				go func() {
					for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
						if _, err := os.Stat(filepath.Join(out, "home")); err == nil {
							cancel()
							return
						}
					}
				}()
			}
			_, err := c.Run(ctx, Job{Kind: KindEvaluate, Workspace: ws, Prompt: "hi", Model: "gpt-6-luna"}, nil)
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the cancel", err)
			}
			home := strings.TrimSpace(readFile(t, filepath.Join(out, "home")))
			if filepath.Dir(home) != root || !strings.HasPrefix(filepath.Base(home), "home-") {
				t.Fatalf("CODEX_HOME %q is not a home-* under %s", home, root)
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("CODEX_HOME %s outlived the session: %v", home, err)
			}
			if ents, _ := os.ReadDir(root); len(ents) != 0 {
				t.Fatalf("the home root holds %v", ents)
			}
		})
	}
}
