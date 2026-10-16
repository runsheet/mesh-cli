package harness

import (
	"encoding/json"
	"testing"
)

// settingsArg is the JSON a command line passes as --settings; it fails
// the test when there is none.
func settingsArg(t *testing.T, args []string) map[string]any {
	t.Helper()
	for i, a := range args {
		if a == "--settings" && i+1 < len(args) {
			var m map[string]any
			if err := json.Unmarshal([]byte(args[i+1]), &m); err != nil {
				t.Fatalf("--settings %q: %v", args[i+1], err)
			}
			return m
		}
	}
	t.Fatalf("no --settings in %q", args)
	return nil
}

// assertNoAttribution fails unless the settings hide Claude Code's
// commit trailer, PR text and session link.
func assertNoAttribution(t *testing.T, settings map[string]any) {
	t.Helper()
	at, ok := settings["attribution"].(map[string]any)
	if !ok {
		t.Fatalf("no attribution in settings %v", settings)
	}
	if at["commit"] != "" || at["pr"] != "" || at["sessionUrl"] != false {
		t.Fatalf("attribution = %v, want commit \"\", pr \"\", sessionUrl false", at)
	}
}

// A job with no Settings still gets --settings, carrying the attribution
// block: no session adds Claude's Co-Authored-By or PR text.
func TestNilSettingsStillCarryNoAttribution(t *testing.T) {
	c := &ClaudeCode{}
	for _, args := range [][]string{c.Args(Job{Prompt: "hi"}), c.SandboxArgs(Job{Prompt: "hi"}, "/home/mesh/mcp.json")} {
		s := settingsArg(t, args)
		assertNoAttribution(t, s)
		if len(s) != 1 {
			t.Fatalf("settings = %v, want the attribution alone", s)
		}
	}
}

// The tool-trim hook survives the merge, and the job's own map is left
// as it was.
func TestToolTrimSettingsKeepTheirHooksBesideAttribution(t *testing.T) {
	hooks := map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash"}}}
	job := Job{Prompt: "hi", Settings: map[string]any{"hooks": hooks, "attribution": false}}
	s := settingsArg(t, (&ClaudeCode{}).Args(job))
	assertNoAttribution(t, s)
	pre, _ := s["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 1 || pre[0].(map[string]any)["matcher"] != "Bash" {
		t.Fatalf("hooks = %v", s["hooks"])
	}
	if job.Settings["attribution"] != false {
		t.Fatalf("the job's Settings were changed: %v", job.Settings)
	}
}
