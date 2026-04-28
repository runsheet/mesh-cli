// Package peers implements the in-daemon peer registry and message bus for
// scope=machine. All exported methods are safe for concurrent use.
package peers

import (
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/ipc"
)

// Registration is the in-broker record for one registered agent.
type Registration struct {
	PeerID     string    // agent name (unique within a node in machine scope)
	NodeID     string    // node identifier; "local" in slice 3 (machine scope)
	Teams      []string  // team memberships declared in the template
	Summary    string    // optional self-description set via mesh_set_summary
	LastSeenAt time.Time // updated on register and set-summary
}

// Broker is the in-daemon peers registry and message bus for scope=machine.
type Broker struct {
	mu       sync.Mutex
	peers    map[string]*Registration        // peer_id → registration
	teams    map[string]map[string]bool      // team_name → set of peer_ids
	delivery map[string]chan ipc.Envelope    // peer_id → inbound delivery channel
}

// New creates an empty Broker.
func New() *Broker {
	return &Broker{
		peers:    make(map[string]*Registration),
		teams:    make(map[string]map[string]bool),
		delivery: make(map[string]chan ipc.Envelope),
	}
}

// Register adds (or re-adds) a peer. The delivery channel receives inbound
// ipc.Envelopes destined for this peer. Calling Register while a peer is
// already registered replaces the previous delivery channel — this happens
// when a shim reconnects after a disconnect.
//
// Summary preservation: if the incoming reg.Summary is empty but the existing
// record already has a non-empty summary (set via mesh_set_summary or a
// previous registration), the existing summary is kept. This means a router
// restart does not erase a manually published summary, and a template
// description only seeds the value on first registration.
func (b *Broker) Register(reg Registration, deliver chan ipc.Envelope) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Close the old delivery channel if re-registering (shim reconnect).
	if old, exists := b.delivery[reg.PeerID]; exists {
		close(old)
	}

	// Preserve an existing non-empty summary when the incoming registration
	// does not supply one (e.g. router restart without a template description).
	if reg.Summary == "" {
		if existing, ok := b.peers[reg.PeerID]; ok && existing.Summary != "" {
			reg.Summary = existing.Summary
		}
	}

	reg.LastSeenAt = time.Now().UTC()
	b.peers[reg.PeerID] = &reg
	b.delivery[reg.PeerID] = deliver

	// Rebuild team index for this peer.
	for team, members := range b.teams {
		delete(members, reg.PeerID)
		if len(members) == 0 {
			delete(b.teams, team)
		}
	}
	for _, team := range reg.Teams {
		if b.teams[team] == nil {
			b.teams[team] = make(map[string]bool)
		}
		b.teams[team][reg.PeerID] = true
	}
}

// Unregister removes a peer and closes its delivery channel.
func (b *Broker) Unregister(peerID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	reg, ok := b.peers[peerID]
	if !ok {
		return
	}
	// Remove from team index.
	for _, team := range reg.Teams {
		if members, ok := b.teams[team]; ok {
			delete(members, peerID)
			if len(members) == 0 {
				delete(b.teams, team)
			}
		}
	}
	delete(b.peers, peerID)
	// Close delivery channel so pumpBroker exits cleanly.
	if ch, ok := b.delivery[peerID]; ok {
		close(ch)
		delete(b.delivery, peerID)
	}
}

// SetSummary updates the summary field for a registered peer.
func (b *Broker) SetSummary(peerID, summary string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	reg, ok := b.peers[peerID]
	if !ok {
		return false
	}
	reg.Summary = summary
	reg.LastSeenAt = time.Now().UTC()
	return true
}

// SendDirect delivers a message from fromPeerID to toPeerID. Returns false if
// toPeerID is not registered.
func (b *Broker) SendDirect(toPeerID, fromPeerID, content string) bool {
	b.mu.Lock()
	ch, ok := b.delivery[toPeerID]
	b.mu.Unlock()
	if !ok {
		return false
	}
	env := ipc.Envelope{
		V:         ipc.Version,
		Type:      "inbound",
		Source:    "peers",
		PeerID:    fromPeerID,
		Content:   content,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	select {
	case ch <- env:
	default:
		// Channel full — drop rather than block the sender.
	}
	return true
}

// SendTeam delivers a message from fromPeerID to every member of team,
// excluding the sender. Returns the number of members reached.
func (b *Broker) SendTeam(team, fromPeerID, content string) int {
	b.mu.Lock()
	members := make([]string, 0)
	for memberID := range b.teams[team] {
		if memberID != fromPeerID {
			members = append(members, memberID)
		}
	}
	channels := make([]chan ipc.Envelope, 0, len(members))
	for _, m := range members {
		if ch, ok := b.delivery[m]; ok {
			channels = append(channels, ch)
		}
	}
	b.mu.Unlock()

	env := ipc.Envelope{
		V:         ipc.Version,
		Type:      "inbound",
		Source:    "peers",
		PeerID:    fromPeerID,
		Team:      team,
		Content:   content,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	sent := 0
	for _, ch := range channels {
		select {
		case ch <- env:
			sent++
		default:
		}
	}
	return sent
}

// SendBroadcast delivers a message from fromPeerID to every peer reachable
// within machine scope, excluding the sender. Returns the number reached.
func (b *Broker) SendBroadcast(fromPeerID, content string) int {
	b.mu.Lock()
	chs := make([]chan ipc.Envelope, 0, len(b.delivery))
	for id, ch := range b.delivery {
		if id != fromPeerID {
			chs = append(chs, ch)
		}
	}
	b.mu.Unlock()

	env := ipc.Envelope{
		V:         ipc.Version,
		Type:      "inbound",
		Source:    "peers",
		PeerID:    fromPeerID,
		Content:   content,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	sent := 0
	for _, ch := range chs {
		select {
		case ch <- env:
			sent++
		default:
		}
	}
	return sent
}

// ListPeers returns a snapshot of all registered peers, optionally filtered by
// team. Passing an empty team string returns all peers.
func (b *Broker) ListPeers(team string) []Registration {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []Registration
	if team == "" {
		for _, reg := range b.peers {
			out = append(out, *reg)
		}
		return out
	}
	members := b.teams[team]
	for peerID := range members {
		if reg, ok := b.peers[peerID]; ok {
			out = append(out, *reg)
		}
	}
	return out
}
