package commands

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/nodev1"
)

// TestNodeRunHarness: --harness codex registers Harnesses:[codex] and
// constructs a CodexSession; --harness claude-code (or the flag
// omitted) is unchanged; an unrecognised value is refused before any
// session runs (ADR-044's amendment, "--harness codex").
func TestNodeRunHarness(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  string
		want  []string
		codex bool
		bad   bool
	}{
		{"default", "", []string{"claude-code"}, false, false},
		{"claude-code", "claude-code", []string{"claude-code"}, false, false},
		{"codex", "codex", []string{"codex"}, true, false},
		{"unrecognised", "gpt-5", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harnesses, sess, err := nodeRunSessionOf(tc.kind, false, nil, nil)
			if (err != nil) != tc.bad {
				t.Fatalf("--harness %q: err = %v, want bad=%v", tc.kind, err, tc.bad)
			}
			if tc.bad {
				if sess != nil {
					t.Fatalf("--harness %q: refused but still built a session", tc.kind)
				}
				return
			}
			if len(harnesses) != len(tc.want) || (len(harnesses) > 0 && harnesses[0] != tc.want[0]) {
				t.Errorf("--harness %q: harnesses = %v, want %v", tc.kind, harnesses, tc.want)
			}
			_, isCodex := sess.(*nodev1.CodexSession)
			if isCodex != tc.codex {
				t.Errorf("--harness %q: codex session = %v, want %v", tc.kind, isCodex, tc.codex)
			}
			if !tc.codex {
				if _, ok := sess.(*nodev1.ClaudeSession); !ok {
					t.Errorf("--harness %q: session is %T, want *nodev1.ClaudeSession", tc.kind, sess)
				}
			}
		})
	}
}

// TestNodeRunHarnessUnattended: --unattended still sets claude-code's
// skip-permissions args; codex has no equivalent flag.
func TestNodeRunHarnessUnattended(t *testing.T) {
	_, sess, err := nodeRunSessionOf("", true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, ok := sess.(*nodev1.ClaudeSession)
	if !ok {
		t.Fatalf("session is %T, want *nodev1.ClaudeSession", sess)
	}
	if len(cs.ExtraArgs) != 1 || cs.ExtraArgs[0] != "--dangerously-skip-permissions" {
		t.Errorf("ExtraArgs = %v, want [--dangerously-skip-permissions]", cs.ExtraArgs)
	}
}
