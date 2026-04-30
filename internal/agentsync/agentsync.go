// Package agentsync provides helpers for automatically pushing un-adopted local
// agents to mesh-api and keeping adopted agents in sync with local state.
//
// Local wins. Cloud rows are overwritten on update. Diff/merge semantics is a
// separate spec — this is the v1 "local is truth" model.
//
// Four triggers call into this package:
//
//  1. End of `mesh node register` — bulk sync via node token
//  2. Daemon startup (after gRPC stream connects) — bulk sync via node token
//  3. New local agent created via `mesh run` — single sync via node token callback
//  4. `mesh update <agent>` — single sync via user token (adopt or update)
package agentsync

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/state"
)

// Result summarises a bulk auto-sync run.
type Result struct {
	Pushed  int
	Adopted int // already had a cloudAgentId — skipped in bulk; updated in update trigger
	Failed  int
}

func (r Result) String() string {
	if r.Pushed == 0 && r.Adopted == 0 && r.Failed == 0 {
		return "auto-push agents: no local agents to push"
	}
	return fmt.Sprintf("auto-push agents: pushed=%d already-adopted=%d failed=%d", r.Pushed, r.Adopted, r.Failed)
}

// AdoptOne imports a single local agent into mesh-api using the node token.
// Returns nil on success. Does NOT prompt — on name collision it returns an
// error so the caller can log and skip.
//
// On success, persists the cloudAgentId back to local state.
func AdoptOne(baseURL, nodeToken, agentName string) error {
	def, ok, err := state.GetAgent(agentName)
	if err != nil {
		return fmt.Errorf("load agent def: %w", err)
	}
	if !ok {
		return fmt.Errorf("agent %q not found in local state", agentName)
	}
	if def.CloudAgentID != nil && *def.CloudAgentID != "" {
		return fmt.Errorf("already adopted (cloudAgentId=%s)", *def.CloudAgentID)
	}

	payload := map[string]any{
		"name":  def.Name,
		"model": def.Model,
	}
	if len(def.RequiredEnv) > 0 {
		payload["requiredEnvNames"] = def.RequiredEnv
	}
	if def.TemplateSource != "" {
		payload["templateSource"] = def.TemplateSource
	}
	if def.TemplateSha != "" {
		payload["templateSha"] = def.TemplateSha
	}
	if def.Role != "" {
		payload["role"] = def.Role
	}

	status, body, err := apiRequest(baseURL, nodeToken, http.MethodPost, "/agent/import", payload)
	if err != nil {
		return fmt.Errorf("mesh-api import: %w", err)
	}

	if status == http.StatusConflict {
		var parsed struct {
			Error        string `json:"error"`
			CloudAgentID string `json:"cloudAgentId"`
		}
		_ = json.Unmarshal(body, &parsed)
		if parsed.CloudAgentID != "" {
			// Already exists — record the id locally so future runs skip.
			cloudID := parsed.CloudAgentID
			_ = state.UpdateAgent(agentName, func(a *state.AgentDef) { a.CloudAgentID = &cloudID })
			return fmt.Errorf("name collision in cluster for %q (already exists, recorded id)", agentName)
		}
		return fmt.Errorf("name collision in cluster for %q — rename with `mesh agent push %s --as <new-name>`", agentName, agentName)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("mesh-api import %s: %s", http.StatusText(status), strings.TrimSpace(string(body)))
	}

	var parsed struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("parse import response: %w", err)
	}
	if parsed.ID == "" {
		return fmt.Errorf("mesh-api import: missing id in response")
	}

	cloudID := parsed.ID
	if err := state.UpdateAgent(agentName, func(a *state.AgentDef) { a.CloudAgentID = &cloudID }); err != nil {
		return fmt.Errorf("persist cloudAgentId: %w", err)
	}

	fmt.Printf("mesh auto-push: adopted name=%s cloudAgentId=%s\n", agentName, cloudID)
	return nil
}

// UpdateOne updates an already-adopted agent's cloud row using the given token.
// The token may be a user access token or a node token — the caller is
// responsible for supplying appropriate credentials.
//
// If the agent is not yet adopted (no cloudAgentId), this is a no-op and
// returns nil without logging — the adopt path handles that case.
func UpdateOne(baseURL, token, agentName string) error {
	def, ok, err := state.GetAgent(agentName)
	if err != nil {
		return fmt.Errorf("load agent def: %w", err)
	}
	if !ok {
		return fmt.Errorf("agent %q not found in local state", agentName)
	}
	if def.CloudAgentID == nil || *def.CloudAgentID == "" {
		return nil // not yet adopted; caller should use AdoptOne instead
	}

	payload := map[string]any{
		"id":    *def.CloudAgentID,
		"name":  def.Name,
		"model": def.Model,
	}
	if def.Description != "" {
		payload["description"] = def.Description
	}

	status, body, err := apiRequest(baseURL, token, http.MethodPost, "/agent/update", payload)
	if err != nil {
		return fmt.Errorf("mesh-api update: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("mesh-api update %s: %s", http.StatusText(status), strings.TrimSpace(string(body)))
	}

	fmt.Printf("mesh auto-sync: updated cloudAgentId=%s name=%s\n", *def.CloudAgentID, agentName)
	return nil
}

// SyncOneWithUserToken dispatches on whether the agent is already adopted:
// - not adopted → AdoptOne (node token path — requires nodeToken non-empty)
// - adopted → UpdateOne (user token path)
//
// Used by `mesh update` (trigger 4) where both tokens may be available.
// Pass an empty nodeToken to skip the adopt path (UpdateOne only).
func SyncOneWithUserToken(baseURL, userToken, nodeToken, agentName string) error {
	def, ok, err := state.GetAgent(agentName)
	if err != nil {
		return fmt.Errorf("load agent def: %w", err)
	}
	if !ok {
		return fmt.Errorf("agent %q not found in local state", agentName)
	}
	if def.CloudAgentID == nil || *def.CloudAgentID == "" {
		if nodeToken == "" {
			return fmt.Errorf("agent %q not adopted and no node token available for adopt", agentName)
		}
		return AdoptOne(baseURL, nodeToken, agentName)
	}
	return UpdateOne(baseURL, userToken, agentName)
}

// SyncAll iterates all local agents via the node-token path:
// - not adopted → AdoptOne
// - adopted → UpdateOne (using nodeToken as bearer — node principal)
//
// Errors per agent are accumulated and logged but do not stop the loop.
func SyncAll(baseURL, nodeToken string) Result {
	agents, err := state.ListAgents()
	if err != nil {
		fmt.Printf("mesh auto-push agents: load state: %v\n", err)
		return Result{Failed: 1}
	}
	if len(agents) == 0 {
		return Result{}
	}

	var res Result
	for _, a := range agents {
		if a.CloudAgentID != nil && *a.CloudAgentID != "" {
			// Already adopted: update the cloud row so it reflects local truth.
			if err := UpdateOne(baseURL, nodeToken, a.Name); err != nil {
				fmt.Printf("mesh auto-sync agents: update %s: %v\n", a.Name, err)
				// Non-fatal for update — count as adopted (skipped from push pov).
			}
			res.Adopted++
			continue
		}
		if err := AdoptOne(baseURL, nodeToken, a.Name); err != nil {
			fmt.Printf("mesh auto-push agents: skip %s: %v\n", a.Name, err)
			res.Failed++
			continue
		}
		res.Pushed++
	}
	return res
}

// apiRequest makes a single authenticated HTTP request to mesh-api.
func apiRequest(baseURL, token, method, path string, body any) (int, []byte, error) {
	if baseURL == "" {
		return 0, nil, fmt.Errorf("baseURL is empty")
	}
	url := strings.TrimRight(baseURL, "/") + path
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return resp.StatusCode, nil, rerr
	}
	return resp.StatusCode, raw, nil
}
