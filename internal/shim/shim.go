// Package shim implements the mesh MCP shim — the stdio JSON-RPC server that
// bridges Claude Code to the daemon's per-agent router socket.
package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/channels"
	"projectmesh.io/mesh-cli/internal/ipc"
	"projectmesh.io/mesh-cli/internal/media"
)

// JSONRPCMessage is the loose on-wire envelope. Presence of each field
// disambiguates request / response / notification per JSON-RPC 2.0.
type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// JSONRPCError is the error sub-object.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// BytesTrimSpace trims leading/trailing ASCII whitespace from a byte slice.
func BytesTrimSpace(b []byte) []byte { return bytes.TrimSpace(b) }

// MeshLastInbound records the platform and routing info needed to reply to
// the most recent inbound message across all sources.
type MeshLastInbound struct {
	mu        sync.Mutex
	source    string
	channelID string
}

func (l *MeshLastInbound) Set(source, channelID string) {
	l.mu.Lock()
	l.source = source
	l.channelID = channelID
	l.mu.Unlock()
}

func (l *MeshLastInbound) ReplyTarget() (string, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.source == "" {
		return "", ""
	}
	switch l.source {
	case "discord":
		if l.channelID != "" {
			return l.source, "discord://channel/" + l.channelID
		}
	case "whatsapp":
		if l.channelID != "" {
			return l.source, "whatsapp://chat/" + l.channelID
		}
	case "peers":
		if l.channelID != "" {
			return l.source, "peers://agent/" + l.channelID
		}
	}
	return l.source, ""
}

// shimAttachment is the parsed form of one entry in the MCP attachments array.
type shimAttachment struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	MIMEType string // derived from extension during validation
}

// ParseMeshSendAttachments parses and validates the attachments field from MCP
// args. agentName is used by media.ValidateAttachmentPath. Returns the parsed
// list (nil if none) or a non-empty error string for the tool response.
func ParseMeshSendAttachments(args map[string]any, agentName string) ([]ipc.Attachment, string) {
	raw, ok := args["attachments"]
	if !ok || raw == nil {
		return nil, ""
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, "attachments must be an array"
	}
	if len(list) == 0 {
		return nil, ""
	}
	if len(list) > 1 {
		return nil, "only one attachment supported in v1"
	}

	item, ok := list[0].(map[string]any)
	if !ok {
		return nil, "attachment item must be an object"
	}
	attPath, _ := item["path"].(string)
	attType, _ := item["type"].(string)

	if attType != "image" {
		return nil, "unsupported attachment type in v1: " + attType
	}

	ext := strings.ToLower(filepath.Ext(attPath))
	mime, ok := media.OutboundImageExts[ext]
	if !ok {
		return nil, "unsupported image type: " + ext
	}

	info, err := os.Stat(attPath)
	if err != nil {
		return nil, "attachment file not found: " + attPath
	}
	if info.Size() > media.OutboundMaxBytes {
		return nil, fmt.Sprintf("attachment too large: %d bytes (max 8 MB)", info.Size())
	}
	if err := media.ValidateAttachmentPath(attPath, agentName); err != nil {
		return nil, err.Error()
	}

	return []ipc.Attachment{{Type: attType, Path: attPath, MIMEType: mime}}, ""
}

// HandleMeshSend handles the mesh_send(target, content) tool call.
func HandleMeshSend(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipc.Writer,
	incOut func(),
	logf func(string, ...any),
	write func(JSONRPCMessage) error,
) error {
	target, _ := args["target"].(string)
	content, _ := args["content"].(string)

	if target == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_send: target is required"},
		})
	}

	platform, _, _, parseErr := channels.ParseTargetURL(target)
	if parseErr != "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_send: " + parseErr},
		})
	}

	switch platform {
	case "discord", "whatsapp", "peers":
		// valid
	default:
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_send: unknown scheme " + platform + " (supported: discord://, whatsapp://, peers://)"},
		})
	}

	ipcAtts, attErr := ParseMeshSendAttachments(args, "")
	if attErr != "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_send: " + attErr},
		})
	}

	if content == "" && len(ipcAtts) == 0 {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_send: content or attachment required"},
		})
	}

	env := ipc.SendEnvWithAttachments(target, "", content, ipcAtts)
	if err := routerWr.Write(env); err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "router send: " + err.Error()},
		})
	}
	if incOut != nil {
		incOut()
	}
	logf("mesh_send target=%s len=%d attachments=%d", target, len(content), len(ipcAtts))
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": "sent",
		}},
	}})
}

// HandleMeshReply handles the mesh_reply(content) tool call.
func HandleMeshReply(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipc.Writer,
	incOut func(),
	last *MeshLastInbound,
	logf func(string, ...any),
	write func(JSONRPCMessage) error,
) error {
	content, _ := args["content"].(string)

	ipcAtts, attErr := ParseMeshSendAttachments(args, "")
	if attErr != "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_reply: " + attErr},
		})
	}

	if content == "" && len(ipcAtts) == 0 {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_reply: content or attachment required"},
		})
	}

	source, target := last.ReplyTarget()
	if target == "" {
		msg := "mesh_reply: no prior inbound to reply to"
		if source != "" {
			msg = "mesh_reply: cannot construct reply target for source " + source
		}
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: msg},
		})
	}

	logf("mesh_reply source=%s target=%s", source, target)
	fwdArgs := map[string]any{"target": target, "content": content}
	if atts, ok := args["attachments"]; ok {
		fwdArgs["attachments"] = atts
	}
	return HandleMeshSend(ctx, id, fwdArgs, routerWr, incOut, logf, write)
}

// HandleMeshListPeers handles the mesh_list_peers(team?) tool call.
func HandleMeshListPeers(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipc.Writer,
	pendingResp chan ipc.Envelope,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	team, _ := args["team"].(string)
	if err := routerWr.Write(ipc.PeersListEnv(team)); err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "router peers-list: " + err.Error()},
		})
	}
	select {
	case resp := <-pendingResp:
		logf("mesh_list_peers team=%q resp=%s", team, resp.Content)
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": resp.Content,
			}},
		}})
	case <-ctx.Done():
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_list_peers: context cancelled"},
		})
	}
}

// HandleMeshSetSummary handles the mesh_set_summary(summary) tool call.
func HandleMeshSetSummary(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipc.Writer,
	pendingResp chan ipc.Envelope,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	summary, _ := args["summary"].(string)
	if summary == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_set_summary: summary is required"},
		})
	}
	if err := routerWr.Write(ipc.PeersSetSummaryEnv(summary)); err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "router peers-set-summary: " + err.Error()},
		})
	}
	select {
	case resp := <-pendingResp:
		if resp.Reason != "" {
			logf("mesh_set_summary err: %s", resp.Reason)
			return write(JSONRPCMessage{
				ID:    id,
				Error: &JSONRPCError{Code: -32000, Message: "mesh_set_summary: " + resp.Reason},
			})
		}
		logf("mesh_set_summary ok len=%d", len(summary))
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "summary updated",
			}},
		}})
	case <-ctx.Done():
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_set_summary: context cancelled"},
		})
	}
}

// HandleMeshGetWorkItem handles the mesh_get_work_item(id) tool call.
func HandleMeshGetWorkItem(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipc.Writer,
	pendingResp chan ipc.Envelope,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	workItemID, _ := args["id"].(string)
	if workItemID == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_get_work_item: id is required"},
		})
	}
	if err := routerWr.Write(ipc.WorkItemGetEnv(workItemID)); err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "router work-item-get: " + err.Error()},
		})
	}
	select {
	case resp := <-pendingResp:
		if resp.Reason != "" {
			logf("mesh_get_work_item id=%s err: %s", workItemID, resp.Reason)
			return write(JSONRPCMessage{
				ID:    id,
				Error: &JSONRPCError{Code: -32000, Message: "mesh_get_work_item: " + resp.Reason},
			})
		}
		logf("mesh_get_work_item id=%s ok", workItemID)
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": resp.Content,
			}},
		}})
	case <-ctx.Done():
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_get_work_item: context cancelled"},
		})
	}
}

// HandleMeshUpdateWorkItem handles the mesh_update_work_item(...) tool call.
func HandleMeshUpdateWorkItem(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipc.Writer,
	pendingResp chan ipc.Envelope,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	workItemID, _ := args["id"].(string)
	if workItemID == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_update_work_item: id is required"},
		})
	}
	payload := map[string]any{"workItemId": workItemID}
	for _, field := range []string{"status", "currentOwner", "ownerRole", "note", "priority", "title"} {
		if v, ok := args[field]; ok && v != nil {
			payload[field] = v
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_update_work_item: marshal: " + err.Error()},
		})
	}
	if err := routerWr.Write(ipc.WorkItemUpdateEnv(string(data))); err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "router work-item-update: " + err.Error()},
		})
	}
	select {
	case resp := <-pendingResp:
		if resp.Reason != "" {
			logf("mesh_update_work_item id=%s err: %s", workItemID, resp.Reason)
			return write(JSONRPCMessage{
				ID:    id,
				Error: &JSONRPCError{Code: -32000, Message: "mesh_update_work_item: " + resp.Reason},
			})
		}
		logf("mesh_update_work_item id=%s ok", workItemID)
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "work item updated",
			}},
		}})
	case <-ctx.Done():
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_update_work_item: context cancelled"},
		})
	}
}

// HandleMeshHandoffWorkItem handles the mesh_handoff_work_item(...) tool call.
func HandleMeshHandoffWorkItem(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipc.Writer,
	pendingResp chan ipc.Envelope,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	workItemID, _ := args["id"].(string)
	fromRole, _ := args["fromRole"].(string)
	toRole, _ := args["toRole"].(string)
	if workItemID == "" || fromRole == "" || toRole == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_handoff_work_item: id, fromRole, and toRole are required"},
		})
	}
	payload := map[string]any{
		"workItemId": workItemID,
		"fromRole":   fromRole,
		"toRole":     toRole,
	}
	for _, field := range []string{"summary", "agentId"} {
		if v, ok := args[field]; ok && v != nil {
			if field == "summary" {
				payload["handoffSummary"] = v
			} else {
				payload[field] = v
			}
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_handoff_work_item: marshal: " + err.Error()},
		})
	}
	if err := routerWr.Write(ipc.WorkItemHandoffEnv(string(data))); err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "router work-item-handoff: " + err.Error()},
		})
	}
	select {
	case resp := <-pendingResp:
		if resp.Reason != "" {
			logf("mesh_handoff_work_item id=%s err: %s", workItemID, resp.Reason)
			return write(JSONRPCMessage{
				ID:    id,
				Error: &JSONRPCError{Code: -32000, Message: "mesh_handoff_work_item: " + resp.Reason},
			})
		}
		logf("mesh_handoff_work_item id=%s %s→%s ok", workItemID, fromRole, toRole)
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": fmt.Sprintf("handoff recorded: %s → %s", fromRole, toRole),
			}},
		}})
	case <-ctx.Done():
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_handoff_work_item: context cancelled"},
		})
	}
}

// BuildMeshShimToolsForAgent returns the tools/list payload for the mesh shim.
// When agentName identifies a shipper agent (name starts with "shipper"),
// the six shipper MCP tools are appended.
func BuildMeshShimToolsForAgent(agentName string) []map[string]any {
	tools := BuildMeshShimTools()
	if strings.HasPrefix(agentName, "shipper") {
		tools = append(tools, BuildShipperShimTools()...)
	}
	return tools
}

// BuildMeshShimTools returns the tools/list payload for the mesh shim.
func BuildMeshShimTools() []map[string]any {
	return []map[string]any{
		{
			"name": "mesh_send",
			"description": "Send a message (optionally with an image attachment) to a channel. target is a URL-shape string: " +
				"discord://channel/<channelId> to send to a Discord channel, " +
				"discord://dm/<userId> to open or reuse a DM with a user, " +
				"peers://agent/<name> to send to a specific peer agent, " +
				"peers://team/<name> to broadcast to all members of a team (excluding self), " +
				"peers://broadcast to reach all reachable peers (excluding self).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{
						"type":        "string",
						"description": "URL-shape destination, e.g. discord://channel/1234567890 or peers://agent/nova",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Message text. Becomes the caption when an attachment is present. May be empty if an attachment is supplied.",
					},
					"attachments": map[string]any{
						"type":        "array",
						"description": "Optional. At most one item in v1.",
						"maxItems":    1,
						"items": map[string]any{
							"type":     "object",
							"required": []string{"path", "type"},
							"properties": map[string]any{
								"path": map[string]any{
									"type":        "string",
									"description": "Absolute path to a local image file.",
								},
								"type": map[string]any{
									"type":        "string",
									"enum":        []string{"image"},
									"description": "Media kind. Only 'image' is supported in v1.",
								},
							},
						},
					},
				},
				"required": []string{"target", "content"},
			},
		},
		{
			"name":        "mesh_reply",
			"description": "Reply to the most recent inbound message. Equivalent to calling mesh_send with the source channel as target. Fails if no inbound has been received yet.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "Reply text. May be empty if an attachment is supplied.",
					},
					"attachments": map[string]any{
						"type":        "array",
						"description": "Optional. At most one item in v1.",
						"maxItems":    1,
						"items": map[string]any{
							"type":     "object",
							"required": []string{"path", "type"},
							"properties": map[string]any{
								"path": map[string]any{
									"type":        "string",
									"description": "Absolute path to a local image file.",
								},
								"type": map[string]any{
									"type":        "string",
									"enum":        []string{"image"},
									"description": "Media kind. Only 'image' is supported in v1.",
								},
							},
						},
					},
				},
				"required": []string{"content"},
			},
		},
		{
			"name":        "mesh_list_peers",
			"description": "List reachable peers registered with this daemon. Optional filter by team.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"team": map[string]any{
						"type":        "string",
						"description": "Filter by team name. Omit to list all peers.",
					},
				},
			},
		},
		{
			"name":        "mesh_set_summary",
			"description": "Publish a short self-description visible to other peers via mesh_list_peers.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{
						"type":        "string",
						"description": "Short description of this agent's current focus or capabilities.",
					},
				},
				"required": []string{"summary"},
			},
		},
		{
			"name":        "mesh_get_work_item",
			"description": "Fetch a work item by UUID. Returns the full work item object as JSON.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Work item UUID.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			"name": "mesh_update_work_item",
			"description": "Update mutable fields of a work item (status, currentOwner, ownerRole, priority, title, note). " +
				"All fields except id are optional — only supplied fields are changed. " +
				"Valid statuses: inbox, clarifying, spec_ready, awaiting_approval, ready_for_devlead, in_execution, blocked, in_review, changes_requested, done, archived.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Work item UUID.",
					},
					"status": map[string]any{
						"type":        "string",
						"description": "New status value.",
					},
					"currentOwner": map[string]any{
						"type":        "string",
						"description": "Agent name or UUID to assign as current owner.",
					},
					"ownerRole": map[string]any{
						"type":        "string",
						"description": "Role of the current owner (human, pm, devlead, builder).",
					},
					"priority": map[string]any{
						"type":        "string",
						"description": "Priority: low, medium, high, urgent.",
					},
					"title": map[string]any{
						"type":        "string",
						"description": "New title.",
					},
					"note": map[string]any{
						"type":        "string",
						"description": "Audit note recorded on the event.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			"name": "mesh_handoff_work_item",
			"description": "Record a handoff from one role to another and advance the work item's owner_role. " +
				"fromRole and toRole must be one of: human, pm, devlead, builder. " +
				"A handoff to 'devlead' automatically advances status to ready_for_devlead.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Work item UUID.",
					},
					"fromRole": map[string]any{
						"type":        "string",
						"description": "Role handing off (human, pm, devlead, builder).",
					},
					"toRole": map[string]any{
						"type":        "string",
						"description": "Role receiving the work item.",
					},
					"summary": map[string]any{
						"type":        "string",
						"description": "Optional handoff summary / context for the receiver.",
					},
					"agentId": map[string]any{
						"type":        "string",
						"description": "Optional target agent UUID or name to assign as new owner.",
					},
				},
				"required": []string{"id", "fromRole", "toRole"},
			},
		},
		{
			"name": "mesh_recall",
			"description": "Semantic recall across project memory. Searches prior decisions, learnings, work-item summaries, and notes for the current project. " +
				"Use this before starting work to avoid re-discovering known context.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Natural-language search query.",
					},
					"scope": map[string]any{
						"type":        "string",
						"enum":        []string{"agent", "project", "org", "all"},
						"description": "Memory scope to search. Defaults to 'all' (agent + project + org).",
					},
					"kind": map[string]any{
						"type":        "string",
						"enum":        []string{"decision", "learning", "work_item_summary", "handoff_summary", "note"},
						"description": "Optional filter by memory kind.",
					},
					"tags": map[string]any{
						"type":        "array",
						"description": "Optional tag filter. Returns only records that have all listed tags.",
						"items":       map[string]any{"type": "string"},
					},
					"top_k": map[string]any{
						"type":        "integer",
						"description": "Number of results to return (default 5, max 20).",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			"name": "mesh_remember",
			"description": "Deposit a memory record for the current project. Use for decisions, learnings, or notes you want other agents or future sessions to recall. " +
				"Content is indexed asynchronously; expect up to 60 s before it's searchable.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind": map[string]any{
						"type":        "string",
						"enum":        []string{"decision", "learning", "work_item_summary", "handoff_summary", "note"},
						"description": "Kind of memory record.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Memory content (max 4000 characters).",
					},
					"scope": map[string]any{
						"type":        "string",
						"enum":        []string{"agent", "project", "org"},
						"description": "Scope for the memory. Defaults to 'project' (shared across agents on the same project).",
					},
					"tags": map[string]any{
						"type":        "array",
						"description": "Optional tags to attach for later filtering.",
						"items":       map[string]any{"type": "string"},
					},
				},
				"required": []string{"kind", "content"},
			},
		},
	}
}

// FormatInboundContent builds the final content string sent to Claude.
// Caption is the raw message text; attachments carry downloaded media.
func FormatInboundContent(caption string, attachments []ipc.Attachment) string {
	content := caption
	for _, att := range attachments {
		label := att.Type
		if label == "" {
			label = "image"
		}
		var marker string
		if att.FailReason != "" {
			marker = "[" + label + ": download_failed reason=" + att.FailReason + "]"
		} else if att.Path != "" {
			marker = "[" + label + ": " + att.Path + "]"
		}
		if marker == "" {
			continue
		}
		if content != "" {
			content += "\n"
		}
		content += marker
	}
	return content
}

// ParentPluginAgent parses a null-byte-delimited /proc/<pid>/cmdline string
// and returns the agent name embedded in a --dangerously-load-development-channels
// flag. Returns "" if the flag is absent or the cmdline is malformed.
func ParentPluginAgent(cmdline string) string {
	const needle = "plugin:"
	const suffix = "@mesh"
	for _, arg := range strings.Split(cmdline, "\x00") {
		val := arg
		if strings.HasPrefix(arg, "--dangerously-load-development-channels=") {
			val = strings.TrimPrefix(arg, "--dangerously-load-development-channels=")
		}
		if strings.HasPrefix(val, needle) && strings.HasSuffix(val, suffix) {
			return val[len(needle) : len(val)-len(suffix)]
		}
	}
	return ""
}

// OpenChannelLogFile returns a per-pid log file under the agent's channels dir.
func OpenChannelLogFile(agent, chanType string) (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".mesh", "agents", agent, "channels")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.log", chanType, os.Getpid()))
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}
