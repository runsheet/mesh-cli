package validate

import "testing"

// MissingTool reads the program a tool_missing step's output names as
// not found, in whichever of the shells' own words it is printed
// (docs/adr/adr-030 and goal
// a-node-low-on-disk-claims-nothing-and-a-missing-tool-is-named, item 1).
func TestMissingToolFromOutput(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
	}{
		{"sh", "sh: 1: pnpm: not found", "pnpm"},
		{"bash", "bash: pnpm: command not found", "pnpm"},
		{"exec", `exec: "pnpm": executable file not found in $PATH`, "pnpm"},
		{"bin sh", "/bin/sh: pnpm: not found", "pnpm"},
		{"no match", "--- FAIL: TestX\nexit status 1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MissingTool(tc.output); got != tc.want {
				t.Fatalf("MissingTool(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
}
