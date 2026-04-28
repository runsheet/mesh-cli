package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestParseTargetURL(t *testing.T) {
	cases := []struct {
		input    string
		platform string
		subtype  string
		id       string
		wantErr  bool
	}{
		{"discord://channel/123456789", "discord", "channel", "123456789", false},
		{"discord://dm/987654321", "discord", "dm", "987654321", false},
		{"discord://channel/123/extra", "discord", "channel", "123/extra", false}, // extra path kept in id
		{"nodomain", "", "", "", true},
		{"discord://channel/", "", "", "", true},
		{"discord://noid", "", "", "", true},
	}

	for _, tc := range cases {
		platform, subtype, id, errMsg := parseTargetURL(tc.input)
		if tc.wantErr {
			if errMsg == "" {
				t.Errorf("parseTargetURL(%q): expected error, got platform=%q subtype=%q id=%q", tc.input, platform, subtype, id)
			}
			continue
		}
		if errMsg != "" {
			t.Errorf("parseTargetURL(%q): unexpected error: %s", tc.input, errMsg)
			continue
		}
		if platform != tc.platform {
			t.Errorf("parseTargetURL(%q) platform: got %q want %q", tc.input, platform, tc.platform)
		}
		if subtype != tc.subtype {
			t.Errorf("parseTargetURL(%q) subtype: got %q want %q", tc.input, subtype, tc.subtype)
		}
		if id != tc.id {
			t.Errorf("parseTargetURL(%q) id: got %q want %q", tc.input, id, tc.id)
		}
	}
}

func TestMigrateMCPFile_DiscordToMesh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	// Write an old-style discord key.
	old := claudeSettings{
		MCPServers: map[string]mcpServerEntry{
			"discord": {
				Command: "/usr/bin/mesh",
				Args:    []string{"channel-server", "discord-shim", "--agent", "larry-pa", "--index", "0", "--account", "larry"},
			},
		},
	}
	data, _ := json.MarshalIndent(old, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	migrateMCPFile(path, "larry-pa", "/usr/bin/mesh", []string{"channel-server", "mesh-shim", "--agent", "larry-pa"})

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got claudeSettings
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatal(err)
	}
	if _, hasMesh := got.MCPServers["mesh"]; !hasMesh {
		t.Errorf("expected mcpServers.mesh after migration, got keys: %v", got.MCPServers)
	}
	if _, hasDiscord := got.MCPServers["discord"]; hasDiscord {
		t.Errorf("expected mcpServers.discord to be removed after migration")
	}
	if got.MCPServers["mesh"].Args[1] != "mesh-shim" {
		t.Errorf("expected args[1]=mesh-shim, got %v", got.MCPServers["mesh"].Args)
	}
}

func TestMigrateMCPFile_AlreadyMesh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	already := claudeSettings{
		MCPServers: map[string]mcpServerEntry{
			"mesh": {
				Command: "/usr/bin/mesh",
				Args:    []string{"channel-server", "mesh-shim", "--agent", "larry-pa"},
			},
		},
	}
	data, _ := json.MarshalIndent(already, "", "  ")
	before := string(data)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	migrateMCPFile(path, "larry-pa", "/usr/bin/mesh", []string{"channel-server", "mesh-shim", "--agent", "larry-pa"})

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// File should be unchanged.
	if string(result) != before {
		t.Errorf("file should not change when already migrated;\nbefore: %s\nafter: %s", before, string(result))
	}
}

func TestMigrateMCPFile_Missing(t *testing.T) {
	// Should not panic or error when the file doesn't exist.
	migrateMCPFile("/tmp/mesh-test-nonexistent-12345/.mcp.json", "agent", "/usr/bin/mesh", nil)
}

func TestChannelRouterSyncRestartsOnChannelChange(t *testing.T) {
	var mu sync.Mutex
	var startCount int
	var lastChannels []ChannelSpec

	// started is signalled each time the stub is invoked so the test can wait
	// for the goroutine to have actually called startFn before checking counts.
	started := make(chan struct{}, 8)

	cr := newChannelRouterWithStart(func(_ context.Context, _ string, _ string, channels []ChannelSpec) {
		mu.Lock()
		startCount++
		lastChannels = channels
		mu.Unlock()
		started <- struct{}{}
		// Block until ctx is cancelled so the runningRouter stays "running"
		// and Sync's <-rr.done only unblocks after cancel() is called.
		// We return immediately here; the goroutine's defer closes done.
	})

	waitStart := func(n int) {
		for i := 0; i < n; i++ {
			<-started
		}
	}

	chA := ChannelSpec{Type: "discord", Config: map[string]any{"account": "bot", "channelId": "111"}}
	chB := ChannelSpec{Type: "whatsapp", Config: map[string]any{"account": "wa", "chatJid": "123@g.us"}}

	// First Sync: one channel — router starts once.
	cr.Sync([]*AgentDef{{Name: "larry", Channels: []ChannelSpec{chA}}})
	waitStart(1)
	mu.Lock()
	sc := startCount
	mu.Unlock()
	if sc != 1 {
		t.Fatalf("after first Sync: startCount = %d, want 1", sc)
	}

	// Second Sync: same channels — no restart.
	cr.Sync([]*AgentDef{{Name: "larry", Channels: []ChannelSpec{chA}}})
	mu.Lock()
	sc = startCount
	mu.Unlock()
	if sc != 1 {
		t.Fatalf("after same-channel Sync: startCount = %d, want 1 (no restart)", sc)
	}

	// Third Sync: channels changed (added chB) — router must restart.
	// Sync blocks on <-rr.done for the old router; our stub returns immediately
	// so done closes, then the new router goroutine is started.
	cr.Sync([]*AgentDef{{Name: "larry", Channels: []ChannelSpec{chA, chB}}})
	waitStart(1)
	mu.Lock()
	sc = startCount
	lc := lastChannels
	mu.Unlock()
	if sc != 2 {
		t.Fatalf("after channel-change Sync: startCount = %d, want 2 (restart)", sc)
	}
	if len(lc) != 2 {
		t.Errorf("restarted router has %d channels, want 2", len(lc))
	}
	if lc[1].Type != "whatsapp" {
		t.Errorf("second channel type: got %q want whatsapp", lc[1].Type)
	}

	// Fourth Sync: agent removed — router stops, not restarted.
	cr.Sync([]*AgentDef{})
	mu.Lock()
	sc = startCount
	mu.Unlock()
	if sc != 2 {
		t.Fatalf("after remove Sync: startCount = %d, want 2 (no extra start)", sc)
	}
	cr.mu.Lock()
	remaining := len(cr.routers)
	cr.mu.Unlock()
	if remaining != 0 {
		t.Errorf("routers map has %d entries after remove, want 0", remaining)
	}
}

func TestFilterSelf(t *testing.T) {
	all := []peerRegistration{
		{PeerID: "nova"},
		{PeerID: "larry"},
		{PeerID: "researcher"},
	}

	// Self is removed; others are preserved in order.
	got := filterSelf(all, "larry")
	if len(got) != 2 {
		t.Fatalf("len: got %d want 2", len(got))
	}
	if got[0].PeerID != "nova" || got[1].PeerID != "researcher" {
		t.Errorf("peers: got %v", got)
	}

	// Self not present — result is unchanged length.
	got2 := filterSelf(all, "absent")
	if len(got2) != 3 {
		t.Errorf("absent self: len got %d want 3", len(got2))
	}

	// Empty input.
	got3 := filterSelf(nil, "nova")
	if len(got3) != 0 {
		t.Errorf("nil input: got %v", got3)
	}
}

func TestMeshLastInboundReplyTarget(t *testing.T) {
	l := &meshLastInbound{}

	// No inbound yet.
	source, target := l.replyTarget()
	if source != "" || target != "" {
		t.Errorf("expected empty source+target before any inbound, got source=%q target=%q", source, target)
	}

	// Discord channel inbound.
	l.set("discord", "123456789")
	source, target = l.replyTarget()
	if source != "discord" {
		t.Errorf("source: got %q want discord", source)
	}
	if target != "discord://channel/123456789" {
		t.Errorf("target: got %q want discord://channel/123456789", target)
	}
}
