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

// TestNodeRunHarnessRegistry: the node holds both harnesses' sessions —
// the --harness one shared, the other beside it (the existing Codex
// app-server session or claude -p) — and registers the routed harnesses
// beside its own only when node.yaml routes some; with no route it
// registers its own alone, as before.
func TestNodeRunHarnessRegistry(t *testing.T) {
	for _, own := range []string{"claude-code", "codex"} {
		harnesses, sess, err := nodeRunSessionOf(own, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		reg := nodeRunSessions(harnesses[0], sess, false, nil, nil)
		if reg[own] != sess {
			t.Fatalf("--harness %s: the registry's %s session is not the node's own", own, own)
		}
		if _, ok := reg["codex"].(*nodev1.CodexSession); !ok {
			t.Fatalf("--harness %s: the registry's codex session is %T", own, reg["codex"])
		}
		if _, ok := reg["claude-code"].(*nodev1.ClaudeSession); !ok {
			t.Fatalf("--harness %s: the registry's claude-code session is %T", own, reg["claude-code"])
		}
	}
	if got := nodeRunRegistered([]string{"claude-code"}, nil); len(got) != 1 || got[0] != "claude-code" {
		t.Fatalf("with no route the node registers %v", got)
	}
	got := nodeRunRegistered([]string{"claude-code"}, []nodev1.HarnessRoute{{Kind: "deploy", Harness: "either"}})
	if len(got) != 2 || got[0] != "claude-code" || got[1] != "codex" {
		t.Fatalf("with deploy → either the node registers %v", got)
	}
}
