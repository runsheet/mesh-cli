package meshapitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// Fake is a stateful Mesh node API for nodev1's tests: register,
// heartbeat, take, and the lease routes, each answering in its fixture's
// shape — a take's brief is the take-lease fixture's, changed by the
// test's Offer — and recording every call.
type Fake struct {
	// TakeWait is how long a take with nothing offered waits before its
	// 204; 50 ms when zero.
	TakeWait time.Duration
	// LeaseTTL is a lease's life from its take or renew; 90 s when zero.
	LeaseTTL time.Duration
	// Session answers POST /v1/leases/{id}/session; the lease-session
	// fixture's answer when nil.
	Session func(leaseID, jkt string) (*meshapi.SessionCredential, int)

	fx      map[string]Fixture
	mu      sync.Mutex
	mux     *http.ServeMux
	nodeID  string
	seq     int
	offers  []meshapi.Brief
	leases  map[string]*fakeLease
	calls   []Call
	cancel  []string
	drain   bool
	offered chan struct{}
}

type fakeLease struct {
	brief   meshapi.Brief
	expires time.Time
	ended   string
}

// Call is one request the fake answered.
type Call struct {
	Route   string // register, connect, heartbeat, take, renew, session, evidence, access, usage, finish, fail, hand-back, blob
	LeaseID string
	Auth    string
	Body    json.RawMessage
}

// NewFake is a fake over the contract's fixtures.
func NewFake(t testing.TB) *Fake {
	f := &Fake{fx: Load(t), leases: map[string]*fakeLease{}, offered: make(chan struct{}, 1)}
	f.mux = http.NewServeMux()
	f.mux.HandleFunc("POST /v1/nodes", f.register)
	f.mux.HandleFunc("POST /v1/nodes/{id}/heartbeat", f.heartbeat)
	f.mux.HandleFunc("POST /v1/nodes/{id}/connections", f.connect)
	f.mux.HandleFunc("POST /v1/take", f.take)
	f.mux.HandleFunc("POST /v1/leases/{id}/{route}", f.lease)
	f.mux.HandleFunc("PUT /v1/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "blob", "")
		w.WriteHeader(http.StatusCreated)
	})
	return f
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "no bearer")
		return
	}
	f.mux.ServeHTTP(w, r)
}

// Offer queues a job whose brief is the take-lease fixture's, changed by
// mut; it answers the job's id.
func (f *Fake) Offer(mut func(*meshapi.Brief)) string {
	var tk meshapi.Take
	if err := json.Unmarshal(f.fx["take-lease"].Response, &tk); err != nil || tk.Brief == nil {
		panic("meshapitest: the take-lease fixture has no brief")
	}
	b := *tk.Brief
	f.mu.Lock()
	f.seq++
	b.JobID = fmt.Sprintf("job_%d", f.seq)
	f.mu.Unlock()
	if mut != nil {
		mut(&b)
	}
	f.mu.Lock()
	f.offers = append(f.offers, b)
	f.mu.Unlock()
	select {
	case f.offered <- struct{}{}:
	default:
	}
	return b.JobID
}

// CancelLease has the next heartbeat ask the node to cancel id.
func (f *Fake) CancelLease(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancel = append(f.cancel, id)
}

// ExpireLease ends id as Mesh's timer would: its renew and every other
// lease route then answer 409 lease_not_live.
func (f *Fake) ExpireLease(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.leases[id]; l != nil && l.ended == "" {
		l.ended = "expired"
	}
}

// Drain has the next heartbeat ask the node to drain.
func (f *Fake) Drain() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drain = true
}

// Calls are the calls of route ("" for all), in order.
func (f *Fake) Calls(route string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.calls {
		if route == "" || c.Route == route {
			out = append(out, c)
		}
	}
	return out
}

// LeaseOf is the lease the job's last take made, "" when none.
func (f *Fake) LeaseOf(jobID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := ""
	for lid, l := range f.leases {
		if l.brief.JobID == jobID && lid > id {
			id = lid
		}
	}
	return id
}

// Ended is how the lease ended: finish, fail, hand-back, expired, or ""
// while it is live.
func (f *Fake) Ended(leaseID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.leases[leaseID]; l != nil {
		return l.ended
	}
	return ""
}

func (f *Fake) record(r *http.Request, route, lease string) json.RawMessage {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Route: route, LeaseID: lease, Auth: r.Header.Get("Authorization"), Body: body})
	return body
}

func (f *Fake) register(w http.ResponseWriter, r *http.Request) {
	body := f.record(r, "register", "")
	var req meshapi.RegisterRequest
	if json.Unmarshal(body, &req) != nil || req.Name == "" || req.SigningKey == "" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid", "a node needs a name and a signing key")
		return
	}
	var n meshapi.Node
	json.Unmarshal(f.fx["nodes-register"].Response, &n)
	f.mu.Lock()
	f.nodeID = n.NodeID
	f.mu.Unlock()
	writeJSON(w, f.fx["nodes-register"].Status, n)
}

// connect accepts every offer as the nodes-connect fixture does, for
// the tenant and product asked.
func (f *Fake) connect(w http.ResponseWriter, r *http.Request) {
	body := f.record(r, "connect", "")
	var req meshapi.ConnectRequest
	if json.Unmarshal(body, &req) != nil || req.Tenant == "" || req.Product == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "tenant and product are required")
		return
	}
	f.mu.Lock()
	known := r.PathValue("id") == f.nodeID
	f.mu.Unlock()
	if !known {
		writeErr(w, http.StatusConflict, "node_not_registered", "the node has not registered")
		return
	}
	var c meshapi.Connection
	json.Unmarshal(f.fx["nodes-connect"].Response, &c)
	c.NodeID, c.Tenant, c.Product = r.PathValue("id"), req.Tenant, req.Product
	writeJSON(w, f.fx["nodes-connect"].Status, c)
}

func (f *Fake) heartbeat(w http.ResponseWriter, r *http.Request) {
	f.record(r, "heartbeat", "")
	f.mu.Lock()
	known := r.PathValue("id") == f.nodeID
	resp := meshapi.HeartbeatResponse{Cancel: f.cancel, Drain: f.drain, RenewEveryS: 30}
	if resp.Cancel == nil {
		resp.Cancel = []string{}
	}
	for _, id := range f.cancel {
		if l := f.leases[id]; l != nil && l.ended == "" {
			l.ended = "cancelled"
		}
	}
	f.cancel = nil
	f.mu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "no_such_node", "no such node")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (f *Fake) take(w http.ResponseWriter, r *http.Request) {
	f.record(r, "take", "")
	wait := f.TakeWait
	if wait == 0 {
		wait = 50 * time.Millisecond
	}
	deadline := time.After(wait)
	for {
		f.mu.Lock()
		if len(f.offers) > 0 {
			b := f.offers[0]
			f.offers = f.offers[1:]
			f.seq++
			ttl := f.ttl()
			b.LeaseID = fmt.Sprintf("lse_%d", f.seq)
			b.LeaseExpiresAt = time.Now().Add(ttl).UTC().Truncate(time.Second)
			f.leases[b.LeaseID] = &fakeLease{brief: b, expires: b.LeaseExpiresAt}
			f.mu.Unlock()
			writeJSON(w, http.StatusOK, meshapi.Take{
				Lease: &meshapi.Lease{LeaseID: b.LeaseID, JobID: b.JobID, N: b.LeaseN, ExpiresAt: b.LeaseExpiresAt, RenewEveryS: b.RenewEveryS},
				Brief: &b,
			})
			return
		}
		f.mu.Unlock()
		select {
		case <-f.offered:
		case <-deadline:
			WriteFixture(w, f.fx["take-waiting"])
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (f *Fake) ttl() time.Duration {
	if f.LeaseTTL > 0 {
		return f.LeaseTTL
	}
	return 90 * time.Second
}

func (f *Fake) lease(w http.ResponseWriter, r *http.Request) {
	id, route := r.PathValue("id"), r.PathValue("route")
	body := f.record(r, route, id)
	f.mu.Lock()
	l := f.leases[id]
	live := l != nil && l.ended == ""
	f.mu.Unlock()
	if !live {
		writeErr(w, http.StatusConflict, "lease_not_live", "lease "+id+" is not live")
		return
	}
	switch route {
	case "renew":
		f.mu.Lock()
		l.expires = time.Now().Add(f.ttl()).UTC().Truncate(time.Second)
		resp := meshapi.RenewResponse{LeaseID: id, ExpiresAt: l.expires, RenewEveryS: l.brief.RenewEveryS}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, resp)
	case "session":
		var req meshapi.SessionRequest
		if json.Unmarshal(body, &req) != nil || req.JKT == "" {
			writeErr(w, http.StatusUnprocessableEntity, "invalid", "a session needs the key's jkt")
			return
		}
		if f.Session != nil {
			s, status := f.Session(id, req.JKT)
			writeJSON(w, status, s)
			return
		}
		WriteFixture(w, f.fx["lease-session"])
	case "evidence":
		var req meshapi.EvidenceRequest
		json.Unmarshal(body, &req)
		ids := make([]string, len(req.Records))
		for i := range ids {
			ids[i] = fmt.Sprintf("ev_%s_%d", id, i)
		}
		writeJSON(w, http.StatusCreated, meshapi.EvidenceResponse{IDs: ids})
	case "access", "usage":
		w.WriteHeader(http.StatusNoContent)
	case "finish", "fail", "hand-back":
		f.mu.Lock()
		l.ended = route
		state := meshapi.JobState{JobID: l.brief.JobID, Status: "ended", Handbacks: l.brief.Handbacks}
		switch route {
		case "finish":
			state.End = "finished"
		case "fail":
			state.End = "failed"
		default:
			state.Status = "queued"
			state.Handbacks++
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, state)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no route "+route)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}
