// Package gatewaytest is an httptest stand-in for the platform gateway:
// enough GraphQL to run the node's loop and the goal commands against —
// index lists with EQ wheres, gets by id, and the slice-6 commands with
// the aggregates' refusals — plus Prologue's document endpoints.
package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
)

// Call is one mutation the fake received.
type Call struct {
	Name  string
	Input map[string]any
}

// Fake is the server and its state. Rows are maps with camelCase keys,
// as the gateway returns them.
type Fake struct {
	Server *httptest.Server
	Token  string

	mu        sync.Mutex
	UserID    string
	Orgs      []map[string]any
	WorkItems map[string]map[string]any
	Goals     map[string]map[string]any
	Projects  map[string]map[string]any
	// Directions by id, as DirectionIndex rows: one project has several.
	Directions map[string]map[string]any
	Repos      map[string]map[string]any
	Accounts   map[string]map[string]any
	Nodes      map[string]map[string]any
	// Documents by "org/project" → list rows; Bodies by "org/project/slug".
	Documents map[string][]map[string]any
	Bodies    map[string]string
	Calls     []Call
	// Refuse maps a mutation name to a refusal message (returned as a
	// GraphQL error) — for tests of "someone else has it".
	Refuse map[string]string
}

// New starts a fake accepting the given bearer token.
func New(token string) *Fake {
	f := &Fake{
		Token:     token,
		UserID:    "11111111-1111-4111-8111-111111111111",
		WorkItems: map[string]map[string]any{}, Goals: map[string]map[string]any{}, Projects: map[string]map[string]any{},
		Directions: map[string]map[string]any{},
		Repos:      map[string]map[string]any{}, Accounts: map[string]map[string]any{}, Nodes: map[string]map[string]any{},
		Documents: map[string][]map[string]any{}, Bodies: map[string]string{}, Refuse: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", f.graphql)
	mux.HandleFunc("/prologue/v1/", f.prologue)
	f.Server = httptest.NewServer(mux)
	return f
}

// Close stops the server.
func (f *Fake) Close() { f.Server.Close() }

// URL is the fake's base URL.
func (f *Fake) URL() string { return f.Server.URL }

// CallsNamed returns the recorded mutations with that name.
func (f *Fake) CallsNamed(name string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.Calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// Item returns a work item row (a copy).
func (f *Fake) Item(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyRow(f.WorkItems[id])
}

func copyRow(r map[string]any) map[string]any {
	if r == nil {
		return nil
	}
	out := make(map[string]any, len(r))
	for k, v := range r {
		out[k] = v
	}
	return out
}

var (
	mutationRe = regexp.MustCompile(`mutation\s*\(\s*\$in:\s*(\w+)!\s*\)\s*\{\s*(\w+)\s*\(`)
	whereRe    = regexp.MustCompile(`\{\s*field:\s*"([a-z_]+)"\s*,\s*op:\s*EQ\s*,\s*value:\s*"([^"]*)"\s*\}`)
	listRe     = regexp.MustCompile(`\{\s*(\w+Indexs)\s*\(`)
	getRe      = regexp.MustCompile(`\{\s*(\w+Index)\s*\(\s*namespace:\s*"runsheet"\s*,\s*id:\s*"([^"]+)"`)
)

func (f *Fake) graphql(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		writeErrors(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "bad request")
		return
	}
	q := strings.TrimSpace(req.Query)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "mutation { signIn"):
		writeData(w, map[string]any{"signIn": map[string]any{"id": f.UserID}})
	case strings.Contains(q, "myOrgs"):
		writeData(w, map[string]any{"myOrgs": f.Orgs})
	case strings.HasPrefix(q, "mutation"):
		m := mutationRe.FindStringSubmatch(q)
		if m == nil {
			writeErrors(w, 200, "unknown mutation")
			return
		}
		in, _ := req.Variables["in"].(map[string]any)
		if msg := f.Refuse[m[2]]; msg != "" {
			writeErrors(w, 200, msg)
			return
		}
		if err := f.apply(m[2], in); err != nil {
			writeErrors(w, 200, err.Error())
			return
		}
		f.Calls = append(f.Calls, Call{Name: m[2], Input: in})
		writeData(w, map[string]any{m[2]: map[string]any{"status": "ok"}})
	default:
		if m := getRe.FindStringSubmatch(q); m != nil {
			writeData(w, map[string]any{m[1]: f.table(m[1] + "s")[m[2]]})
			return
		}
		m := listRe.FindStringSubmatch(q)
		if m == nil {
			writeErrors(w, 200, "unknown query")
			return
		}
		rows := []map[string]any{}
		for _, row := range f.table(m[1]) {
			ok := true
			for _, wm := range whereRe.FindAllStringSubmatch(q, -1) {
				if fmt.Sprint(row[camel(wm[1])]) != wm[2] {
					ok = false
				}
			}
			if ok {
				rows = append(rows, row)
			}
		}
		writeData(w, map[string]any{m[1]: rows})
	}
}

func (f *Fake) table(name string) map[string]map[string]any {
	switch name {
	case "workItemIndexs":
		return f.WorkItems
	case "goalIndexs":
		return f.Goals
	case "projectIndexs":
		return f.Projects
	case "directionIndexs":
		return f.Directions
	case "repoIndexs":
		return f.Repos
	case "accountIndexs":
		return f.Accounts
	case "nodeIndexs":
		return f.Nodes
	}
	return map[string]map[string]any{}
}

func (f *Fake) apply(name string, in map[string]any) error {
	id, _ := in["aggregateId"].(string)
	item := f.WorkItems[id]
	switch name {
	case "claimWorkItem":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		if item["status"] == "claimed" && item["nodeId"] == in["nodeId"] {
			return nil
		}
		if item["status"] != "ready" {
			return fmt.Errorf("item is %s, not ready", item["status"])
		}
		item["status"], item["nodeId"], item["harness"] = "claimed", in["nodeId"], in["harness"]
	case "reportWorkItemProgress":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		if item["status"] != "claimed" && item["status"] != "running" {
			return fmt.Errorf("item is %s, not running", item["status"])
		}
		item["status"] = "running"
		item["costCents"] = toInt(item["costCents"]) + toInt(in["costCents"])
		if n, _ := in["note"].(string); n != "" {
			item["lastNote"] = n
		}
		if s, _ := in["sessionId"].(string); s != "" {
			item["sessionId"] = s
		}
	case "finishWorkItem":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		if item["status"] != "claimed" && item["status"] != "running" {
			return fmt.Errorf("item is %s, not running", item["status"])
		}
		item["status"], item["branch"], item["headSha"] = "evaluating", in["branch"], in["headSha"]
		item["costCents"] = toInt(item["costCents"]) + toInt(in["costCents"])
	case "failWorkItem":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		item["status"], item["lastError"] = "failed", in["error"]
	case "evaluateWorkItem":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		if in["passed"] == true {
			item["status"] = "review"
		} else {
			item["status"] = "ready"
		}
		item["evaluation"] = in["notes"]
		item["costCents"] = toInt(item["costCents"]) + toInt(in["costCents"])
	case "registerNode":
		row := map[string]any{"id": id, "status": "online", "running": 0}
		for k, v := range in {
			row[k] = v
		}
		f.Nodes[id] = row
	case "heartbeatNode":
		if f.Nodes[id] == nil {
			return fmt.Errorf("node not registered")
		}
		f.Nodes[id]["running"] = in["running"]
		f.Nodes[id]["version"] = in["version"]
	case "setGoal":
		row := f.Goals[id]
		if row == nil {
			row = map[string]any{"id": id, "status": "draft", "spentCents": 0}
			f.Goals[id] = row
		}
		for k, v := range in {
			row[k] = v
		}
	case "claimDirectionResearch", "recordDirectionResearch":
		// the window, the gap and the zone are the real aggregate's to
		// judge; what a test needs here is the claim itself — on, free,
		// and let go on the hand-in
		direction := f.Directions[id]
		if direction == nil {
			return fmt.Errorf("no such direction")
		}
		if name == "claimDirectionResearch" {
			if enabled, _ := direction["enabled"].(bool); !enabled {
				return fmt.Errorf("the direction is off")
			}
			if direction["status"] == "archived" {
				return fmt.Errorf("the direction is archived")
			}
			if held, _ := direction["researchNodeId"].(string); held != "" && held != in["nodeId"] {
				return fmt.Errorf("node %s has held the research", held)
			}
			direction["researchNodeId"] = in["nodeId"]
			return nil
		}
		if held, _ := direction["researchNodeId"].(string); held != in["nodeId"] {
			return fmt.Errorf("the research is not node %s's to hand in", in["nodeId"])
		}
		direction["researchNodeId"] = ""
		direction["proposals"] = in["proposals"]
		direction["researchCostCents"] = toInt(in["costCents"])
		direction["researchNotes"] = in["notes"]
	case "pauseGoal", "resumeGoal", "cancelGoal", "approveGraph", "planGoal", "requestPlan", "claimPlan":
		if f.Goals[id] == nil {
			return fmt.Errorf("no such goal")
		}
		switch name {
		case "pauseGoal":
			f.Goals[id]["status"] = "paused"
		case "resumeGoal":
			f.Goals[id]["status"] = "running"
		case "cancelGoal":
			f.Goals[id]["status"] = "cancelled"
		case "approveGraph":
			f.Goals[id]["status"] = "running"
		case "planGoal":
			f.Goals[id]["status"] = "awaiting_approval"
			f.Goals[id]["spentCents"] = toInt(f.Goals[id]["spentCents"]) + toInt(in["costCents"])
		case "requestPlan":
			f.Goals[id]["status"] = "planning"
		case "claimPlan":
			if held, _ := f.Goals[id]["plannerNodeId"].(string); held != "" && held != in["nodeId"] {
				return fmt.Errorf("another node is planning it")
			}
			f.Goals[id]["plannerNodeId"] = in["nodeId"]
		}
	}
	return nil
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}

func camel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func (f *Fake) prologue(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// /prologue/v1/projects/{org}/{slug}/documents[/{doc}]
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/prologue/v1/projects/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(parts) == 3 && parts[2] == "documents":
		key := parts[0] + "/" + parts[1]
		docs := f.Documents[key]
		if docs == nil {
			docs = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"project": key, "documents": docs})
	case len(parts) == 4 && parts[2] == "documents":
		body, ok := f.Bodies[parts[0]+"/"+parts[1]+"/"+parts[3]]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(body + "\n"))
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func writeData(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeErrors(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": msg}}})
}
