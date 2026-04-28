package main

// TODO(spec22-phase6): channelrouter logic moved to internal/channels.
// This file is a shim; symbols are re-exposed for callers in cmd/mesh.

import (
	"context"

	"projectmesh.io/mesh-cli/internal/channels"
	"projectmesh.io/mesh-cli/internal/peers"
)

// MeshRouterSockPath is re-exported from internal/channels.
func MeshRouterSockPath(agentName string) (string, error) {
	return channels.MeshRouterSockPath(agentName)
}

// ChannelRouter is re-exported from internal/channels.
type ChannelRouter = channels.ChannelRouter

// NewChannelRouter creates an empty ChannelRouter backed by the real runAgentRouter.
// broker is threaded into the router (Phase 7 will replace globalPeersBroker).
// TODO(spec22-phase7): replace globalPeersBroker with parameter threading.
func NewChannelRouter() *ChannelRouter {
	return channels.NewChannelRouter(globalPeersBroker)
}

// newChannelRouterWithStart creates a ChannelRouter with a custom start function.
// Used in tests.
func newChannelRouterWithStart(fn func(ctx context.Context, agentName, description string, chs []ChannelSpec)) *ChannelRouter {
	// Adapt the old 4-arg test stub to the new 5-arg (broker) signature.
	adapted := func(ctx context.Context, agentName, description string, chs []channels.Spec, _ *peers.Broker) {
		fn(ctx, agentName, description, chs)
	}
	return channels.NewChannelRouterWithStart(globalPeersBroker, adapted)
}

// agentEntries converts a []*AgentDef slice to []channels.AgentEntry.
func agentEntries(agents []*AgentDef) []channels.AgentEntry {
	entries := make([]channels.AgentEntry, 0, len(agents))
	for _, a := range agents {
		entries = append(entries, channels.AgentEntry{
			Name:        a.Name,
			Description: a.Description,
			Channels:    a.Channels,
		})
	}
	return entries
}

// filterSelf is re-exported from internal/channels.
func filterSelf(ps []peerRegistration, self string) []peerRegistration {
	return channels.FilterSelf(ps, self)
}

// parseTargetURL is re-exported from internal/channels.
func parseTargetURL(target string) (platform, subtype, id, errMsg string) {
	return channels.ParseTargetURL(target)
}

// routerSubscription is re-exported from internal/channels.
type routerSubscription = channels.RouterSubscription

// ReadRouterSubscriptions is re-exported from internal/channels.
func ReadRouterSubscriptions(agentName string) ([]routerSubscription, error) {
	return channels.ReadRouterSubscriptions(agentName)
}

// routerSubsFile is re-exported from internal/channels.
func routerSubsFile(agentName string) (string, error) {
	return channels.RouterSubsFile(agentName)
}
