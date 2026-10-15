package validate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// recordingRunner records each command it is given and answers with
// its scripted exit code and output.
type recordingRunner struct {
	scriptedRunner
	commands []string
	timeouts []time.Duration
}

func (r *recordingRunner) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	r.commands = append(r.commands, cmd)
	r.timeouts = append(r.timeouts, timeout)
	return r.scriptedRunner.Run(ctx, cmd, timeout, stdout, stderr)
}

func touch(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The install a checkout needs is read from its files: the lockfile
// picks the package manager and its frozen install, no lockfile is a
// plain npm install, no package.json is none, and a node_modules
// already there — the builder's workspace — is none (ADR-013 amendment
// 2026-10-15).
func TestInstallCommandFollowsTheCheckout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  string
	}{
		{"no package.json", []string{"go.mod"}, ""},
		{"npm lockfile", []string{"package.json", "package-lock.json"}, "npm ci"},
		{"shrinkwrap", []string{"package.json", "npm-shrinkwrap.json"}, "npm ci"},
		{"pnpm lockfile", []string{"package.json", "pnpm-lock.yaml", "package-lock.json"}, "pnpm install --frozen-lockfile"},
		{"yarn lockfile", []string{"package.json", "yarn.lock"}, "yarn install --frozen-lockfile"},
		{"no lockfile", []string{"package.json"}, "npm install"},
		{"already installed", []string{"package.json", "package-lock.json", "node_modules/.package-lock.json"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			touch(t, dir, tc.files...)
			if got := InstallCommand(dir); got != tc.want {
				t.Fatalf("InstallCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

// Install runs the checkout's install once through the runner, under a
// suite's default timeout, and says what it ran; a checkout that needs
// none runs nothing.
func TestInstallRunsTheCheckoutsInstall(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "package.json", "package-lock.json")
	r := &recordingRunner{scriptedRunner: scriptedRunner{output: "added 412 packages\n"}}
	cmd, took, err := Install(context.Background(), dir, r)
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "npm ci" || len(r.commands) != 1 || r.commands[0] != "npm ci" || r.timeouts[0] != DefaultTimeout || took < 0 {
		t.Fatalf("ran %v (%v) in %s, said %q", r.commands, r.timeouts, took, cmd)
	}

	none := t.TempDir()
	touch(t, none, "go.mod")
	r = &recordingRunner{}
	if cmd, _, err := Install(context.Background(), none, r); err != nil || cmd != "" || len(r.commands) != 0 {
		t.Fatalf("a Go checkout installs nothing: %q %v %v", cmd, r.commands, err)
	}
}

// An install that exits non-zero, or whose runner errors, is an
// *InstallError of kind deps_install — an environment failure whose
// reason names the command, the exit and the last line it said — so
// the suites never run on a checkout with no dependencies and the
// failure is never the code's.
func TestInstallFailureIsTheEnvironments(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "package.json", "pnpm-lock.yaml")
	r := &recordingRunner{scriptedRunner: scriptedRunner{code: 1, output: "ERR_PNPM_OUTDATED_LOCKFILE\n Cannot install with \"frozen-lockfile\" because pnpm-lock.yaml is not up to date\n\n"}}
	_, _, err := Install(context.Background(), dir, r)
	var ie *InstallError
	if !errors.As(err, &ie) || ie.ExitCode != 1 || ie.Command != "pnpm install --frozen-lockfile" {
		t.Fatalf("an *InstallError with the exit and the command: %#v", err)
	}
	if Kinded(err) != DepsInstall {
		t.Fatalf("kinded deps_install: %q", Kinded(err))
	}
	want := `deps_install: dependency install: pnpm install --frozen-lockfile exited 1: Cannot install with "frozen-lockfile" because pnpm-lock.yaml is not up to date`
	if got := Reason(err); got != want {
		t.Fatalf("Reason:\n got %q\nwant %q", got, want)
	}
	if KindOfReason(Reason(err)) != DepsInstall {
		t.Fatalf("the reason opens with its kind: %q", Reason(err))
	}

	boom := errors.New("sandbox: pod gone")
	r = &recordingRunner{scriptedRunner: scriptedRunner{code: -1, err: boom}}
	_, _, err = Install(context.Background(), dir, r)
	if !errors.As(err, &ie) || !errors.Is(err, boom) || Kinded(err) != DepsInstall {
		t.Fatalf("a runner error is the install's, kinded: %v", err)
	}
}
