package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
)

// meshAPIHTTPClient returns a minimal HTTP client for calling mesh-api over
// HTTPS. Callers inject the node token for Bearer auth.
var meshAPIHTTPClient = &http.Client{Timeout: 15 * time.Second}

// loadMeshAPIBase reads the base URL from ~/.mesh-session.json. The session
// file is written by `mesh auth login` and carries the api base URL.
func loadMeshAPIBase() (string, error) {
	sess, err := auth.LoadSession()
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}
	if sess.BaseURL == "" {
		return "", fmt.Errorf("session base URL is empty; run `mesh auth login` first")
	}
	return auth.NormalizeBaseURL(sess.BaseURL), nil
}

// callMeshAPI makes an authenticated POST to a mesh-api endpoint and decodes
// the JSON response into out (may be nil to discard the body).
func callMeshAPI(ctx context.Context, agentName, path string, body any, out any) (int, error) {
	baseURL, err := loadMeshAPIBase()
	if err != nil {
		return 0, err
	}
	token, err := auth.LoadNodeToken()
	if err != nil {
		return 0, fmt.Errorf("load node token: %w", err)
	}
	token = strings.TrimSpace(token)

	reqBody, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(reqBody))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if agentName != "" {
		req.Header.Set("X-Mesh-Agent-Id", agentName)
	}

	resp, err := meshAPIHTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errBody struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if jsonErr := json.Unmarshal(raw, &errBody); jsonErr == nil {
			msg := errBody.Error
			if msg == "" {
				msg = errBody.Message
			}
			if msg != "" {
				return resp.StatusCode, fmt.Errorf("%s", msg)
			}
		}
		return resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// HandleMeshRecall handles the mesh_recall tool call.
func HandleMeshRecall(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	agentName string,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	query, _ := args["query"].(string)
	if query == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_recall: query is required"},
		})
	}

	reqBody := map[string]any{"query": query}
	if scope, ok := args["scope"].(string); ok && scope != "" {
		reqBody["scope"] = scope
	}
	if kind, ok := args["kind"].(string); ok && kind != "" {
		reqBody["kind"] = kind
	}
	if tags, ok := args["tags"].([]any); ok && len(tags) > 0 {
		strTags := make([]string, 0, len(tags))
		for _, t := range tags {
			if s, ok := t.(string); ok {
				strTags = append(strTags, s)
			}
		}
		if len(strTags) > 0 {
			reqBody["tags"] = strTags
		}
	}
	if topK, ok := args["top_k"].(float64); ok && topK > 0 {
		reqBody["top_k"] = int(topK)
	}

	var respBody struct {
		Results []map[string]any `json:"results"`
	}
	_, err := callMeshAPI(ctx, agentName, "/memory/recall", reqBody, &respBody)
	if err != nil {
		logf("mesh_recall err: %v", err)
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_recall: " + err.Error()},
		})
	}

	out, jsonErr := json.MarshalIndent(respBody, "", "  ")
	if jsonErr != nil {
		out = []byte("{}")
	}
	logf("mesh_recall query=%q results=%d", query, len(respBody.Results))
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(out),
		}},
	}})
}

// HandleMeshRemember handles the mesh_remember tool call.
func HandleMeshRemember(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	agentName string,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	kind, _ := args["kind"].(string)
	content, _ := args["content"].(string)

	if kind == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_remember: kind is required"},
		})
	}
	if content == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_remember: content is required"},
		})
	}
	if len(content) > 4000 {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_remember: content exceeds 4000 characters"},
		})
	}

	reqBody := map[string]any{
		"kind":    kind,
		"content": content,
		"scope":   "project",
	}
	if scope, ok := args["scope"].(string); ok && scope != "" {
		reqBody["scope"] = scope
	}
	if tags, ok := args["tags"].([]any); ok && len(tags) > 0 {
		strTags := make([]string, 0, len(tags))
		for _, t := range tags {
			if s, ok := t.(string); ok {
				strTags = append(strTags, s)
			}
		}
		if len(strTags) > 0 {
			reqBody["tags"] = strTags
		}
	}

	var respBody struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
	}
	_, err := callMeshAPI(ctx, agentName, "/memory/remember", reqBody, &respBody)
	if err != nil {
		logf("mesh_remember err: %v", err)
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: "mesh_remember: " + err.Error()},
		})
	}

	logf("mesh_remember kind=%s id=%s", kind, respBody.ID)
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": fmt.Sprintf(`{"id":"%s","created_at":"%s"}`, respBody.ID, respBody.CreatedAt),
		}},
	}})
}
