package main

// TestParseTargetURL, TestChannelRouterSyncRestartsOnChannelChange, and TestFilterSelf
// have moved to internal/channels/channelrouter_test.go (spec22-phase6).
//
// TestMigrateMCPFile_* have moved to internal/claudeconf/claudeconf_test.go.
//
// TestMeshLastInboundReplyTarget remains here because meshLastInbound is defined
// in channelserver_mesh_shim.go (still in package main; moves in spec22-phase6 shim step).

import "testing"

func TestMeshLastInboundReplyTarget(t *testing.T) {
	l := &meshLastInbound{}

	// No inbound yet.
	source, target := l.ReplyTarget()
	if source != "" || target != "" {
		t.Errorf("expected empty source+target before any inbound, got source=%q target=%q", source, target)
	}

	// Discord channel inbound.
	l.Set("discord", "123456789")
	source, target = l.ReplyTarget()
	if source != "discord" {
		t.Errorf("source: got %q want discord", source)
	}
	if target != "discord://channel/123456789" {
		t.Errorf("target: got %q want discord://channel/123456789", target)
	}
}
