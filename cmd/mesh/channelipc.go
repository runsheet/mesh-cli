package main

// This file is a thin compatibility shim. All IPC types and builders now live
// in internal/ipc; the aliases and wrappers below let the rest of cmd/mesh/
// continue to use the old unexported names without change until Phase 9 rewires
// the final callers directly to internal/ipc.

import (
	"io"
	"net"

	"projectmesh.io/mesh-cli/internal/ipc"
)

const ipcVersion = ipc.Version

type ipcEnvelope = ipc.Envelope
type ipcAttachment = ipc.Attachment
type ipcChannelOverride = ipc.ChannelOverride
type peerEntryWire = ipc.PeerEntryWire

type ipcWriter = ipc.Writer
type ipcReader = ipc.Reader

func newIPCWriter(w io.Writer) *ipcWriter         { return ipc.NewWriter(w) }
func newIPCReader(r io.Reader) *ipcReader         { return ipc.NewReader(r) }
func dialGateway(sockPath string) (net.Conn, *ipcReader, *ipcWriter, error) {
	return ipc.DialGateway(sockPath)
}

func ipcSubscribeChannelEnv(agent string, index int, channelID, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool) ipcEnvelope {
	return ipc.SubscribeChannelEnv(agent, index, channelID, inboundPrefix, allowedUsers, allowedRoles, requireMention)
}

func ipcSubscribeGuildEnv(agent string, index int, guildID, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool, overrides []ipcChannelOverride) ipcEnvelope {
	return ipc.SubscribeGuildEnv(agent, index, guildID, inboundPrefix, allowedUsers, allowedRoles, requireMention, overrides)
}

func ipcSubscribeEnv(agent string, index int, routingKey, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool) ipcEnvelope {
	return ipc.SubscribeEnv(agent, index, routingKey, inboundPrefix, allowedUsers, allowedRoles, requireMention)
}

func ipcSendEnv(channelID, content string) ipcEnvelope {
	return ipc.SendEnv(channelID, content)
}

func ipcSendEnvWithAttachments(target, channelID, content string, attachments []ipcAttachment) ipcEnvelope {
	return ipc.SendEnvWithAttachments(target, channelID, content, attachments)
}

func ipcUnsubscribeEnv(routingKey string) ipcEnvelope {
	return ipc.UnsubscribeEnv(routingKey)
}

func ipcAckEnv(routingKey string) ipcEnvelope {
	return ipc.AckEnv(routingKey)
}

func ipcInboundEnvFull(routingKey, channelID, sender, messageID, userID, timestamp, content string, attachments []ipcAttachment) ipcEnvelope {
	return ipc.InboundEnvFull(routingKey, channelID, sender, messageID, userID, timestamp, content, attachments)
}

func ipcInboundEnv(routingKey, channelID, sender, content string) ipcEnvelope {
	return ipc.InboundEnv(routingKey, channelID, sender, content)
}

func ipcErrorEnv(reason, detail string) ipcEnvelope {
	return ipc.ErrorEnv(reason, detail)
}

func ipcListGroupsEnv() ipcEnvelope {
	return ipc.ListGroupsEnv()
}

func ipcListGroupsRespEnv(content string) ipcEnvelope {
	return ipc.ListGroupsRespEnv(content)
}

func ipcJoinGroupEnv(code string) ipcEnvelope {
	return ipc.JoinGroupEnv(code)
}

func ipcJoinGroupRespEnv(jid, subject string) ipcEnvelope {
	return ipc.JoinGroupRespEnv(jid, subject)
}

func ipcPeersListEnv(team string) ipcEnvelope {
	return ipc.PeersListEnv(team)
}

func ipcPeersListRespEnv(content string) ipcEnvelope {
	return ipc.PeersListRespEnv(content)
}

func ipcPeersSetSummaryEnv(summary string) ipcEnvelope {
	return ipc.PeersSetSummaryEnv(summary)
}

func ipcPeersSetSummaryRespEnv(errReason string) ipcEnvelope {
	return ipc.PeersSetSummaryRespEnv(errReason)
}
