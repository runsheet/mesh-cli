package gatewaytest

import (
	"encoding/json"
	"net/http"
	"strings"
)

// meshUsage serves the node's two reports to Mesh (runsheet ADR 078 §5):
// POST /mesh/v1/jobs/{id}/usage, kept in JobUsages for a job the fake
// knows (a mesh session it opened), else a JSON 404 `unknown job`; and
// POST /mesh/v1/nodes/{id}/accounts, kept in NodeAccounts. Each answers
// 204. NoMeshUsage answers both a plain 404. It reports false for any
// other path.
func (f *Fake) meshUsage(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	var kind, id string
	switch {
	case strings.HasPrefix(path, "/mesh/v1/jobs/") && strings.HasSuffix(path, "/usage"):
		kind, id = "usage", strings.TrimSuffix(strings.TrimPrefix(path, "/mesh/v1/jobs/"), "/usage")
	case strings.HasPrefix(path, "/mesh/v1/nodes/") && strings.HasSuffix(path, "/accounts"):
		kind, id = "accounts", strings.TrimSuffix(strings.TrimPrefix(path, "/mesh/v1/nodes/"), "/accounts")
	default:
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.NoMeshUsage || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return true
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeREST(w, http.StatusBadRequest, map[string]any{"message": "bad json"})
		return true
	}
	switch kind {
	case "usage":
		if f.MeshSessions[id] == nil && !f.Jobs[id] {
			writeREST(w, http.StatusNotFound, map[string]any{"message": "unknown job"})
			return true
		}
		if ref, _ := body["account_ref"].(string); ref == "" {
			writeREST(w, http.StatusBadRequest, map[string]any{"message": "account_ref is required"})
			return true
		}
		if f.JobUsages == nil {
			f.JobUsages = map[string][]map[string]any{}
		}
		f.JobUsages[id] = append(f.JobUsages[id], body)
	case "accounts":
		if f.NodeAccounts == nil {
			f.NodeAccounts = map[string][]map[string]any{}
		}
		f.NodeAccounts[id] = append(f.NodeAccounts[id], body)
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}
