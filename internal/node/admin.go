package node

// The node's admin endpoints (ADR-039, The node's HTTP server), served on
// ~/.mesh/node.sock alone: what the node is doing, read live from its
// loop rather than from the state files a tick leaves. Nothing here
// carries a token, a key or a tool's secret.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// NodeStatus is GET /status.
type NodeStatus struct {
	NodeID     string                `json:"nodeId,omitempty"`
	Name       string                `json:"name,omitempty"`
	Slug       string                `json:"slug,omitempty"`
	Version    string                `json:"version,omitempty"`
	Commit     string                `json:"commit,omitempty"`
	PID        int                   `json:"pid"`
	Started    time.Time             `json:"started"`
	Condition  string                `json:"condition,omitempty"`
	Draining   bool                  `json:"draining"`
	Runtime    *RuntimeState         `json:"runtime,omitempty"`
	Capacity   int                   `json:"capacity"`
	Running    int                   `json:"running"`
	Validating int                   `json:"validating"`
	Health     string                `json:"health,omitempty"`
	Update     string                `json:"updateError,omitempty"`
	Connection []StatusConnection    `json:"connections"`
	Sessions   []gateway.NodeSession `json:"sessions"`
	Held       []StatusHeld          `json:"held"`
	Findings   []Finding             `json:"findings"`
}

// StatusConnection is one org the node serves, by its connection.
type StatusConnection struct {
	Org          string `json:"org"`
	OrgID        string `json:"orgId"`
	ConnectionID string `json:"connectionId,omitempty"`
	Product      string `json:"product,omitempty"`
	Cap          int    `json:"cap"`
}

// StatusHeld is one goal held off the node.
type StatusHeld struct {
	GoalID string    `json:"goalId"`
	Slug   string    `json:"slug"`
	Org    string    `json:"org"`
	Kind   string    `json:"kind"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
}

// StatusJob is one job directory (GET /jobs).
type StatusJob struct {
	JobMeta
	Sessions []string `json:"sessions"`
}

// AdminHandler serves the node's admin endpoints over l and its layout:
//
//	GET /health  200 {"ok": true} while the node claims, 503 with why when
//	             its runtime is broken
//	GET /status  NodeStatus
//	GET /jobs    every job directory, its job.json and its session dirs
//	GET /metrics Prometheus text
func AdminHandler(l *Loop, layout *Layout, started time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if rt := l.RuntimeBroken(); rt != nil {
			writeAdmin(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "runtime": rt})
			return
		}
		writeAdmin(w, http.StatusOK, map[string]any{"ok": true, "draining": l.Draining()})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeAdmin(w, http.StatusOK, StatusOf(l, started))
	})
	mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, r *http.Request) {
		jobs, err := jobsOf(layout)
		if err != nil {
			writeAdmin(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeAdmin(w, http.StatusOK, jobs)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, metricsOf(StatusOf(l, started), layout))
	})
	return mux
}

// StatusOf is the node's status as its loop stands now.
func StatusOf(l *Loop, started time.Time) NodeStatus {
	st := NodeStatus{
		Version: l.Version, Commit: l.Commit, PID: os.Getpid(), Started: started,
		Condition: l.Condition(), Draining: l.Draining(), Runtime: l.RuntimeBroken(),
		Capacity: l.Capacity(), Running: l.Running(), Validating: l.Validating(),
		Health: l.HealthLine(), Update: l.UpdateError(),
		Connection: []StatusConnection{}, Sessions: l.Sessions(), Held: []StatusHeld{}, Findings: l.LatestFindings(),
	}
	if l.Cfg != nil {
		st.NodeID, st.Name, st.Slug = l.Cfg.NodeID, l.Cfg.Name, l.Cfg.Slug
	}
	for _, sc := range l.CurrentScopes() {
		st.Connection = append(st.Connection, StatusConnection{Org: sc.Name(), OrgID: sc.ID, ConnectionID: sc.ConnectionID, Product: sc.Product, Cap: sc.Cap})
	}
	for _, h := range l.HeldGoals() {
		st.Held = append(st.Held, StatusHeld{GoalID: h.GoalID, Slug: h.Slug, Org: h.Org, Kind: h.Kind, Reason: h.Reason, Since: h.Since, Until: h.Until})
	}
	if st.Sessions == nil {
		st.Sessions = []gateway.NodeSession{}
	}
	if st.Findings == nil {
		st.Findings = []Finding{}
	}
	return st
}

// jobsOf is every job under layout with its session dirs.
func jobsOf(layout *Layout) ([]StatusJob, error) {
	out := []StatusJob{}
	if layout == nil {
		return out, nil
	}
	jobs, err := layout.Jobs()
	if err != nil {
		return nil, err
	}
	for _, fj := range jobs {
		j := StatusJob{JobMeta: fj.Meta, Sessions: []string{}}
		entries, _ := os.ReadDir(fj.Dir)
		for _, e := range entries {
			if e.IsDir() && strings.HasSuffix(e.Name(), ".session") {
				j.Sessions = append(j.Sessions, strings.TrimSuffix(e.Name(), ".session"))
			}
		}
		out = append(out, j)
	}
	return out, nil
}

// metricsOf is st and layout's jobs in the Prometheus text format.
func metricsOf(st NodeStatus, layout *Layout) string {
	var b strings.Builder
	gauge := func(name, help string, v float64, labels ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		fmt.Fprintf(&b, "%s%s %g\n", name, labelSet(labels...), v)
	}
	bool01 := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	gauge("mesh_node_capacity", "Sessions the node runs at once.", float64(st.Capacity))
	gauge("mesh_node_running", "Builds running.", float64(st.Running))
	gauge("mesh_node_validating", "Validations running.", float64(st.Validating))
	gauge("mesh_node_draining", "1 while the node drains.", bool01(st.Draining))
	gauge("mesh_node_runtime_broken", "1 while the node's runtime is broken.", bool01(st.Runtime != nil))
	gauge("mesh_node_held_goals", "Goals held off the node.", float64(len(st.Held)))

	kinds := map[string]int{}
	for _, s := range st.Sessions {
		kinds[s.Kind]++
	}
	b.WriteString("# HELP mesh_node_sessions Sessions the node runs, by kind.\n# TYPE mesh_node_sessions gauge\n")
	for _, k := range sortedKeys(kinds) {
		fmt.Fprintf(&b, "mesh_node_sessions%s %d\n", labelSet("kind", k), kinds[k])
	}
	b.WriteString("# HELP mesh_node_connection_cap A connection's cap on concurrent sessions (0 none).\n# TYPE mesh_node_connection_cap gauge\n")
	for _, c := range st.Connection {
		fmt.Fprintf(&b, "mesh_node_connection_cap%s %d\n", labelSet("org", c.Org, "connection", c.ConnectionID), c.Cap)
	}
	b.WriteString("# HELP mesh_node_finding A quartermaster finding: 0 ok, 1 warn, 2 critical.\n# TYPE mesh_node_finding gauge\n")
	for _, f := range st.Findings {
		sev := map[string]int{"ok": 0, "warn": 1, "critical": 2}[f.Severity]
		fmt.Fprintf(&b, "mesh_node_finding%s %d\n", labelSet("kind", f.Kind, "subject", f.Subject), sev)
	}
	if jobs, err := jobsOf(layout); err == nil {
		units := map[string]int{}
		for _, j := range jobs {
			units[j.Unit]++
		}
		b.WriteString("# HELP mesh_node_jobs Job directories on the node, by unit.\n# TYPE mesh_node_jobs gauge\n")
		for _, u := range sortedKeys(units) {
			fmt.Fprintf(&b, "mesh_node_jobs%s %d\n", labelSet("unit", u), units[u])
		}
	}
	return b.String()
}

// promEscape escapes a label value as the text format asks.
var promEscape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// labelSet renders name/value pairs as a Prometheus label set.
func labelSet(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, kv[i]+`="`+promEscape.Replace(kv[i+1])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeAdmin(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ReadStatus is GET /status on the admin socket at sock: the running
// node's status, live. A node not running answers an error.
func ReadStatus(ctx context.Context, sock string) (*NodeStatus, error) {
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://node/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin socket %s: %s", sock, resp.Status)
	}
	var st NodeStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// LiveLines are what `mesh node status` prints of a running node's
// status: its process, its sessions and its connections.
func (st *NodeStatus) LiveLines() []string {
	kinds := map[string]int{}
	for _, s := range st.Sessions {
		kinds[s.Kind]++
	}
	var parts []string
	for _, k := range sortedKeys(kinds) {
		parts = append(parts, fmt.Sprintf("%d %s", kinds[k], k))
	}
	sessions := "no sessions"
	if len(parts) > 0 {
		sessions = strings.Join(parts, ", ")
	}
	state := "running"
	if st.Draining {
		state = "draining"
	}
	lines := []string{fmt.Sprintf("live: %s %s, pid %d, up %s; %s of capacity %d", state, st.Version, st.PID, time.Since(st.Started).Round(time.Second), sessions, st.Capacity)}
	for _, c := range st.Connection {
		cap := "no cap"
		if c.Cap > 0 {
			cap = fmt.Sprintf("cap %d", c.Cap)
		}
		lines = append(lines, fmt.Sprintf("  connection %s (%s, %s) %s", c.Org, c.Product, cap, c.ConnectionID))
	}
	return lines
}
