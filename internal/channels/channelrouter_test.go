package channels

import (
	"context"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/peers"
)

func TestChannelRouterSyncRestartsOnChannelChange(t *testing.T) {
	var mu sync.Mutex
	var startCount int
	var lastChannels []Spec

	started := make(chan struct{}, 8)

	cr := NewChannelRouterWithStart(peers.New(), func(_ context.Context, _ string, _ string, channels []Spec, _ *peers.Broker) {
		mu.Lock()
		startCount++
		lastChannels = channels
		mu.Unlock()
		started <- struct{}{}
	})

	waitStart := func(n int) {
		for i := 0; i < n; i++ {
			<-started
		}
	}

	chA := Spec{Type: "discord", Config: map[string]any{"account": "bot", "channelId": "111"}}
	chB := Spec{Type: "whatsapp", Config: map[string]any{"account": "wa", "chatJid": "123@g.us"}}

	// First Sync: one channel — router starts once.
	cr.Sync([]AgentEntry{{Name: "larry", Channels: []Spec{chA}}})
	waitStart(1)
	mu.Lock()
	sc := startCount
	mu.Unlock()
	if sc != 1 {
		t.Fatalf("after first Sync: startCount = %d, want 1", sc)
	}

	// Second Sync: same channels — no restart.
	cr.Sync([]AgentEntry{{Name: "larry", Channels: []Spec{chA}}})
	mu.Lock()
	sc = startCount
	mu.Unlock()
	if sc != 1 {
		t.Fatalf("after same-channel Sync: startCount = %d, want 1 (no restart)", sc)
	}

	// Third Sync: channels changed — router must restart.
	cr.Sync([]AgentEntry{{Name: "larry", Channels: []Spec{chA, chB}}})
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
	cr.Sync([]AgentEntry{})
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
	all := []peers.Registration{
		{PeerID: "nova"},
		{PeerID: "larry"},
		{PeerID: "researcher"},
	}

	got := FilterSelf(all, "larry")
	if len(got) != 2 {
		t.Fatalf("len: got %d want 2", len(got))
	}
	if got[0].PeerID != "nova" || got[1].PeerID != "researcher" {
		t.Errorf("peers: got %v", got)
	}

	got2 := FilterSelf(all, "absent")
	if len(got2) != 3 {
		t.Errorf("absent self: len got %d want 3", len(got2))
	}

	got3 := FilterSelf(nil, "nova")
	if len(got3) != 0 {
		t.Errorf("nil input: got %v", got3)
	}
}

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
		{"discord://channel/123/extra", "discord", "channel", "123/extra", false},
		{"nodomain", "", "", "", true},
		{"discord://channel/", "", "", "", true},
		{"discord://noid", "", "", "", true},
	}

	for _, tc := range cases {
		platform, subtype, id, errMsg := ParseTargetURL(tc.input)
		if tc.wantErr {
			if errMsg == "" {
				t.Errorf("ParseTargetURL(%q): expected error, got platform=%q subtype=%q id=%q", tc.input, platform, subtype, id)
			}
			continue
		}
		if errMsg != "" {
			t.Errorf("ParseTargetURL(%q): unexpected error: %s", tc.input, errMsg)
			continue
		}
		if platform != tc.platform {
			t.Errorf("ParseTargetURL(%q) platform: got %q want %q", tc.input, platform, tc.platform)
		}
		if subtype != tc.subtype {
			t.Errorf("ParseTargetURL(%q) subtype: got %q want %q", tc.input, subtype, tc.subtype)
		}
		if id != tc.id {
			t.Errorf("ParseTargetURL(%q) id: got %q want %q", tc.input, id, tc.id)
		}
	}
}
