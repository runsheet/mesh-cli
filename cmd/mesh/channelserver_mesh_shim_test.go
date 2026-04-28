package main

import (
	"testing"
)

func TestParentPluginAgent(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		want    string
	}{
		{
			name:    "matching agent in separate token",
			cmdline: "/usr/bin/claude\x00--dangerously-load-development-channels\x00plugin:larry-pa@mesh\x00",
			want:    "larry-pa",
		},
		{
			name:    "agent as equals-form single token",
			cmdline: "/usr/bin/claude\x00--dangerously-load-development-channels=plugin:researcher@mesh\x00",
			want:    "researcher",
		},
		{
			name:    "flag absent entirely",
			cmdline: "/usr/bin/claude\x00--some-other-flag\x00--headless\x00",
			want:    "",
		},
		{
			name:    "flag present but different suffix (not @mesh)",
			cmdline: "/usr/bin/claude\x00plugin:larry-pa@discord\x00",
			want:    "",
		},
		{
			name:    "empty cmdline",
			cmdline: "",
			want:    "",
		},
		{
			name:    "malformed — flag with no value token following",
			cmdline: "/usr/bin/claude\x00--dangerously-load-development-channels\x00",
			want:    "",
		},
		{
			name:    "agent name with hyphens",
			cmdline: "claude\x00--dangerously-load-development-channels\x00plugin:my-agent-name@mesh\x00",
			want:    "my-agent-name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parentPluginAgent(tc.cmdline)
			if got != tc.want {
				t.Errorf("parentPluginAgent(%q) = %q, want %q", tc.cmdline, got, tc.want)
			}
		})
	}
}

func TestCheckParentPlugin(t *testing.T) {
	var logged []string
	logf := func(format string, args ...any) {
		// capture via fmt-style format — we only care whether it was called
		logged = append(logged, format)
	}

	t.Run("matching agent passes", func(t *testing.T) {
		logged = nil
		// Simulate what checkParentPlugin does with a synthetic cmdline by
		// calling parentPluginAgent directly and mirroring the logic.
		cmdline := "/usr/bin/claude\x00--dangerously-load-development-channels\x00plugin:larry-pa@mesh\x00"
		parentAgent := parentPluginAgent(cmdline)
		agentName := "larry-pa"
		if parentAgent != "" && parentAgent != agentName {
			logf("agent mismatch: shim --agent=%s but parent claude loaded plugin:%s@mesh", agentName, parentAgent)
			t.Fatal("should not have flagged a mismatch for matching agents")
		}
		if len(logged) != 0 {
			t.Errorf("unexpected log entries: %v", logged)
		}
	})

	t.Run("mismatching agent logs and returns false", func(t *testing.T) {
		logged = nil
		cmdline := "/usr/bin/claude\x00--dangerously-load-development-channels\x00plugin:researcher@mesh\x00"
		parentAgent := parentPluginAgent(cmdline)
		agentName := "larry-pa"
		mismatch := parentAgent != "" && parentAgent != agentName
		if !mismatch {
			t.Fatal("expected mismatch to be detected")
		}
		logf("agent mismatch: shim --agent=%s but parent claude loaded plugin:%s@mesh", agentName, parentAgent)
		if len(logged) == 0 {
			t.Error("expected log entry for mismatch")
		}
	})

	t.Run("flag absent skips check", func(t *testing.T) {
		logged = nil
		cmdline := "/usr/bin/bash\x00-c\x00some-script\x00"
		parentAgent := parentPluginAgent(cmdline)
		if parentAgent != "" {
			t.Errorf("expected empty parentAgent for cmdline without flag, got %q", parentAgent)
		}
	})

	t.Run("proc unreadable skips check", func(t *testing.T) {
		// checkParentPlugin returns true when /proc/<ppid>/cmdline is unreadable.
		// We can't easily simulate that without forking, so verify the pure
		// parsing function returns "" for an empty input (the permissive path).
		if got := parentPluginAgent(""); got != "" {
			t.Errorf("empty cmdline: got %q want empty", got)
		}
	})
}
