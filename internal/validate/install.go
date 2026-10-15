package validate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// InstallCommand is the dependency install a checkout needs before its
// suites can run, read from the files at dir: a package.json with a
// pnpm-lock.yaml is `pnpm install --frozen-lockfile`, with a yarn.lock
// `yarn install --frozen-lockfile`, with a package-lock.json (or
// npm-shrinkwrap.json) `npm ci`, with no lockfile `npm install`. A
// checkout with no package.json, or one whose node_modules is already
// there — the builder's own workspace, which its session installed
// into — needs none, and the answer is "". Go, Python and the rest
// fetch their own modules as their suites run.
func InstallCommand(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		return ""
	}
	for _, c := range []struct{ lock, cmd string }{
		{"pnpm-lock.yaml", "pnpm install --frozen-lockfile"},
		{"yarn.lock", "yarn install --frozen-lockfile"},
		{"package-lock.json", "npm ci"},
		{"npm-shrinkwrap.json", "npm ci"},
	} {
		if _, err := os.Stat(filepath.Join(dir, c.lock)); err == nil {
			return c.cmd
		}
	}
	return "npm install"
}

// InstallError is a dependency install that did not finish: its
// command, exit (124 a timeout, -1 none) and the tail of what it said.
// Its kind is DepsInstall, so Reason opens with `deps_install:` and the
// failure parks the item or the batch as the environment's.
type InstallError struct {
	Command  string
	ExitCode int
	Output   string
	Err      error
}

func (e *InstallError) Error() string {
	what := fmt.Sprintf("dependency install: %s exited %d", e.Command, e.ExitCode)
	if e.Err != nil {
		what = fmt.Sprintf("dependency install: %s: %v", e.Command, e.Err)
	}
	if tail := lastLine(e.Output); tail != "" {
		what += ": " + tail
	}
	return what
}

func (e *InstallError) Unwrap() error { return e.Err }

// EnvironmentKind implements kinded.
func (e *InstallError) EnvironmentKind() EnvironmentKind { return DepsInstall }

// lastLine is the last non-empty line of s, trimmed, within 200
// characters — enough to name the registry or the lockfile mismatch.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 200 {
				l = l[:200] + "…"
			}
			return l
		}
	}
	return ""
}

// Install runs the dependency install a checkout at dir needs
// (InstallCommand) through runner, before its suites: a fresh worktree
// — a merge batch's candidate, a rebase's checkout — has no
// node_modules, and a suite that is an npm script finds no tsc without
// one (exit 127, read as tool_missing on every console batch, dl360
// 2026-10-15). The command and how long it took are returned for the
// log; "" means none was needed. A command that errors or exits
// non-zero is an *InstallError, kinded DepsInstall. The timeout is
// DefaultTimeout, a suite's own default.
func Install(ctx context.Context, dir string, runner Runner) (command string, took time.Duration, err error) {
	command = InstallCommand(dir)
	if command == "" {
		return "", 0, nil
	}
	var out strings.Builder
	start := time.Now()
	code, runErr := runner.Run(ctx, command, DefaultTimeout, &out, &out)
	took = time.Since(start)
	if runErr != nil || code != 0 {
		return command, took, &InstallError{Command: command, ExitCode: code, Output: out.String(), Err: runErr}
	}
	return command, took, nil
}
