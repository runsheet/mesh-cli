package main

import (
	"bytes"
	"io"
	"testing"
)

func TestIPCRoundTrip(t *testing.T) {
	envelopes := []ipcEnvelope{
		ipcSubscribeEnv("pm", 0, "123456789", "user", []string{"u1"}, []string{"r1"}, true),
		ipcSendEnv("123456789", "hello world"),
		ipcUnsubscribeEnv("123456789"),
		ipcAckEnv("123456789"),
		ipcInboundEnv("123456789", "123456789", "@alice", "user @alice: hi"),
		ipcErrorEnv("routing-key-conflict", "already owned by builder"),
	}

	var buf bytes.Buffer
	wr := newIPCWriter(&buf)
	for _, env := range envelopes {
		if err := wr.Write(env); err != nil {
			t.Fatalf("write %s: %v", env.Type, err)
		}
	}

	rd := newIPCReader(&buf)
	for i, want := range envelopes {
		got, err := rd.Read()
		if err != nil {
			t.Fatalf("read[%d]: %v", i, err)
		}
		if got.V != ipcVersion {
			t.Errorf("[%d] v: got %d want %d", i, got.V, ipcVersion)
		}
		if got.Type != want.Type {
			t.Errorf("[%d] type: got %q want %q", i, got.Type, want.Type)
		}
		if got.RoutingKey != want.RoutingKey {
			t.Errorf("[%d] routingKey: got %q want %q", i, got.RoutingKey, want.RoutingKey)
		}
		if got.Content != want.Content {
			t.Errorf("[%d] content: got %q want %q", i, got.Content, want.Content)
		}
	}

	// Next read should return EOF.
	_, err := rd.Read()
	if err != io.EOF {
		t.Errorf("expected EOF after all envelopes, got %v", err)
	}
}

func TestIPCVersionMismatch(t *testing.T) {
	var buf bytes.Buffer
	// Write a fake envelope with the wrong version.
	buf.WriteString(`{"v":99,"type":"subscribe","routingKey":"abc"}` + "\n")
	rd := newIPCReader(&buf)
	_, err := rd.Read()
	if err == nil {
		t.Fatal("expected version mismatch error, got nil")
	}
}

func TestIPCGuildModeSubscribe(t *testing.T) {
	req := true
	pfx := "announcement"
	overrides := []ipcChannelOverride{
		{ChannelID: "111", RequireMention: boolPtr(false)},
		{ChannelID: "222", RequireMention: boolPtr(false), InboundPrefix: &pfx},
	}
	env := ipcSubscribeGuildEnv("pm", 0, "guild-999", "user", nil, nil, req, overrides)

	if env.SubscriptionMode != "guild" {
		t.Errorf("subscriptionMode: got %q want %q", env.SubscriptionMode, "guild")
	}
	if env.RoutingKey != "guild-999" {
		t.Errorf("routingKey: got %q want guild-999", env.RoutingKey)
	}
	if len(env.ChannelOverrides) != 2 {
		t.Fatalf("channelOverrides len: got %d want 2", len(env.ChannelOverrides))
	}
	if env.ChannelOverrides[0].ChannelID != "111" {
		t.Errorf("override[0].channelId: got %q want 111", env.ChannelOverrides[0].ChannelID)
	}
	if env.ChannelOverrides[1].InboundPrefix == nil || *env.ChannelOverrides[1].InboundPrefix != "announcement" {
		t.Errorf("override[1].inboundPrefix: got %v want announcement", env.ChannelOverrides[1].InboundPrefix)
	}

	// Round-trip through the IPC wire format.
	var buf bytes.Buffer
	wr := newIPCWriter(&buf)
	if err := wr.Write(env); err != nil {
		t.Fatalf("write: %v", err)
	}
	rd := newIPCReader(&buf)
	got, err := rd.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.SubscriptionMode != "guild" {
		t.Errorf("round-trip subscriptionMode: got %q want guild", got.SubscriptionMode)
	}
	if len(got.ChannelOverrides) != 2 {
		t.Fatalf("round-trip overrides len: got %d want 2", len(got.ChannelOverrides))
	}
}

func TestIPCChannelModeSubscribeOmitsSubscriptionMode(t *testing.T) {
	// channel-mode envelopes must not carry subscriptionMode so old gateways
	// treat them as channel-mode (additive backward compat).
	env := ipcSubscribeChannelEnv("pm", 0, "chan-123", "user", nil, nil, false)
	if env.SubscriptionMode != "" {
		t.Errorf("channel-mode subscribe should have empty subscriptionMode, got %q", env.SubscriptionMode)
	}

	// Serialise and check JSON doesn't include the field.
	var buf bytes.Buffer
	wr := newIPCWriter(&buf)
	if err := wr.Write(env); err != nil {
		t.Fatalf("write: %v", err)
	}
	line := buf.String()
	if bytes.Contains([]byte(line), []byte(`"subscriptionMode"`)) {
		t.Errorf("channel-mode JSON should not include subscriptionMode field: %s", line)
	}
}

func TestIPCInboundCarriesChannelID(t *testing.T) {
	env := ipcInboundEnv("guild-999", "chan-111", "@alice", "hello")
	if env.RoutingKey != "guild-999" {
		t.Errorf("routingKey: got %q want guild-999", env.RoutingKey)
	}
	if env.ChannelID != "chan-111" {
		t.Errorf("channelId: got %q want chan-111", env.ChannelID)
	}

	var buf bytes.Buffer
	wr := newIPCWriter(&buf)
	if err := wr.Write(env); err != nil {
		t.Fatalf("write: %v", err)
	}
	rd := newIPCReader(&buf)
	got, err := rd.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ChannelID != "chan-111" {
		t.Errorf("round-trip channelId: got %q want chan-111", got.ChannelID)
	}
}

func boolPtr(b bool) *bool { return &b }
