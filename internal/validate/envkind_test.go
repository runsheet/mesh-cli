package validate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

// scriptedRunner answers every step with its exit code, output and error.
type scriptedRunner struct {
	code   int
	output string
	err    error
}

func (r scriptedRunner) Run(_ context.Context, _ string, _ time.Duration, stdout, _ io.Writer) (int, error) {
	_, _ = io.WriteString(stdout, r.output)
	return r.code, r.err
}

// Each environment failure — a step's exit 124, its exit 127, a git
// credential refusal, an image-pull refusal (the node's own pull's
// error, and podman's 125 on a run that could not pull) and a broken
// runtime (podman's 125 on a rootless store it cannot open, lock or map
// a namespace for) — is reported with its kind on the step and on the
// run's error; a plain exit 1, and a pass, carry none (docs/adr/adr-030).
func TestEnvironmentKindOfEachFailure(t *testing.T) {
	credErr := errors.New("git fetch origin main: exit status 128: remote: Write access to repository not granted.\nfatal: unable to access 'https://github.com/acme/widgets/': The requested URL returned error: 403")
	pullErr := errors.New("node image ghcr.io/runsheet/mesh-node:dev: pull failed: Error: initializing source docker://ghcr.io/runsheet/mesh-node:dev: reading manifest dev in ghcr.io/runsheet/mesh-node: unauthorized")
	for _, tc := range []struct {
		name   string
		runner scriptedRunner
		want   EnvironmentKind
	}{
		{"exit 124", scriptedRunner{code: 124}, StepTimeout},
		{"exit 127", scriptedRunner{code: 127, output: "sh: 1: tsc: not found"}, ToolMissing},
		{"credential refusal", scriptedRunner{code: -1, err: credErr}, CredentialRefused},
		{"image pull refusal", scriptedRunner{code: -1, err: pullErr}, ImagePullRefused},
		{"podman could not pull", scriptedRunner{code: 125, output: "Trying to pull ghcr.io/runsheet/mesh-node:dev...\nError: initializing source docker://ghcr.io/runsheet/mesh-node:dev: reading manifest dev in ghcr.io/runsheet/mesh-node: denied"}, ImagePullRefused},
		{"podman storage corrupted", scriptedRunner{code: 125, output: "Error: error opening storage: layer not known"}, RuntimeBroken},
		{"podman database mismatch", scriptedRunner{code: 125, output: "Error: database configuration mismatch: expected mount_program=, found mount_program=/usr/bin/fuse-overlayfs"}, RuntimeBroken},
		{"podman namespace refused", scriptedRunner{code: 125, output: "Error: cannot set up namespace using \"/usr/bin/newuidmap\": exit status 1"}, RuntimeBroken},
		{"exit 1", scriptedRunner{code: 1, output: "--- FAIL: TestX"}, ""},
		{"exit 125 not a pull", scriptedRunner{code: 125, output: "Error: no such container"}, ""},
		{"pass", scriptedRunner{code: 0}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results, err := Run(context.Background(), []Step{{Name: "check", Command: "npm run check"}}, tc.runner, Options{Dir: t.TempDir()})
			if len(results) != 1 {
				t.Fatalf("results = %+v", results)
			}
			if got := results[0].EnvironmentKind; got != tc.want {
				t.Fatalf("step kind = %q, want %q", got, tc.want)
			}
			if tc.runner.err != nil {
				if got := Kinded(err); got != tc.want {
					t.Fatalf("run error %v: kind %q, want %q", err, got, tc.want)
				}
				if got, want := Reason(fmt.Errorf("validation: %w", err)), string(tc.want)+": validation: "+tc.runner.err.Error(); got != want {
					t.Fatalf("reason = %q, want %q", got, want)
				}
			}
		})
	}
}

// The host runner's own 127 for a command the shell cannot find is
// tool_missing, end to end.
func TestHostRunnerMissingToolIsToolMissing(t *testing.T) {
	dir := t.TempDir()
	results, err := Run(context.Background(), []Step{{Name: "types", Command: "mesh-no-such-tool --version"}}, HostRunner{Dir: dir}, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].ExitCode != 127 || results[0].EnvironmentKind != ToolMissing {
		t.Fatalf("result = %+v", results[0])
	}
}

// IsRuntimeBroken matches podman's own words for a runtime it cannot
// use — storage it cannot open or lock, a readlink into its own layers
// finding nothing, a permission refusal on its storage root, or a user
// namespace it could not set up — and never a plain permission or
// readlink failure of the agent's own command, which names no storage
// path (docs/adr/adr-030's amendment).
func TestIsRuntimeBroken(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"error opening storage", "Error: error opening storage: layer not known", true},
		{"creating temporary storage", "Error: creating temporary storage: permission denied", true},
		{"database configuration mismatch", "Error: database configuration mismatch: expected mount_program=, found mount_program=/usr/bin/fuse-overlayfs", true},
		{"readlink in containers storage", "Error: readlink /home/mesh/.local/share/containers/storage/overlay/1a2b3c: no such file or directory", true},
		{"readlink in overlay path", "Error: readlink /home/mesh/.local/share/containers/storage/overlay/l/XYZ: no such file or directory", true},
		{"permission denied under storage root", "Error: mkdir /home/mesh/.local/share/containers/storage/overlay: permission denied", true},
		{"cannot set up namespace", `Error: cannot set up namespace using "/usr/bin/newuidmap": exit status 1`, true},
		{"newuidmap missing", "Error: newuidmap: command not found", true},
		{"agent's own permission denied", "EACCES: permission denied, open '/workspace/foo.txt'", false},
		{"agent's own readlink", "readlink /workspace/node_modules/.bin/tsc: no such file or directory", false},
		{"image pull refusal", "Error: initializing source docker://ghcr.io/runsheet/mesh-node:dev: reading manifest dev in ghcr.io/runsheet/mesh-node: unauthorized", false},
		{"ordinary test failure", "--- FAIL: TestX", false},
	} {
		if got := IsRuntimeBroken(tc.text); got != tc.want {
			t.Errorf("%s: IsRuntimeBroken(%q) = %v, want %v", tc.name, tc.text, got, tc.want)
		}
	}
}

func TestWithKindAndKindOfReason(t *testing.T) {
	if got := WithKind(StepTimeout, "validation failed: step it exited 124"); got != "step_timeout: validation failed: step it exited 124" {
		t.Fatalf("WithKind = %q", got)
	}
	if got := WithKind(StepTimeout, "step_timeout: x"); got != "step_timeout: x" {
		t.Fatalf("WithKind twice = %q", got)
	}
	if got := WithKind("", "validation failed: step it exited 1"); got != "validation failed: step it exited 1" {
		t.Fatalf("no kind = %q", got)
	}
	if KindOfReason("validation failed: step it exited 1") != "" || KindOfReason("tool_missing: x") != ToolMissing {
		t.Fatal("KindOfReason")
	}
	for _, k := range EnvironmentKinds() {
		if !k.Valid() {
			t.Fatalf("%q not valid", k)
		}
	}
}
