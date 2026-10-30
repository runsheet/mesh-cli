package meshapitest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	// Security, set, answers a lease's security reports with a status
	// (202, 422, 503…); 202 when nil.
	Security func(leaseID string, reports []meshapi.SecurityReport) int

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
	streams map[chan meshapi.NodeEvent]struct{}
	// turns are each thread job's turns (Turn), and seen the newest
	// turns_seen a renewal of the job's lease said.
	turns map[string][]meshapi.Turn
	seen  map[string]int64
}

type fakeLease struct {
	brief   meshapi.Brief
	expires time.Time
	ended   string
}

// Call is one request the fake answered.
type Call struct {
	Route   string // register, connect, heartbeat, take, renew, session, evidence, access, usage, progress, security, finish, fail, hand-back, blob
	LeaseID string
	Auth    string
	Body    json.RawMessage
}

// NewFake is a fake over the contract's fixtures.
func NewFake(t testing.TB) *Fake {
	f := &Fake{fx: Load(t), leases: map[string]*fakeLease{}, offered: make(chan struct{}, 1), streams: map[chan meshapi.NodeEvent]struct{}{},
		turns: map[string][]meshapi.Turn{}, seen: map[string]int64{}}
	f.mux = http.NewServeMux()
	f.mux.HandleFunc("POST /v1/nodes", f.register)
	f.mux.HandleFunc("POST /v1/nodes/{id}/heartbeat", f.heartbeat)
	f.mux.HandleFunc("POST /v1/nodes/{id}/connections", f.connect)
	f.mux.HandleFunc("GET /v1/nodes/{id}/events", f.events)
	f.mux.HandleFunc("POST /v1/take", f.take)
	f.mux.HandleFunc("POST /v1/leases/{id}/{route}", f.lease)
	f.mux.HandleFunc("PUT /v1/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		body := f.record(r, "blob", "")
		sum := sha256.Sum256(body)
		if d := r.PathValue("digest"); d != "sha256:"+hex.EncodeToString(sum[:]) {
			writeErr(w, http.StatusUnprocessableEntity, "refused", "the bytes are not "+d)
			return
		}
		writeJSON(w, http.StatusCreated, meshapi.BlobPut{Digest: r.PathValue("digest"), Size: int64(len(body))})
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

// CancelLease has the next heartbeat ask the node to cancel id. As Mesh
// does, the lease stays live until the node hands it back.
func (f *Fake) CancelLease(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancel = append(f.cancel, id)
}

// StreamCancel sends a cancel for the lease over every connected event
// stream and answers how many streams took it.
func (f *Fake) StreamCancel(leaseID, cause, reason string) int {
	f.mu.Lock()
	jobID := ""
	if l := f.leases[leaseID]; l != nil {
		jobID = l.brief.JobID
	}
	f.mu.Unlock()
	data, _ := json.Marshal(meshapi.CancelEvent{LeaseID: leaseID, JobID: jobID, Cause: cause, Reason: reason})
	return f.Stream(meshapi.NodeEvent{Event: meshapi.EventCancel, Data: data})
}

// Stream sends e over every connected event stream and answers how many
// took it.
func (f *Fake) Stream(e meshapi.NodeEvent) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for c := range f.streams {
		select {
		case c <- e:
			n++
		default:
		}
	}
	return n
}

// Streams is how many event streams are connected.
func (f *Fake) Streams() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

// events serves GET /v1/nodes/{id}/events: server-sent events, as Stream
// sends them, until the client goes.
func (f *Fake) events(w http.ResponseWriter, r *http.Request) {
	f.record(r, "events", "")
	f.mu.Lock()
	known := r.PathValue("id") == f.nodeID
	c := make(chan meshapi.NodeEvent, 16)
	if known {
		f.streams[c] = struct{}{}
	}
	f.mu.Unlock()
	if !known {
		writeErr(w, http.StatusForbidden, "forbidden", "not this machine's node")
		return
	}
	defer func() {
		f.mu.Lock()
		delete(f.streams, c)
		f.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	fmt.Fprint(w, ": open\n\n")
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-c:
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Event, e.Data)
			if fl != nil {
				fl.Flush()
			}
		}
	}
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

// Turn adds a turn to the thread job, as a product's POST
// /v1/jobs/{id}/turns would: renewals that say a turns_seen below it
// answer it (projectmesh-io/api ADR 001 amendment, threads:
// "Acknowledged turns"). It reaches no event stream; Stream does that.
func (f *Fake) Turn(jobID string, t meshapi.Turn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns[jobID] = append(f.turns[jobID], t)
}

// TurnsSeen is the newest turns_seen a renewal of the job's lease said,
// -1 when none said one.
func (f *Fake) TurnsSeen(jobID string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.seen[jobID]; ok {
		return s
	}
	return -1
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
	if route == "security" && l != nil {
		// a lease may report while live or after it ended, as Mesh takes it
		var req meshapi.SecurityRequest
		json.Unmarshal(body, &req)
		status := http.StatusAccepted
		if f.Security != nil {
			status = f.Security(id, req.Reports)
		}
		if status != http.StatusAccepted {
			writeErr(w, status, "refused", "the fake refused the reports")
			return
		}
		writeJSON(w, status, meshapi.SecurityAccepted{JobID: l.brief.JobID, LeaseID: id, Accepted: len(req.Reports)})
		return
	}
	if !live {
		writeErr(w, http.StatusConflict, "lease_not_live", "lease "+id+" is not live")
		return
	}
	switch route {
	case "renew":
		f.mu.Lock()
		l.expires = time.Now().Add(f.ttl()).UTC().Truncate(time.Second)
		resp := meshapi.RenewResponse{LeaseID: id, ExpiresAt: l.expires, RenewEveryS: l.brief.RenewEveryS}
		var req meshapi.RenewRequest
		if json.Unmarshal(body, &req) == nil && req.TurnsSeen != nil && l.brief.Thread != nil {
			job := l.brief.JobID
			f.seen[job] = max(f.seen[job], *req.TurnsSeen)
			resp.TurnsThrough = *req.TurnsSeen
			for _, t := range f.turns[job] {
				if t.Seq > *req.TurnsSeen {
					resp.Turns = append(resp.Turns, t)
					resp.TurnsThrough = max(resp.TurnsThrough, t.Seq)
				}
			}
		}
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
		// each id is its statement's digest, as Mesh gives it
		ids := make([]string, len(req.Records))
		for i, rec := range req.Records {
			var env struct {
				Payload string `json:"payload"`
			}
			json.Unmarshal(rec.Envelope, &env)
			payload, err := base64.StdEncoding.DecodeString(env.Payload)
			if err != nil || len(payload) == 0 {
				writeErr(w, http.StatusUnprocessableEntity, "refused", fmt.Sprintf("record %d: the envelope carries no payload", i))
				return
			}
			sum := sha256.Sum256(payload)
			ids[i] = "sha256:" + hex.EncodeToString(sum[:])
		}
		writeJSON(w, http.StatusCreated, meshapi.EvidenceResponse{IDs: ids})
	case "access", "usage":
		w.WriteHeader(http.StatusNoContent)
	case "progress":
		// the lease's newest note, counted as Mesh's LeaseProgress counts
		var req meshapi.ProgressRequest
		json.Unmarshal(body, &req)
		at := time.Now().UTC().Truncate(time.Second)
		f.mu.Lock()
		var notes int64
		for _, c := range f.calls {
			if c.Route == "progress" && c.LeaseID == id {
				notes++
			}
		}
		job := l.brief.JobID
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, meshapi.Progressed{JobID: job, Status: "running",
			Progress: &meshapi.ProgressNote{LeaseID: id, Note: req.Note, At: &at, Notes: notes}})
	case "finish", "fail", "hand-back":
		f.mu.Lock()
		l.ended = route
		state := meshapi.JobState{JobID: l.brief.JobID, Status: "ended", Handbacks: l.brief.Handbacks}
		switch route {
		case "finish":
			state.End = "finished"
		case "fail":
			// only cause work ends the job; any other cause is a
			// hand-back, as Mesh's FailJob has it
			var fr meshapi.FailRequest
			json.Unmarshal(body, &fr)
			if fr.Cause == meshapi.CauseWork {
				state.End = "failed"
				break
			}
			state.Status = "queued"
			state.Handbacks++
		default:
			var hb meshapi.HandBackRequest
			json.Unmarshal(body, &hb)
			if hb.Cause == meshapi.CauseCancelled {
				// a cancelled job's lease ends it cancelled (Mesh wave 1.5)
				state.End = "cancelled"
				break
			}
			if hb.Cause == meshapi.CauseParked {
				// a thread's park is never counted (Mesh wave 4.1)
				state.Status = "parked"
				break
			}
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
