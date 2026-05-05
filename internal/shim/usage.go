package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HandleMeshRecordUsage implements the mesh_record_usage internal MCP tool.
// Called by the node daemon shim after forwarding a tool-call result that
// contains a usage block. Hits POST /usage/record with the node token.
func HandleMeshRecordUsage(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	nodeToken string,
	apiBaseURL string,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	agentID, _ := args["agentId"].(string)
	projectID, _ := args["projectId"].(string)
	model, _ := args["model"].(string)

	if agentID == "" || projectID == "" || model == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_record_usage: agentId, projectId, and model are required"},
		})
	}

	payload := map[string]any{
		"agentId":   agentID,
		"projectId": projectID,
		"model":     model,
	}
	for _, f := range []string{"toolCallId", "inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens"} {
		if v, ok := args[f]; ok && v != nil {
			payload[f] = v
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_record_usage: marshal: " + err.Error()},
		})
	}

	req, err := newAPIRequest(ctx, "POST", apiBaseURL+"/usage/record", nodeToken, data)
	if err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_record_usage: request build: " + err.Error()},
		})
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_record_usage: http: " + err.Error()},
		})
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: fmt.Sprintf("mesh_record_usage: api returned %d", resp.StatusCode)},
		})
	}

	logf("mesh_record_usage agentId=%s model=%s", agentID, model)
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": "recorded"}},
	}})
}

// HandleMeshGetProjectCost implements the mesh_get_project_cost agent-callable MCP tool.
// Forwards to GET /usage/cost using the node token.
func HandleMeshGetProjectCost(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	nodeToken string,
	apiBaseURL string,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	projectID, _ := args["project_id"].(string)
	if projectID == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_get_project_cost: project_id is required"},
		})
	}

	u := apiBaseURL + "/usage/cost?projectId=" + projectID
	if since, ok := args["since"].(string); ok && since != "" {
		u += "&since=" + since
	}
	if until, ok := args["until"].(string); ok && until != "" {
		u += "&until=" + until
	}

	req, err := newAPIRequest(ctx, "GET", u, nodeToken, nil)
	if err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_get_project_cost: request build: " + err.Error()},
		})
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_get_project_cost: http: " + err.Error()},
		})
	}
	defer resp.Body.Close()

	var respBody map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_get_project_cost: decode: " + err.Error()},
		})
	}

	if resp.StatusCode >= 300 {
		errMsg, _ := respBody["error"].(string)
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: fmt.Sprintf("mesh_get_project_cost: api %d: %s", resp.StatusCode, errMsg)},
		})
	}

	out, _ := json.Marshal(respBody)
	logf("mesh_get_project_cost projectId=%s", projectID)
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(out)}},
	}})
}

func newAPIRequest(ctx context.Context, method, url, token string, body []byte) (*http.Request, error) {
	var bodyReader *bytesReader
	if body != nil {
		bodyReader = &bytesReader{data: body}
	}
	var req *http.Request
	var err error
	if bodyReader != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, bodyReader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

type bytesReader struct {
	data   []byte
	offset int
}

func (b *bytesReader) Read(p []byte) (int, error) {
	if b.offset >= len(b.data) {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, b.data[b.offset:])
	b.offset += n
	return n, nil
}

// BuildUsageShimTools returns the two usage MCP tool definitions.
func BuildUsageShimTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "mesh_record_usage",
			"description": "Internal tool — record token usage for a tool call. Called by the node daemon after forwarding a result with a usage block. Not for direct agent use.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agentId":          map[string]any{"type": "string"},
					"projectId":        map[string]any{"type": "string"},
					"model":            map[string]any{"type": "string"},
					"toolCallId":       map[string]any{"type": "string"},
					"inputTokens":      map[string]any{"type": "integer"},
					"outputTokens":     map[string]any{"type": "integer"},
					"cacheReadTokens":  map[string]any{"type": "integer"},
					"cacheWriteTokens": map[string]any{"type": "integer"},
				},
				"required": []string{"agentId", "projectId", "model", "inputTokens", "outputTokens"},
			},
		},
		{
			"name":        "mesh_get_project_cost",
			"description": "Query token usage and cost for a project over a time range. Returns totalUsd and per-agent breakdown.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project_id": map[string]any{"type": "string", "description": "Project UUID."},
					"since":      map[string]any{"type": "string", "format": "date-time", "description": "Start of range (ISO 8601). Defaults to 7 days ago."},
					"until":      map[string]any{"type": "string", "format": "date-time", "description": "End of range (ISO 8601). Defaults to now."},
				},
				"required": []string{"project_id"},
			},
		},
	}
}

// suppress unused import for time if not used elsewhere
var _ = time.Now
