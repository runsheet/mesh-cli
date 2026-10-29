package meshapi

import (
	"encoding/json"
	"time"
)

// The Mesh v1 node API's shapes (Mesh v1 spec, "Node API" and "The job
// brief"). JSON is snake_case throughout; the field names runsheet ADR
// 079 gave the brief's code (onto, apply, apply_mode, apply_as,
// push_to) are kept. Every shape here has a fixture in testdata, the
// copy of projectmesh-io/api's contracts/v1 this client is held to.

// Capacity is a node's resources: its total, what it cannot offer now,
// or what it has free.
type Capacity struct {
	CPUs  float64 `json:"cpus"`
	MemGB float64 `json:"mem_gb"`
	Slots int     `json:"slots,omitempty"`
}

// Resources are what one job needs.
type Resources struct {
	CPUs  float64 `json:"cpus"`
	MemGB float64 `json:"mem_gb"`
}

// RegisterRequest is POST /v1/nodes: the node as it starts.
type RegisterRequest struct {
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	Harnesses     []string `json:"harnesses"`
	Tools         []string `json:"tools"`
	CapacityTotal Capacity `json:"capacity_total"`
	// SigningKey is the node's public key its ToolRecords are signed
	// with: "ed25519:<base64 raw key>".
	SigningKey string `json:"signing_key"`
}

// Node is Mesh's answer to a registration.
type Node struct {
	NodeID string `json:"node_id"`
	// HeartbeatEveryS is how often Mesh wants a heartbeat; 0 leaves the
	// node's default.
	HeartbeatEveryS int `json:"heartbeat_every_s,omitempty"`
}

// ConnectRequest is POST /v1/nodes/{id}/connections: the node's keeper
// offers it to a tenant for a product (runsheet ADR 066). Mesh accepts it
// on creation when the keeper is the estate admin or owns the tenant;
// otherwise it waits as offered.
type ConnectRequest struct {
	Tenant  string `json:"tenant"`
	Product string `json:"product"`
}

// Connection is a node's connection as Mesh answers it: status offered,
// accepted or revoked, and via estate or own when accepted on creation.
// Only an accepted connection lets the node take the product's jobs for
// the tenant.
type Connection struct {
	ConnectionID string `json:"connection_id"`
	NodeID       string `json:"node_id,omitempty"`
	Tenant       string `json:"tenant"`
	Product      string `json:"product"`
	Status       string `json:"status"`
	Via          string `json:"via,omitempty"`
}

// LeaseBeat is one live lease in a heartbeat.
type LeaseBeat struct {
	LeaseID    string    `json:"lease_id"`
	ProgressAt time.Time `json:"progress_at"`
}

// AccountReport is one model account's standing as the node saw it.
type AccountReport struct {
	AccountRef   string     `json:"account_ref"`
	LimitedUntil *time.Time `json:"limited_until,omitempty"`
}

// HeartbeatRequest is POST /v1/nodes/{id}/heartbeat.
type HeartbeatRequest struct {
	Version       string          `json:"version"`
	CapacityTotal Capacity        `json:"capacity_total"`
	Unavailable   Capacity        `json:"unavailable"`
	Leases        []LeaseBeat     `json:"leases"`
	Accounts      []AccountReport `json:"accounts"`
}

// HeartbeatResponse is what Mesh asks of the node: leases to cancel,
// whether to drain, and how often to renew.
type HeartbeatResponse struct {
	Cancel      []string `json:"cancel"`
	Drain       bool     `json:"drain"`
	RenewEveryS int      `json:"renew_every_s,omitempty"`
}

// TakeRequest is POST /v1/take, a long-poll of at most WaitS (≤25) seconds.
type TakeRequest struct {
	NodeID    string    `json:"node_id"`
	Free      *Capacity `json:"free,omitempty"`
	Harnesses []string  `json:"harnesses"`
	WaitS     int       `json:"wait_s"`
}

// Take is a take's answer: a lease and its brief, or Waiting — why no
// job fits the node now — with both nil.
type Take struct {
	Lease   *Lease `json:"lease,omitempty"`
	Brief   *Brief `json:"brief,omitempty"`
	Waiting string `json:"waiting,omitempty"`
}

// Lease is one node's run of a job.
type Lease struct {
	LeaseID     string    `json:"lease_id"`
	JobID       string    `json:"job_id"`
	N           int       `json:"n"`
	ExpiresAt   time.Time `json:"expires_at"`
	RenewEveryS int       `json:"renew_every_s"`
}

// Brief is everything a node needs to run one lease of a job.
type Brief struct {
	JobID            string            `json:"job_id"`
	LeaseID          string            `json:"lease_id"`
	LeaseExpiresAt   time.Time         `json:"lease_expires_at"`
	RenewEveryS      int               `json:"renew_every_s"`
	Kind             string            `json:"kind"`
	Product          string            `json:"product"`
	Tenant           string            `json:"tenant"`
	Subject          Subject           `json:"subject"`
	LeaseN           int               `json:"lease_n"`
	Handbacks        int               `json:"handbacks"`
	Harness          BriefHarness      `json:"harness"`
	Guidance         string            `json:"guidance"`
	BriefTool        string            `json:"brief_tool"`
	FinishTool       string            `json:"finish_tool"`
	FinishSchema     json.RawMessage   `json:"finish_schema,omitempty"`
	TurnTool         string            `json:"turn_tool,omitempty"`
	Servers          []Server          `json:"servers"`
	NodeTools        []string          `json:"node_tools"`
	EvidenceRequired []string          `json:"evidence_required"`
	Code             *Code             `json:"code,omitempty"`
	Git              *Git              `json:"git,omitempty"`
	Access           *Access           `json:"access,omitempty"`
	Resources        Resources         `json:"resources"`
	Pods             []json.RawMessage `json:"pods,omitempty"`
	Thread           *Thread           `json:"thread,omitempty"`
	CredentialRef    string            `json:"credential_ref,omitempty"`
}

// Subject is what a job is about, in the product's own terms.
type Subject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// BriefHarness is the harness a job runs on, its model policy and budget.
type BriefHarness struct {
	Name   string      `json:"name"`
	Model  ModelPolicy `json:"model"`
	Budget Budget      `json:"budget"`
}

// ModelPolicy is the kind's model: its default, the escalation ladder,
// what escalates (red, conflict, stall, asked) and the ceiling.
type ModelPolicy struct {
	Default  string   `json:"default"`
	Escalate []string `json:"escalate"`
	On       []string `json:"on"`
	Max      string   `json:"max"`
}

// Budget caps a session.
type Budget struct {
	USD     float64 `json:"usd"`
	Minutes int     `json:"minutes"`
	Turns   int     `json:"turns"`
}

// Server is one product MCP server the session reaches through the
// node's sessionproxy.
type Server struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Code is the brief's code inputs (runsheet ADR 079's names).
type Code struct {
	Repo      Repo     `json:"repo"`
	BaseRef   string   `json:"base_ref"`
	BaseSHA   string   `json:"base_sha"`
	Onto      *Onto    `json:"onto,omitempty"`
	Apply     []Apply  `json:"apply,omitempty"`
	ApplyMode string   `json:"apply_mode,omitempty"`
	ApplyAs   *ApplyAs `json:"apply_as,omitempty"`
	PushTo    string   `json:"push_to,omitempty"`
}

// Repo names a repository on its host.
type Repo struct {
	Host     string `json:"host"`
	FullName string `json:"full_name"`
}

// Onto is the commit a job's changes are applied onto.
type Onto struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// Apply is one change to apply.
type Apply struct {
	SHA  string `json:"sha"`
	Ref  string `json:"ref"`
	Base string `json:"base"`
}

// ApplyAs is the identity and date applied commits carry.
type ApplyAs struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

// Git is the job's git policy, opaque to Mesh and enforced by the
// node's relay. Vendor and BaseURL name the git vendor the job reaches
// (Mesh v1 spec, "Many git vendors"): GitHub at Host when empty.
type Git struct {
	Host      string     `json:"host"`
	Vendor    string     `json:"vendor,omitempty"`
	BaseURL   string     `json:"base_url,omitempty"`
	Repo      string     `json:"repo"`
	Access    string     `json:"access"`
	ReadRepos []string   `json:"read_repos,omitempty"`
	Push      []PushRule `json:"push,omitempty"`
}

// PushRule is one ref a push may touch: an exact ref or a prefix
// ending "/*". FastForwardOnly is true when absent: the relay refuses a
// force push unless the rule says false.
type PushRule struct {
	Ref             string `json:"ref"`
	CreateOnly      bool   `json:"create_only"`
	FastForwardOnly *bool  `json:"fast_forward_only,omitempty"`
}

// Access is what a job may reach.
type Access struct {
	Repo  bool        `json:"repo"`
	API   []APIAccess `json:"api,omitempty"`
	MCP   []string    `json:"mcp"`
	Model bool        `json:"model"`
}

// APIAccess is one vendor API the job may call through Latchkey's proxy.
type APIAccess struct {
	Vendor  string   `json:"vendor"`
	Methods []string `json:"methods"`
	Paths   []string `json:"paths"`
}

// Thread is a thread job's timing.
type Thread struct {
	ParkAfterS int `json:"park_after_s"`
	IdleCloseS int `json:"idle_close_s"`
}

// RenewResponse is POST /v1/leases/{id}/renew's answer.
type RenewResponse struct {
	LeaseID     string    `json:"lease_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	RenewEveryS int       `json:"renew_every_s"`
}

// SessionRequest is POST /v1/leases/{id}/session: the thumbprint of the
// P-256 key the session's tokens are bound to.
type SessionRequest struct {
	JKT string `json:"jkt"`
}

// SessionCredential is Mesh's answer, once: the Latchkey session it
// opened as attester and its request credential (lksr_…), which the
// node trades at RequestURL for DPoP-bound mesh-session tokens. String
// never prints the credential.
type SessionCredential struct {
	SessionID    string    `json:"session_id"`
	RequestURL   string    `json:"request_url"`
	RequestToken string    `json:"request_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (s *SessionCredential) String() string {
	if s == nil {
		return "no session"
	}
	return "session " + s.SessionID
}

// Evidence (Mesh v1 spec, "Evidence signing"): each node tool signs
// its own ToolRecord, an in-toto statement (ToolPredicateType) in a DSSE
// envelope, before the model sees its output. The evidence id is
// "sha256:" and the hex SHA-256 of the statement's bytes.
const (
	// ToolPredicateType is a ToolRecord statement's predicate type.
	ToolPredicateType = "https://projectmesh.io/attest/tool/v1"
	// SigningSigstore is a record signed keylessly: a Fulcio certificate
	// for the lease's mesh-session identity, the signature in Rekor.
	SigningSigstore = "sigstore"
	// SigningNode is a record signed by the node's own ed25519 key, when
	// Fulcio or Rekor could not be used.
	SigningNode = "node"
	// NoteNodeSigned is how Mesh marks a node-signed record.
	NoteNodeSigned = "node-signed, unverified identity"
)

// EvidencePost is one signed ToolRecord as the node posts it: the DSSE
// envelope of its statement, how it was signed, Rekor's entry for a
// sigstore signature, and why a node-signed one is not sigstore's.
type EvidencePost struct {
	Signing          string          `json:"signing"`
	Envelope         json.RawMessage `json:"envelope"`
	RekorEntry       json.RawMessage `json:"rekor_entry,omitempty"`
	UnverifiedReason string          `json:"unverified_reason,omitempty"`
}

// EvidenceRequest is POST /v1/leases/{id}/evidence.
type EvidenceRequest struct {
	Records []EvidencePost `json:"records"`
}

// EvidenceResponse names the evidence ids Mesh gave the records, in
// order: each its statement's digest.
type EvidenceResponse struct {
	IDs []string `json:"ids"`
}

// EvidenceRecord is one record as a product reads it (GET
// /v1/evidence/{digest}).
type EvidenceRecord struct {
	ID               string          `json:"id"`
	JobID            string          `json:"job_id"`
	LeaseID          string          `json:"lease_id"`
	LeaseN           int             `json:"lease_n"`
	Tool             string          `json:"tool"`
	Exit             int             `json:"exit"`
	Summary          string          `json:"summary"`
	InputDigest      string          `json:"input_digest"`
	OutputDigest     string          `json:"output_digest,omitempty"`
	StartedAt        time.Time       `json:"started_at"`
	EndedAt          time.Time       `json:"ended_at"`
	Signing          string          `json:"signing"`
	Note             string          `json:"note,omitempty"`
	UnverifiedReason string          `json:"unverified_reason,omitempty"`
	Envelope         json.RawMessage `json:"envelope"`
	RekorEntry       json.RawMessage `json:"rekor_entry,omitempty"`
	RecordedAt       time.Time       `json:"recorded_at"`
}

// BlobPut is PUT /v1/blobs/{digest}'s answer.
type BlobPut struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// AccessRecord is one access the node's relay answered for a lease.
type AccessRecord struct {
	Kind    string    `json:"kind"` // git, api or mcp
	Repo    string    `json:"repo,omitempty"`
	Vendor  string    `json:"vendor,omitempty"`
	Service string    `json:"service,omitempty"`
	Method  string    `json:"method,omitempty"`
	Path    string    `json:"path,omitempty"`
	Refs    []string  `json:"refs,omitempty"`
	Outcome string    `json:"outcome"`
	Reason  string    `json:"reason,omitempty"`
	Bytes   int64     `json:"bytes"`
	At      time.Time `json:"at"`
}

// AccessRequest is POST /v1/leases/{id}/access.
type AccessRequest struct {
	Records []AccessRecord `json:"records"`
}

// ModelUsage is what a lease spent on one model.
type ModelUsage struct {
	Model                 string  `json:"model"`
	AccountRef            string  `json:"account_ref,omitempty"`
	InputTokens           int64   `json:"input_tokens"`
	CachedInputTokens     int64   `json:"cached_input_tokens"`
	CacheWriteInputTokens int64   `json:"cache_write_input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	CostUSD               float64 `json:"cost_usd"`
}

// UsageRequest is POST /v1/leases/{id}/usage.
type UsageRequest struct {
	Usage      []ModelUsage `json:"usage"`
	Turns      int          `json:"turns"`
	DurationMS int64        `json:"duration_ms"`
}

// FinishRequest is POST /v1/leases/{id}/finish: the product's opaque
// result and the session's ref, for a later resume.
type FinishRequest struct {
	Result     json.RawMessage `json:"result"`
	SessionRef string          `json:"session_ref,omitempty"`
	Evidence   []string        `json:"evidence,omitempty"`
}

// FailRequest is POST /v1/leases/{id}/fail: a work failure, which ends
// the job.
type FailRequest struct {
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
}

// HandBackRequest is POST /v1/leases/{id}/hand-back: the lease ends and
// the job is offered again, keeping its place.
type HandBackRequest struct {
	Cause      string `json:"cause"`
	Reason     string `json:"reason"`
	SessionRef string `json:"session_ref,omitempty"`
}

// JobState is the answer of every route that ends a lease: the job's
// status after it.
type JobState struct {
	JobID     string `json:"job_id"`
	Status    string `json:"status"`
	End       string `json:"end,omitempty"`
	Handbacks int    `json:"handbacks"`
}

// The causes a lease ends with (Mesh v1 spec, "Job and Lease").
const (
	CauseWork        = "work"
	CauseCannotRun   = "cannot_run"
	CausePodKilled   = "pod_killed"
	CauseStalled     = "stalled"
	CauseNodeUpdate  = "node_update"
	CauseNodeRestart = "node_restart"
	CausePanic       = "panic"
	CauseRevoked     = "revoked"
	CauseParked      = "parked"
	// CauseCancelled is a node stopping on Mesh's cancel: the job then
	// ends cancelled (projectmesh-io/api wave 1.5).
	CauseCancelled = "cancelled"
)
