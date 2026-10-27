package node

import (
	"fmt"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/validate"
)

// A step's output is redacted of credential-named values alone: PATH,
// HOME and CI=true stay, so the tail still reads.
func TestSecretEnvKeepsCredentialsAlone(t *testing.T) {
	got := secretEnv([]string{"PATH=/usr/local/go/bin:/usr/bin", "HOME=/home/mesh", "CI=true",
		"GITHUB_TOKEN=ghs_0123456789abcdef", "DATABASE_DSN=postgres://u:p@h/db", "API_KEY=short"})
	want := []string{"GITHUB_TOKEN=ghs_0123456789abcdef", "DATABASE_DSN=postgres://u:p@h/db"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("secretEnv = %v, want %v", got, want)
	}
}

// A failing step's tail goes to node.log at warn, every line under the
// run's prefix; a passing step logs none.
func TestAFailingStepsTailIsLogged(t *testing.T) {
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	logStepTail(logf, "pr acme/widgets#7@abcdef12", validate.StepResult{Name: "integration", ExitCode: 1, Tail: "--- FAIL: TestX\nFAIL"})
	want := []string{
		"pr acme/widgets#7@abcdef12: warn: validation step integration exited 1; its last 2 line(s):",
		"pr acme/widgets#7@abcdef12: warn: integration | --- FAIL: TestX",
		"pr acme/widgets#7@abcdef12: warn: integration | FAIL",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged:\n%s", strings.Join(lines, "\n"))
	}
	lines = nil
	logStepTail(logf, "p", validate.StepResult{Name: "unit"})
	if len(lines) != 0 {
		t.Fatalf("a passing step logged %v", lines)
	}
}
