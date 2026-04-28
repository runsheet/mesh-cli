package ipc_test

import (
	"bytes"
	"io"
	"testing"

	"projectmesh.io/mesh-cli/internal/ipc"
)

func TestRoundTrip(t *testing.T) {
	envelopes := []ipc.Envelope{
		ipc.SubscribeEnv("pm", 0, "123456789", "user", []string{"u1"}, []string{"r1"}, true),
		ipc.SendEnv("123456789", "hello world"),
		ipc.UnsubscribeEnv("123456789"),
		ipc.AckEnv("123456789"),
		ipc.InboundEnv("123456789", "123456789", "@alice", "user @alice: hi"),
		ipc.ErrorEnv("routing-key-conflict", "already owned by builder"),
	}

	var buf bytes.Buffer
	wr := ipc.NewWriter(&buf)
	for _, env := range envelopes {
		if err := wr.Write(env); err != nil {
			t.Fatalf("write %s: %v", env.Type, err)
		}
	}

	rd := ipc.NewReader(&buf)
	for i, want := range envelopes {
		got, err := rd.Read()
		if err != nil {
			t.Fatalf("read[%d]: %v", i, err)
		}
		if got.V != ipc.Version {
			t.Errorf("[%d] v: got %d want %d", i, got.V, ipc.Version)
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

func TestVersionMismatch(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"v":99,"type":"subscribe","routingKey":"abc"}` + "\n")
	rd := ipc.NewReader(&buf)
	_, err := rd.Read()
	if err == nil {
		t.Fatal("expected version mismatch error, got nil")
	}
}

func TestGuildModeSubscribe(t *testing.T) {
	req := true
	pfx := "announcement"
	overrides := []ipc.ChannelOverride{
		{ChannelID: "111", RequireMention: boolPtr(false)},
		{ChannelID: "222", RequireMention: boolPtr(false), InboundPrefix: &pfx},
	}
	env := ipc.SubscribeGuildEnv("pm", 0, "guild-999", "user", nil, nil, req, overrides)

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

	var buf bytes.Buffer
	wr := ipc.NewWriter(&buf)
	if err := wr.Write(env); err != nil {
		t.Fatalf("write: %v", err)
	}
	rd := ipc.NewReader(&buf)
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

func TestChannelModeSubscribeOmitsSubscriptionMode(t *testing.T) {
	env := ipc.SubscribeChannelEnv("pm", 0, "chan-123", "user", nil, nil, false)
	if env.SubscriptionMode != "" {
		t.Errorf("channel-mode subscribe should have empty subscriptionMode, got %q", env.SubscriptionMode)
	}

	var buf bytes.Buffer
	wr := ipc.NewWriter(&buf)
	if err := wr.Write(env); err != nil {
		t.Fatalf("write: %v", err)
	}
	line := buf.String()
	if bytes.Contains([]byte(line), []byte(`"subscriptionMode"`)) {
		t.Errorf("channel-mode JSON should not include subscriptionMode field: %s", line)
	}
}

func TestInboundCarriesChannelID(t *testing.T) {
	env := ipc.InboundEnv("guild-999", "chan-111", "@alice", "hello")
	if env.RoutingKey != "guild-999" {
		t.Errorf("routingKey: got %q want guild-999", env.RoutingKey)
	}
	if env.ChannelID != "chan-111" {
		t.Errorf("channelId: got %q want chan-111", env.ChannelID)
	}

	var buf bytes.Buffer
	wr := ipc.NewWriter(&buf)
	if err := wr.Write(env); err != nil {
		t.Fatalf("write: %v", err)
	}
	rd := ipc.NewReader(&buf)
	got, err := rd.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ChannelID != "chan-111" {
		t.Errorf("round-trip channelId: got %q want chan-111", got.ChannelID)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestListGroupsRoundTrip(t *testing.T) {
	req := ipc.ListGroupsEnv()
	if req.Type != "list-groups" {
		t.Errorf("type: got %q want list-groups", req.Type)
	}
	if req.V != ipc.Version {
		t.Errorf("v: got %d want %d", req.V, ipc.Version)
	}

	resp := ipc.ListGroupsRespEnv(`[{"jid":"123@g.us","subject":"Test Group"}]`)
	if resp.Type != "list-groups-resp" {
		t.Errorf("type: got %q want list-groups-resp", resp.Type)
	}

	var buf bytes.Buffer
	wr := ipc.NewWriter(&buf)
	for _, env := range []ipc.Envelope{req, resp} {
		if err := wr.Write(env); err != nil {
			t.Fatalf("write %s: %v", env.Type, err)
		}
	}
	rd := ipc.NewReader(&buf)

	gotReq, err := rd.Read()
	if err != nil {
		t.Fatalf("read req: %v", err)
	}
	if gotReq.Type != "list-groups" {
		t.Errorf("req type: got %q want list-groups", gotReq.Type)
	}

	gotResp, err := rd.Read()
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}
	if gotResp.Type != "list-groups-resp" {
		t.Errorf("resp type: got %q want list-groups-resp", gotResp.Type)
	}
	if gotResp.Content != `[{"jid":"123@g.us","subject":"Test Group"}]` {
		t.Errorf("resp content: got %q", gotResp.Content)
	}
}

func TestJoinGroupRoundTrip(t *testing.T) {
	req := ipc.JoinGroupEnv("ABCxyz123inviteCode")
	if req.Type != "join-group" {
		t.Errorf("type: got %q want join-group", req.Type)
	}
	if req.RoutingKey != "ABCxyz123inviteCode" {
		t.Errorf("routingKey: got %q want ABCxyz123inviteCode", req.RoutingKey)
	}

	resp := ipc.JoinGroupRespEnv("123456789@g.us", "My Cool Group")
	if resp.Type != "join-group-resp" {
		t.Errorf("type: got %q want join-group-resp", resp.Type)
	}
	if resp.RoutingKey != "123456789@g.us" {
		t.Errorf("routingKey: got %q want 123456789@g.us", resp.RoutingKey)
	}
	if resp.Content != "My Cool Group" {
		t.Errorf("content: got %q want My Cool Group", resp.Content)
	}

	var buf bytes.Buffer
	wr := ipc.NewWriter(&buf)
	for _, env := range []ipc.Envelope{req, resp} {
		if err := wr.Write(env); err != nil {
			t.Fatalf("write %s: %v", env.Type, err)
		}
	}
	rd := ipc.NewReader(&buf)

	gotReq, err := rd.Read()
	if err != nil {
		t.Fatalf("read req: %v", err)
	}
	if gotReq.RoutingKey != "ABCxyz123inviteCode" {
		t.Errorf("req routingKey: got %q", gotReq.RoutingKey)
	}

	gotResp, err := rd.Read()
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}
	if gotResp.RoutingKey != "123456789@g.us" {
		t.Errorf("resp jid: got %q", gotResp.RoutingKey)
	}
	if gotResp.Content != "My Cool Group" {
		t.Errorf("resp subject: got %q", gotResp.Content)
	}
}

func TestPeersListRoundTrip(t *testing.T) {
	req := ipc.PeersListEnv("eng")
	if req.Type != "peers-list" {
		t.Errorf("type: got %q want peers-list", req.Type)
	}
	if req.V != ipc.Version {
		t.Errorf("v: got %d want %d", req.V, ipc.Version)
	}
	if req.RoutingKey != "eng" {
		t.Errorf("routingKey: got %q want eng", req.RoutingKey)
	}

	reqAll := ipc.PeersListEnv("")
	if reqAll.RoutingKey != "" {
		t.Errorf("empty team: routingKey should be empty, got %q", reqAll.RoutingKey)
	}

	const payload = `[{"peer_id":"nova","node_id":"local","teams":["eng"],"last_seen_at":"2026-04-27T00:00:00Z","summary":"orchestrator"}]`
	resp := ipc.PeersListRespEnv(payload)
	if resp.Type != "peers-list-resp" {
		t.Errorf("type: got %q want peers-list-resp", resp.Type)
	}
	if resp.Content != payload {
		t.Errorf("content: got %q", resp.Content)
	}

	var buf bytes.Buffer
	wr := ipc.NewWriter(&buf)
	for _, env := range []ipc.Envelope{req, reqAll, resp} {
		if err := wr.Write(env); err != nil {
			t.Fatalf("write %s: %v", env.Type, err)
		}
	}
	rd := ipc.NewReader(&buf)

	gotReq, err := rd.Read()
	if err != nil {
		t.Fatalf("read req: %v", err)
	}
	if gotReq.Type != "peers-list" {
		t.Errorf("req type: got %q want peers-list", gotReq.Type)
	}
	if gotReq.RoutingKey != "eng" {
		t.Errorf("req routingKey: got %q want eng", gotReq.RoutingKey)
	}

	gotReqAll, err := rd.Read()
	if err != nil {
		t.Fatalf("read reqAll: %v", err)
	}
	if gotReqAll.RoutingKey != "" {
		t.Errorf("reqAll routingKey: got %q want empty", gotReqAll.RoutingKey)
	}

	gotResp, err := rd.Read()
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}
	if gotResp.Type != "peers-list-resp" {
		t.Errorf("resp type: got %q want peers-list-resp", gotResp.Type)
	}
	if gotResp.Content != payload {
		t.Errorf("resp content: got %q", gotResp.Content)
	}
}

func TestPeersSetSummaryRoundTrip(t *testing.T) {
	const summary = "orchestrates planning and dispatches tasks to specialists"
	req := ipc.PeersSetSummaryEnv(summary)
	if req.Type != "peers-set-summary" {
		t.Errorf("type: got %q want peers-set-summary", req.Type)
	}
	if req.V != ipc.Version {
		t.Errorf("v: got %d want %d", req.V, ipc.Version)
	}
	if req.Content != summary {
		t.Errorf("content: got %q want %q", req.Content, summary)
	}

	respOK := ipc.PeersSetSummaryRespEnv("")
	if respOK.Type != "peers-set-summary-resp" {
		t.Errorf("ok resp type: got %q want peers-set-summary-resp", respOK.Type)
	}
	if respOK.Reason != "" {
		t.Errorf("ok resp reason: got %q want empty", respOK.Reason)
	}

	const errMsg = "agent not registered with peers broker"
	respErr := ipc.PeersSetSummaryRespEnv(errMsg)
	if respErr.Reason != errMsg {
		t.Errorf("err resp reason: got %q want %q", respErr.Reason, errMsg)
	}

	var buf bytes.Buffer
	wr := ipc.NewWriter(&buf)
	for _, env := range []ipc.Envelope{req, respOK, respErr} {
		if err := wr.Write(env); err != nil {
			t.Fatalf("write %s: %v", env.Type, err)
		}
	}
	rd := ipc.NewReader(&buf)

	gotReq, err := rd.Read()
	if err != nil {
		t.Fatalf("read req: %v", err)
	}
	if gotReq.Type != "peers-set-summary" {
		t.Errorf("req type: got %q want peers-set-summary", gotReq.Type)
	}
	if gotReq.Content != summary {
		t.Errorf("req content: got %q want %q", gotReq.Content, summary)
	}

	gotRespOK, err := rd.Read()
	if err != nil {
		t.Fatalf("read respOK: %v", err)
	}
	if gotRespOK.Type != "peers-set-summary-resp" {
		t.Errorf("respOK type: got %q want peers-set-summary-resp", gotRespOK.Type)
	}
	if gotRespOK.Reason != "" {
		t.Errorf("respOK reason: got %q want empty", gotRespOK.Reason)
	}

	gotRespErr, err := rd.Read()
	if err != nil {
		t.Fatalf("read respErr: %v", err)
	}
	if gotRespErr.Reason != errMsg {
		t.Errorf("respErr reason: got %q want %q", gotRespErr.Reason, errMsg)
	}
}
