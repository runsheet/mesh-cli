package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// validateRepo is a git checkout whose validate.yml declares three
// suites, each of which would leave a marker in markers on the host if
// it ran there: unit (every change), lint (only *.go, which the change
// touches) and docs (only docs/**, which it does not).
func validateRepo(t *testing.T, markers string) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	yml := fmt.Sprintf(`suites:
  - name: unit
    kind: unit
    command: touch %[1]s/unit && go test ./...
  - name: lint
    kind: lint
    command: touch %[1]s/lint && go vet ./...
    paths: ["**/*.go"]
  - name: docs
    kind: lint
    command: touch %[1]s/docs && true
    paths: ["docs/**"]
`, markers)
	if err := os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return dir, strings.TrimSpace(string(out))
}

func TestValidateRunsInTheJobContainer(t *testing.T) {
	markers := t.TempDir()
	dir, head := validateRepo(t, markers)

	run := func(t *testing.T, agent func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error)) (*sandbox.Fake, *State, error) {
		fake := &sandbox.Fake{Agent: agent}
		jc := &JobContainer{
			Runtime: fake,
			Pod:     sandbox.Pod{Name: "mesh-build-item-5"},
			Image:   "ghcr.io/runsheet/mesh-node@sha256:abc",
			Mounts:  []sandbox.Mount{{Source: dir, Target: dir}},
			Env:     []string{"HOME=/home/mesh"},
		}
		state := &State{WorkspaceDir: dir, Pod: jc, BaseRef: "origin/main"}
		v := Validate{
			Meta: validate.Meta{NodeID: "node-1", ItemID: "item-5"},
			ChangedPaths: func(ctx context.Context, d, base string) ([]string, error) {
				if d != dir || base != "origin/main" {
					t.Errorf("changed paths asked of %q from %q, want %q from origin/main", d, base, dir)
				}
				return []string{"main.go"}, nil
			},
		}
		records := Run(context.Background(), Job{Steps: []Step{v.Step()}}, state)
		return fake, state, records[0].Err
	}

	// every suite command is an exec in the job's pod, and none runs on
	// the host
	assertInPod := func(t *testing.T, fake *sandbox.Fake, wantSuites []string) {
		t.Helper()
		if len(fake.Pods) != 0 {
			t.Errorf("validate made %d pod(s) of its own; want the job's alone", len(fake.Pods))
		}
		if len(fake.Agents) != len(wantSuites) {
			t.Fatalf("%d execs in the pod, want %d (%v)", len(fake.Agents), len(wantSuites), wantSuites)
		}
		for _, c := range fake.Calls {
			if c != "agent mesh-build-item-5" {
				t.Errorf("runtime call %q; want every one an exec in the job's pod", c)
			}
		}
		for i, spec := range fake.Agents {
			if !strings.HasPrefix(spec.Name, "mesh-build-item-5-exec-") {
				t.Errorf("exec %d container %q is not in the job's pod", i, spec.Name)
			}
			if spec.Image != "ghcr.io/runsheet/mesh-node@sha256:abc" || spec.Workdir != dir {
				t.Errorf("exec %d image %q workdir %q, want the job container's", i, spec.Image, spec.Workdir)
			}
			cmd := strings.Join(spec.Command, " ")
			if !strings.HasPrefix(cmd, "sh -c touch "+markers+"/"+wantSuites[i]+" ") {
				t.Errorf("exec %d command %q, want suite %s", i, cmd, wantSuites[i])
			}
		}
		entries, err := os.ReadDir(markers)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Errorf("suite %s ran on the host", e.Name())
		}
	}

	t.Run("passing", func(t *testing.T) {
		fake, state, err := run(t, nil)
		if err != nil {
			t.Fatalf("validate step failed: %v", err)
		}
		// docs is not affected by main.go and is not run
		assertInPod(t, fake, []string{"unit", "lint"})

		att, ok := state.Results[ValidateStepName].(validate.Attestation)
		if !ok {
			t.Fatalf("state's validate result is %T, want an attestation", state.Results[ValidateStepName])
		}
		if att.Commit != head || !att.Sandboxed || att.RunnerImage != "ghcr.io/runsheet/mesh-node@sha256:abc" {
			t.Errorf("attestation commit %q sandboxed %v image %q; want %s, sandboxed, the job's image", att.Commit, att.Sandboxed, att.RunnerImage, head)
		}
		if att.NodeID != "node-1" || att.ItemID != "item-5" {
			t.Errorf("attestation node %q item %q", att.NodeID, att.ItemID)
		}
		if len(att.Steps) != 2 || att.Steps[0].Name != "unit" || att.Steps[1].Name != "lint" {
			t.Errorf("attestation steps %+v, want unit and lint", att.Steps)
		}
	})

	t.Run("failing", func(t *testing.T) {
		fake, state, err := run(t, func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
			if strings.Contains(strings.Join(spec.Command, " "), "go vet") {
				fmt.Fprintln(spec.Stdout, "main.go:1: vet: something is off")
				return sandbox.Exit{Code: 1}, nil
			}
			return sandbox.Exit{}, nil
		})
		var se *SuiteError
		if !errors.As(err, &se) {
			t.Fatalf("validate step error = %v, want a failed suite", err)
		}
		if se.Result.Name != "lint" || se.Result.ExitCode != 1 {
			t.Errorf("failed suite %s exit %d, want lint exit 1", se.Result.Name, se.Result.ExitCode)
		}
		if !strings.Contains(err.Error(), "suite lint exited 1") || !strings.Contains(err.Error(), "vet: something is off") {
			t.Errorf("step error %q does not name the suite and carry its output", err)
		}
		assertInPod(t, fake, []string{"unit", "lint"})

		att, ok := state.Results[ValidateStepName].(validate.Attestation)
		if !ok {
			t.Fatalf("a failed validation still attests; got %T", state.Results[ValidateStepName])
		}
		if name, failed := att.Failed(); !failed || name != "lint" {
			t.Errorf("attestation's failed suite %q (%v), want lint", name, failed)
		}
	})

	t.Run("no job container", func(t *testing.T) {
		state := &State{WorkspaceDir: dir}
		if err := (Validate{}).Run(context.Background(), state); err == nil {
			t.Fatal("validate with no job container passed; it must never fall back to the host")
		}
	})
}
