package node

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

// commandsRunner records every command the validator hands it and
// answers each with the exit code scripted for it (0 when none).
type commandsRunner struct {
	exits    map[string]int
	commands []string
}

func (r *commandsRunner) Run(_ context.Context, cmd string, _ time.Duration, stdout, _ io.Writer) (int, error) {
	r.commands = append(r.commands, cmd)
	if code, ok := r.exits[cmd]; ok {
		_, _ = io.WriteString(stdout, "npm ERR! code E401\nnpm ERR! Unable to authenticate\n")
		return code, nil
	}
	return 0, nil
}

// A checkout with a package.json and a lockfile and no node_modules —
// a merge batch's worktree, a rebase's checkout — installs its
// dependencies through the validator's runner before any suite runs
// (ADR-013 amendment 2026-10-15); one whose node_modules is there, the
// builder's workspace, runs its suites as before.
func TestValidationInstallsDependenciesBeforeTheSuites(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	for _, name := range []string{"package.json", "package-lock.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logs := &syncBuf{}
	vr := validator{WS: &Git{}, logf: log.New(logs, "", 0).Printf}
	var notes []string
	tg := baseGoneTarget(dir, &notes)
	runner := &commandsRunner{}
	v := &buildValidation{runner: runner, close: func() {}}

	if _, err := vr.run(context.Background(), tg, v); err != nil {
		t.Fatalf("run: %v\n%s", err, logs.String())
	}
	if len(runner.commands) < 2 || runner.commands[0] != "npm ci" {
		t.Fatalf("the install runs first, then the suites: %v", runner.commands)
	}
	for _, c := range runner.commands[1:] {
		if c != "true" {
			t.Fatalf("after the install only the suites run: %v", runner.commands)
		}
	}
	if !containsLine(logs.String(), "item g#2: validation: dependencies installed with npm ci in ") {
		t.Fatalf("the log names the install:\n%s", logs.String())
	}

	// the builder's workspace: node_modules there, nothing to install
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner = &commandsRunner{}
	v = &buildValidation{runner: runner, close: func() {}}
	if _, err := vr.run(context.Background(), tg, v); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, c := range runner.commands {
		if c == "npm ci" {
			t.Fatalf("an installed checkout installs nothing: %v", runner.commands)
		}
	}
}

// An install that fails is the environment's, deps_install: the run's
// error carries the kind and names the command, the exit and what npm
// said, no suite ran, and nothing was attested.
func TestValidationInstallFailureIsTheEnvironments(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logs := &syncBuf{}
	vr := validator{WS: &Git{}, logf: log.New(logs, "", 0).Printf}
	var notes []string
	tg := baseGoneTarget(dir, &notes)
	runner := &commandsRunner{exits: map[string]int{"pnpm install --frozen-lockfile": 1}}
	v := &buildValidation{runner: runner, close: func() {}}

	att, err := vr.run(context.Background(), tg, v)
	var ie *validate.InstallError
	if !errors.As(err, &ie) || validate.Kinded(err) != validate.DepsInstall {
		t.Fatalf("a deps_install error: %v (%T)", err, err)
	}
	want := "deps_install: dependency install: pnpm install --frozen-lockfile exited 1: npm ERR! Unable to authenticate"
	if got := validate.Reason(err); got != want {
		t.Fatalf("Reason:\n got %q\nwant %q", got, want)
	}
	if len(runner.commands) != 1 || att.Commit != "" || len(v.runs) != 0 {
		t.Fatalf("no suite ran and nothing was attested: %v, %+v", runner.commands, att)
	}
	if !containsLine(logs.String(), "item g#2: validation: "+want) {
		t.Fatalf("the log says why:\n%s", logs.String())
	}
}

func containsLine(logs, prefix string) bool {
	for _, l := range strings.Split(logs, "\n") {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}
