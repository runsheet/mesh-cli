package shim

// Federation registry for cross-org resolution in the mesh-shim (§8 of spec 33).
//
// At shim startup the registry is populated from GET /federation/my-memberships on
// the partner's own mesh-api. It is refreshed every 5 minutes in the background.
//
// When mesh_get_task, mesh_list_tasks, or mesh_send (peers:// target)
// resolve a project that is in the federated_projects map, the shim routes the
// request to the host's mesh-api instead of the local router socket.
//
// JWT issuance: the shim reads the Ed25519 private key from ~/.mesh/federation-key
// (written by mesh project accept-invite) and mints a 15-minute JWT for each
// federated call.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FederatedProject describes one project the partner has read_comment access to.
type FederatedProject struct {
	ProjectID      string `json:"project_id"`
	HostOrgSlug    string `json:"host_org_slug"`
	HostAPIURL     string `json:"host_api_url"`
	AgentName      string `json:"agent_name"`
	PartnerOrgSlug string `json:"partner_org_slug"`
	PartnerNodeID  string `json:"partner_node_id"`
}

// FederationRegistry is the in-process registry consulted on every tool call.
type FederationRegistry struct {
	mu       sync.RWMutex
	projects map[string]FederatedProject // keyed by project_id
	privKey  ed25519.PrivateKey
	privOnce sync.Once
}

var globalFedRegistry = &FederationRegistry{
	projects: make(map[string]FederatedProject),
}

// InitFederationRegistry loads the federation key and populates the registry
// from the partner's mesh-api. Should be called once at shim startup (non-fatal
// on error — federation features simply won't be available).
func InitFederationRegistry(baseURL, accessToken, agentName string, logf func(string, ...any)) {
	globalFedRegistry.privOnce.Do(func() {
		key, err := loadFederationPrivKey()
		if err != nil {
			logf("federation: key not loaded (%v) — cross-org routing disabled", err)
			return
		}
		globalFedRegistry.mu.Lock()
		globalFedRegistry.privKey = key
		globalFedRegistry.mu.Unlock()
	})

	refreshFederationRegistry(baseURL, accessToken, agentName, logf)

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			refreshFederationRegistry(baseURL, accessToken, agentName, logf)
		}
	}()
}

func loadFederationPrivKey() (ed25519.PrivateKey, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".mesh", "federation-key")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("federation-key is not valid PEM")
	}
	raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse federation key: %w", err)
	}
	priv, ok := raw.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("federation key is not Ed25519")
	}
	return priv, nil
}

type myMembershipsResp struct {
	OK          bool `json:"ok"`
	Memberships []struct {
		ProjectID          string `json:"project_id"`
		PartnerAgentName   string `json:"partner_agent_name"`
		FederationPartners *struct {
			PartnerOrgSlug string `json:"partner_org_slug"`
			PartnerNodeID  string `json:"partner_node_id"`
		} `json:"federation_partners"`
	} `json:"memberships"`
}

func refreshFederationRegistry(baseURL, accessToken, agentName string, logf func(string, ...any)) {
	url := strings.TrimRight(baseURL, "/") + "/federation/my-memberships"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		logf("federation refresh: build request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logf("federation refresh: http: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logf("federation refresh: unexpected status %d", resp.StatusCode)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logf("federation refresh: read body: %v", err)
		return
	}

	var parsed myMembershipsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		logf("federation refresh: parse: %v", err)
		return
	}

	newProjects := make(map[string]FederatedProject, len(parsed.Memberships))
	for _, m := range parsed.Memberships {
		if m.PartnerAgentName != agentName {
			continue
		}
		fp := FederatedProject{
			ProjectID: m.ProjectID,
			AgentName: m.PartnerAgentName,
		}
		if m.FederationPartners != nil {
			fp.PartnerOrgSlug = m.FederationPartners.PartnerOrgSlug
			fp.PartnerNodeID = m.FederationPartners.PartnerNodeID
		}
		// HostAPIURL and HostOrgSlug are not stored in the memberships query
		// in v1 — they need to be configured separately or embedded in the
		// federation registry. For now we use the partner's mesh-api as a relay.
		// This is a known v1 limitation; the full cross-org URL needs to come
		// from the invite acceptance record.
		newProjects[m.ProjectID] = fp
	}

	globalFedRegistry.mu.Lock()
	globalFedRegistry.projects = newProjects
	globalFedRegistry.mu.Unlock()

	logf("federation registry refreshed: %d federated projects for agent=%s", len(newProjects), agentName)
}

// LookupFederatedProject returns the FederatedProject for a given project ID,
// or false if the project is local.
func LookupFederatedProject(projectID string) (FederatedProject, bool) {
	globalFedRegistry.mu.RLock()
	defer globalFedRegistry.mu.RUnlock()
	fp, ok := globalFedRegistry.projects[projectID]
	return fp, ok
}

// mintFederationJWT creates a signed Ed25519 JWT for a federated call.
// The JWT has a 15-minute TTL and encodes the partner identity in sub.
func mintFederationJWT(partnerOrgSlug, agentName, nodeID, hostOrgSlug string) (string, error) {
	globalFedRegistry.mu.RLock()
	privKey := globalFedRegistry.privKey
	globalFedRegistry.mu.RUnlock()

	if privKey == nil {
		return "", fmt.Errorf("no federation key loaded")
	}

	now := time.Now().Unix()
	header := map[string]any{
		"alg": "EdDSA",
		"typ": "JWT",
	}
	claims := map[string]any{
		"sub":   fmt.Sprintf("%s/%s/%s", partnerOrgSlug, agentName, nodeID),
		"aud":   hostOrgSlug,
		"iat":   now,
		"exp":   now + 900, // 15 minutes
		"scope": "read_comment",
	}

	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	sigInput := headerB64 + "." + claimsB64

	sig := ed25519.Sign(privKey, []byte(sigInput))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return sigInput + "." + sigB64, nil
}

// FederatedGetTask calls the host's POST /federation/task/get.
func FederatedGetTask(fp FederatedProject, taskID string) (string, error) {
	jwt, err := mintFederationJWT(fp.PartnerOrgSlug, fp.AgentName, fp.PartnerNodeID, fp.HostOrgSlug)
	if err != nil {
		return "", fmt.Errorf("mint JWT: %w", err)
	}

	payload := map[string]any{
		"project_id": fp.ProjectID,
		"task_id":    taskID,
	}
	body, _ := json.Marshal(payload)

	url := strings.TrimRight(fp.HostAPIURL, "/") + "/federation/task/get"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("federated get-task: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("federated get-task returned %d: %s", resp.StatusCode, string(respBytes))
	}

	var out struct {
		OK   bool           `json:"ok"`
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return "", fmt.Errorf("parse federated response: %w", err)
	}
	result, _ := json.MarshalIndent(out.Item, "", "  ")
	return string(result), nil
}

// FederatedListTasks calls the host's POST /federation/task/list.
func FederatedListTasks(fp FederatedProject, status string, limit, offset int) (string, error) {
	jwt, err := mintFederationJWT(fp.PartnerOrgSlug, fp.AgentName, fp.PartnerNodeID, fp.HostOrgSlug)
	if err != nil {
		return "", fmt.Errorf("mint JWT: %w", err)
	}

	payload := map[string]any{
		"project_id": fp.ProjectID,
		"limit":      limit,
		"offset":     offset,
	}
	if status != "" {
		payload["status"] = status
	}
	body, _ := json.Marshal(payload)

	url := strings.TrimRight(fp.HostAPIURL, "/") + "/federation/task/list"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("federated list-tasks: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("federated list-tasks returned %d: %s", resp.StatusCode, string(respBytes))
	}

	var out struct {
		OK    bool             `json:"ok"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return "", fmt.Errorf("parse federated response: %w", err)
	}
	result, _ := json.MarshalIndent(out.Items, "", "  ")
	return string(result), nil
}

// FederatedPeerMessage calls the host's POST /federation/peer-message.
func FederatedPeerMessage(fp FederatedProject, targetAgentName, message string) error {
	jwt, err := mintFederationJWT(fp.PartnerOrgSlug, fp.AgentName, fp.PartnerNodeID, fp.HostOrgSlug)
	if err != nil {
		return fmt.Errorf("mint JWT: %w", err)
	}

	payload := map[string]any{
		"project_id":        fp.ProjectID,
		"target_agent_name": targetAgentName,
		"message":           message,
	}
	body, _ := json.Marshal(payload)

	url := strings.TrimRight(fp.HostAPIURL, "/") + "/federation/peer-message"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("federated peer-message: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("federated peer-message returned %d: %s", resp.StatusCode, string(respBytes))
	}
	return nil
}
