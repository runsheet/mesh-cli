package peers_test

import (
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/ipc"
	"projectmesh.io/mesh-cli/internal/peers"
)

func TestBroker_RegisterUnregister(t *testing.T) {
	b := peers.New()
	ch := make(chan ipc.Envelope, 8)
	b.Register(peers.Registration{PeerID: "alice", NodeID: "local", Teams: []string{"eng"}}, ch)

	ps := b.ListPeers("")
	if len(ps) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(ps))
	}
	if ps[0].PeerID != "alice" {
		t.Errorf("peer id: got %q want alice", ps[0].PeerID)
	}

	b.Unregister("alice")
	ps = b.ListPeers("")
	if len(ps) != 0 {
		t.Errorf("expected 0 peers after unregister, got %d", len(ps))
	}

	// Channel should be closed.
	select {
	case _, open := <-ch:
		if open {
			t.Error("delivery channel should be closed after Unregister")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("delivery channel not closed after Unregister")
	}
}

func TestBroker_TeamIndex(t *testing.T) {
	b := peers.New()
	chA := make(chan ipc.Envelope, 8)
	chB := make(chan ipc.Envelope, 8)
	chC := make(chan ipc.Envelope, 8)

	b.Register(peers.Registration{PeerID: "alice", NodeID: "local", Teams: []string{"design", "eng"}}, chA)
	b.Register(peers.Registration{PeerID: "bob", NodeID: "local", Teams: []string{"design"}}, chB)
	b.Register(peers.Registration{PeerID: "carol", NodeID: "local", Teams: []string{"eng"}}, chC)

	designPeers := b.ListPeers("design")
	if len(designPeers) != 2 {
		t.Errorf("expected 2 design peers, got %d", len(designPeers))
	}

	engPeers := b.ListPeers("eng")
	if len(engPeers) != 2 {
		t.Errorf("expected 2 eng peers, got %d", len(engPeers))
	}
}

func TestBroker_SendDirect(t *testing.T) {
	b := peers.New()
	chAlice := make(chan ipc.Envelope, 8)
	b.Register(peers.Registration{PeerID: "alice", NodeID: "local"}, chAlice)

	ok := b.SendDirect("alice", "bob", "hello alice")
	if !ok {
		t.Fatal("SendDirect should return true when peer is registered")
	}

	select {
	case env := <-chAlice:
		if env.Content != "hello alice" {
			t.Errorf("content: got %q want 'hello alice'", env.Content)
		}
		if env.PeerID != "bob" {
			t.Errorf("peer_id: got %q want 'bob'", env.PeerID)
		}
		if env.Source != "peers" {
			t.Errorf("source: got %q want 'peers'", env.Source)
		}
		if env.Team != "" {
			t.Errorf("team should be empty for direct send, got %q", env.Team)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no message received on alice's channel")
	}

	// Send to non-existent peer.
	ok = b.SendDirect("nobody", "bob", "hello")
	if ok {
		t.Error("SendDirect to unregistered peer should return false")
	}
}

func TestBroker_SendTeam_SelfExclusion(t *testing.T) {
	b := peers.New()
	chAlice := make(chan ipc.Envelope, 8)
	chBob := make(chan ipc.Envelope, 8)
	chCarol := make(chan ipc.Envelope, 8)

	b.Register(peers.Registration{PeerID: "alice", NodeID: "local", Teams: []string{"design"}}, chAlice)
	b.Register(peers.Registration{PeerID: "bob", NodeID: "local", Teams: []string{"design"}}, chBob)
	b.Register(peers.Registration{PeerID: "carol", NodeID: "local", Teams: []string{"design"}}, chCarol)

	// alice sends to team/design — she should NOT receive her own message.
	n := b.SendTeam("design", "alice", "team message")
	if n != 2 {
		t.Errorf("SendTeam should reach 2 members (not sender), got %d", n)
	}

	// bob and carol should receive it.
	for _, ch := range []chan ipc.Envelope{chBob, chCarol} {
		select {
		case env := <-ch:
			if env.Content != "team message" {
				t.Errorf("content: got %q want 'team message'", env.Content)
			}
			if env.Team != "design" {
				t.Errorf("team: got %q want 'design'", env.Team)
			}
			if env.PeerID != "alice" {
				t.Errorf("peer_id: got %q want 'alice'", env.PeerID)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("team member did not receive message")
		}
	}

	// alice's channel should be empty.
	select {
	case env := <-chAlice:
		t.Errorf("sender should not receive their own team message, got: %v", env)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestBroker_SendBroadcast_SelfExclusion(t *testing.T) {
	b := peers.New()
	chAlice := make(chan ipc.Envelope, 8)
	chBob := make(chan ipc.Envelope, 8)

	b.Register(peers.Registration{PeerID: "alice", NodeID: "local"}, chAlice)
	b.Register(peers.Registration{PeerID: "bob", NodeID: "local"}, chBob)

	n := b.SendBroadcast("alice", "broadcast")
	if n != 1 {
		t.Errorf("SendBroadcast from alice should reach 1 peer (bob), got %d", n)
	}

	select {
	case env := <-chBob:
		if env.Content != "broadcast" {
			t.Errorf("bob content: got %q want 'broadcast'", env.Content)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("bob did not receive broadcast")
	}

	// alice should not receive her own broadcast.
	select {
	case env := <-chAlice:
		t.Errorf("sender should not receive own broadcast, got: %v", env)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestBroker_SetSummary(t *testing.T) {
	b := peers.New()
	ch := make(chan ipc.Envelope, 8)
	b.Register(peers.Registration{PeerID: "alice", NodeID: "local"}, ch)

	ok := b.SetSummary("alice", "I am the design agent")
	if !ok {
		t.Fatal("SetSummary should return true for registered peer")
	}
	ps := b.ListPeers("")
	if len(ps) != 1 || ps[0].Summary != "I am the design agent" {
		t.Errorf("summary not updated: %+v", ps)
	}

	ok = b.SetSummary("nobody", "ghost")
	if ok {
		t.Error("SetSummary should return false for unregistered peer")
	}
}

func TestBroker_ReRegister(t *testing.T) {
	b := peers.New()
	ch1 := make(chan ipc.Envelope, 8)
	ch2 := make(chan ipc.Envelope, 8)

	b.Register(peers.Registration{PeerID: "alice", NodeID: "local", Teams: []string{"eng"}}, ch1)
	// Re-register with new channel (shim reconnect).
	b.Register(peers.Registration{PeerID: "alice", NodeID: "local", Teams: []string{"eng"}}, ch2)

	b.SendDirect("alice", "bob", "hi")

	select {
	case <-ch2:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("message not delivered to new channel after re-register")
	}
	select {
	case _, open := <-ch1:
		// ch1 should have been closed by the second Register.
		if open {
			t.Error("old delivery channel should be closed on re-register")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("old delivery channel not closed after re-register")
	}
}

func TestBroker_RegisterSummary(t *testing.T) {
	b := peers.New()
	ch := make(chan ipc.Envelope, 8)

	// Register with a non-empty summary (from template description).
	b.Register(peers.Registration{PeerID: "nova", NodeID: "local", Summary: "hello"}, ch)
	ps := b.ListPeers("")
	if len(ps) != 1 || ps[0].Summary != "hello" {
		t.Fatalf("expected summary=hello after first Register, got %+v", ps)
	}

	// Re-register with empty Summary (e.g. router restart, no description change
	// but new delivery channel) — existing summary must be preserved.
	ch2 := make(chan ipc.Envelope, 8)
	b.Register(peers.Registration{PeerID: "nova", NodeID: "local", Summary: ""}, ch2)
	ps = b.ListPeers("")
	if len(ps) != 1 || ps[0].Summary != "hello" {
		t.Errorf("expected preserved summary=hello after re-register with empty Summary, got %+v", ps)
	}

	// SetSummary ("manual") followed by re-register with a template auto-value —
	// the manual value must survive because it is already non-empty.
	b.SetSummary("nova", "manual")
	ch3 := make(chan ipc.Envelope, 8)
	b.Register(peers.Registration{PeerID: "nova", NodeID: "local", Summary: "auto"}, ch3)
	ps = b.ListPeers("")
	if len(ps) != 1 || ps[0].Summary != "auto" {
		// When incoming Summary is non-empty it wins outright (the template
		// description supplied at registration time takes effect).
		t.Errorf("expected summary=auto when re-registering with non-empty Summary, got %+v", ps)
	}

	// SetSummary to "manual" again; re-register with empty Summary → preserve.
	b.SetSummary("nova", "manual")
	ch4 := make(chan ipc.Envelope, 8)
	b.Register(peers.Registration{PeerID: "nova", NodeID: "local", Summary: ""}, ch4)
	ps = b.ListPeers("")
	if len(ps) != 1 || ps[0].Summary != "manual" {
		t.Errorf("expected preserved summary=manual after re-register with empty Summary, got %+v", ps)
	}
}

func TestBroker_UnregisterCleansTeamIndex(t *testing.T) {
	b := peers.New()
	chA := make(chan ipc.Envelope, 8)
	chB := make(chan ipc.Envelope, 8)

	b.Register(peers.Registration{PeerID: "alice", NodeID: "local", Teams: []string{"design"}}, chA)
	b.Register(peers.Registration{PeerID: "bob", NodeID: "local", Teams: []string{"design"}}, chB)

	b.Unregister("alice")

	ps := b.ListPeers("design")
	if len(ps) != 1 || ps[0].PeerID != "bob" {
		t.Errorf("team index not cleaned after Unregister: %+v", ps)
	}
}
