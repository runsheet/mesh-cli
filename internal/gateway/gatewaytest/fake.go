// Package gatewaytest is an httptest stand-in for the platform gateway:
// enough GraphQL to run the node's loop and the goal commands against —
// index lists with EQ wheres, gets by id, and the slice-6 commands with
// the aggregates' refusals — plus mesh-context's document endpoints
// (/mesh/v1, with /prologue/v1 kept as the dated alias).
package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Call is one mutation the fake received.
type Call struct {
	Name  string
	Input map[string]any
	// At is when the fake received it: an item's heartbeats over time.
	At time.Time
	// Bearer is the token the request carried.
	Bearer string
	// Query is the mutation's raw GraphQL document, for a test that
	// must assert on the selection the client sent — a field probed
	// for and included only when the schema takes it (takesScopeShares
	// and the like) rather than one carried as input.
	Query string
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
	// Agents by id, as AgentIndex rows (runsheet trd §6e).
	Agents map[string]map[string]any
	// Credentials are CredentialIndex rows (runsheet trd §6i), served
	// by credentialIndexs on org_id or enterprise_id; no secret.
	Credentials []map[string]any
	// Documents by "org/project" → list rows; Bodies by "org/project/slug".
	Documents map[string][]map[string]any
	Bodies    map[string]string
	// NoMeshV1 answers every /mesh/v1 context request with 404, as a
	// platform that serves only the /prologue/v1 alias — for a test that
	// the client falls back to it.
	NoMeshV1 bool
	// MeshV1Hits and PrologueV1Hits count the requests each context path
	// received, for a test to prove which one a read actually used.
	MeshV1Hits, PrologueV1Hits int
	// ItemBearers are further tokens the fake takes on GraphQL, beside
	// Token: an item's claim-time token, as RecordValidationAttestation
	// carries it (WithItemToken).
	ItemBearers []string
	Calls       []Call
	// Reads are the index lists and get-by-id reads the fake served:
	// Name is the list (goalIndexs) or the get (repoIndex), Input its EQ
	// wheres by field (org_id) or, for a get, {"id": id}.
	Reads []Call
	// Refuse maps a mutation name to a refusal message (returned as a
	// GraphQL error) — for tests of "someone else has it".
	Refuse map[string]string
	// FailReads maps an index list name (goalIndexs) or a get
	// (workItemIndex) to an error it answers with instead of rows; a
	// list's key may name one where, workItemIndexs/status=review, to
	// fail only the lists that filter by it; "myOrgs" fails MyOrgs.
	FailReads map[string]string
	// GitHubTokens maps an item id to the issueItemRepoToken answer
	// (token, expiresAt, repository); only the node holding the item's
	// claim is given it.
	GitHubTokens map[string]map[string]any
	// PlanGitHubTokens maps a goal id to its planGitHubToken answer: one
	// row per repo enrolled in the goal's project ({token, expiresAt,
	// repository}); only the node holding the goal's plan claim
	// (plannerNodeId) is given them, and a project repo with no row
	// among them refuses the whole read, naming it (adr-022).
	PlanGitHubTokens map[string][]map[string]any
	// ResearchGitHubTokens is PlanGitHubTokens for a direction's
	// researchGitHubToken answer, keyed by direction id and held by its
	// own research claim (researchNodeId).
	ResearchGitHubTokens map[string][]map[string]any
	// TokenNodeID is the node the caller's credential names; the platform
	// hands an item's token only to the node holding its claim. Empty
	// skips the check.
	TokenNodeID string
	// Unsupported names mutations the fake answers as a platform that
	// predates them: the schema's "Cannot query field" error.
	Unsupported map[string]bool
	// UnknownFields names fields the fake's schema does not have, as a
	// platform older than them: a read selecting one is refused with
	// "Cannot query field", a command whose input (or an item of it)
	// carries one with "Unknown field".
	UnknownFields map[string]bool
	// NoNoVerdict is a platform older than runsheet ADR 035: its
	// EvaluateWorkItemInput has no `noVerdict` (nor `noVerdictReason`),
	// introspection says so, and an evaluateWorkItem carrying one is
	// refused with "Unknown field".
	NoNoVerdict bool
	// NoFailedClauses is a platform whose EvaluateWorkItemInput has no
	// failedClauses: the node sends the verdict without them.
	NoFailedClauses bool
	// NoProtectedFields is a platform whose WorkItemIndex has no
	// protectedFiles/protectedRulePaths and whose ProjectIndex has no
	// protectedPaths (goal one-evaluator-for-ordinary-work): a selection
	// naming one of the three is refused with "Unknown field", as
	// UnknownFields refuses any other field — this is the convenience
	// flag for all three at once.
	NoProtectedFields bool
	// NoEvaluations is a platform whose WorkItemIndex has no
	// `evaluations`: the read WorkItemPriorAttempts makes is refused
	// with "Cannot query field", as the client tolerates (the
	// evaluation still runs, without the lineage's earlier verdicts).
	NoEvaluations bool
	// PlanQuestions is a platform whose PlanGoalInput has `questions`
	// (ADR-020); without it, as the platform is today, the field is
	// left out of the introspection and refused on planGoal.
	PlanQuestions bool
	// ItemPrecondition is a platform whose PlannedItemInput has
	// `precondition` (ADR-023's amendment, the cut-over rule); without
	// it, as the platform is today, the field is left out of the
	// introspection and a planGoal item carrying it is refused.
	ItemPrecondition bool
	// NoReplaceItem is a platform whose RequestPlanInput has no
	// `replaceItem` (before the narrow re-plan): introspection leaves it
	// out and a requestPlan carrying it is refused with "Unknown field".
	NoReplaceItem bool
	// NoActor is a platform older than every-actor-has-a-name: its
	// ClaimWorkItemInput, ReportWorkItemProgressInput,
	// FinishWorkItemInput, EvaluateWorkItemInput and PlanGoalInput have
	// no `actor`, introspection leaves it out, and a command of the five
	// carrying it is refused with "Unknown field".
	NoActor bool
	// NoDescription is a platform older than every-actor-has-a-name item
	// 3: its FinishWorkItemInput has no `description`, introspection
	// leaves it out, and a finishWorkItem carrying it is refused with
	// "Unknown field".
	NoDescription bool
	// GoalSplit is a platform whose Mutation has proposeGoalSplit
	// (ADR-023, the size rule): false, as the platform is today (goal
	// a-goal-fits-in-eight-items is still queued), leaves the field out
	// of the Mutation introspection and refuses a call to it as a schema
	// without it, the way an absent recordValidationAttestation does.
	GoalSplit bool
	// PlanRequests is every requestPlan the fake was sent, refused or
	// not, with its replaceItem ("" for a whole re-plan); refuse one
	// with Refuse["requestPlan"].
	PlanRequests []PlanRequest
	// PlanReleases is every releaseGoalPlan the fake was sent: a plan
	// claim given up with the refusal's text as the reason (goal
	// a-refused-plan-is-released-not-replanned).
	PlanReleases []PlanRelease
	// ClaimRefusals maps an item id to the message claimWorkItem answers
	// for it as the gateway error, the item left ready (goal
	// a-refused-plan-is-released-not-replanned): a claim refused for
	// budget, pause or approval.
	ClaimRefusals map[string]string
	// AttestationReceipt is what recordValidationAttestation answers on
	// success — a test sets the contract fixture's result; nil answers
	// {status: ok, commit: <the input's commit>}. Unsupported[
	// "recordValidationAttestation"] leaves the field out of the
	// Mutation introspection (and refuses it as a schema without it),
	// and Refuse["recordValidationAttestation"] answers its words as a
	// GraphQL error, as the platform refuses a commit that is not the
	// branch head.
	AttestationReceipt map[string]any
	// RateLimit429 names a mutation the fake answers with a bare HTTP 429
	// ("rate limited", no GraphQL error body) every time it is sent — for
	// a test of the retry-and-give-up path (retryOnTransient,
	// AttestationBackoff); never combined with Refuse or Unsupported for
	// the same mutation.
	RateLimit429 map[string]bool
	// TransientFirst is, per mutation, how many leading calls the fake
	// answers transiently before it answers as it otherwise would — for a
	// test of the retry-then-succeed path (retryOnTransient,
	// AttestationRetryFor). TransientAs says how, per mutation: "503"
	// (the default, a bare 503), "429" (a bare 429) or "close" (the
	// connection closed with no answer, a transport error). A transient
	// answer is not recorded in Calls.
	TransientFirst map[string]int
	TransientAs    map[string]string
	transientSent  map[string]int
	// NoUsageTools refuses a `usage` report carrying `tools` (the
	// per-tool calls) with "Unknown field", as a platform older than
	// tools-show-their-use whose UsageReportInput has only the total.
	NoUsageTools bool
	// NoValidationUsage refuses a `usage` report carrying `validation`
	// (per-step validation minutes) with "Unknown field", as a platform
	// older than node-attests-its-validation whose UsageReportInput has
	// no such field.
	NoValidationUsage bool
	// NoContextUsage refuses a `usage` report carrying `contextBytes`
	// with "Unknown field", as a platform older than
	// context-budget-that-holds (adr-016).
	NoContextUsage bool
	// NoDocumentsFetched refuses a `usage` report carrying
	// `documentsFetched` with "Unknown field", as a platform older than
	// context-is-fetched-not-shipped (adr-016's amendment 2026-10-02).
	NoDocumentsFetched bool
	// NoToolNames refuses a `usage` report carrying `toolNames` with
	// "Unknown field", as a platform older than
	// context-is-fetched-not-shipped (adr-016's amendment 2026-10-02).
	NoToolNames bool
	// NoToolError refuses a usage report whose `tools` entry carries
	// `error`, and introspection answers the entry without it, as a
	// platform older than the tool entry's error text.
	NoToolError bool
	// NoUnclaimedUntil is a platform whose holding.unclaimed element
	// type has no `until` (adr-005's amendment 2026-10-03, goal
	// no-headroom-is-a-hold-not-a-claim-loop, item 4): introspection
	// answers the element type without it, so a heartbeat carrying
	// Holding.Unclaimed[].Until sends the entry without `until`.
	NoUnclaimedUntil bool
	// NoHoldingOrgs is a platform whose holding input has no `orgs`
	// (mesh-cli ADR-023, Reporting): introspection answers the holding
	// type without it, so a heartbeat's holding goes without `orgs`.
	NoHoldingOrgs bool
	// NoHoldingKinds is a platform whose holding input has no `kinds`
	// (ADR-025, item 6): introspection answers the holding type without
	// it, so a heartbeat's holding goes without `kinds`.
	NoHoldingKinds bool
	// NoAdoptedFields is a platform whose `adopted` element type has
	// none of goalId, directionId, sessionId and alive (mesh-cli ADR-009
	// amendment): introspection answers the element type without them,
	// so a heartbeat carrying an AdoptedSession with any of the four set
	// sends the entry without them, itemId, kind and since alone.
	NoAdoptedFields bool
	// NoScopeShares is a platform whose registerNode/heartbeatNode
	// payload's `scopes` element type has no `shares` (goal
	// every-org-gets-its-share-of-a-node, item 3): introspection on
	// DispatchResult answers the scopes element type without it, so the
	// node asks for the scopes selection without `shares` and every
	// scope answers Shares 0.
	NoScopeShares bool
	// RefuseReleases answers every releaseWorkItem with a refusal and
	// changes nothing, as a platform that will not take a node's release.
	// By default the fake follows the contract (trd §1): a node releases
	// a claimed or running item it holds.
	RefuseReleases bool
	// RebaseTokens is the platform half ADR-001 asks for and runsheet
	// does not serve yet: issueItemRepoToken to the node holding an
	// item's rebase while it is in review. Off, the fake refuses it as
	// runsheet's gateway/itemtoken.go does today.
	RebaseTokens bool
	// PairedFinish is the platform half of pairing (mesh-cli ADR-023,
	// Pairing) the hosted paired field will bring: a built finish of an
	// item already in evaluating, on the branch it was finished on,
	// records the new head — the pair finishing after its holder — and
	// leaves it evaluating. Off, the fake refuses it as the platform does
	// today ("item is evaluating, not running").
	PairedFinish bool
	// HostedWorkTools is the platform's /mcp serving the hosted work
	// tools (mesh-cli ADR-023, Hosted path): tools/list names mesh_claim,
	// mesh_progress, mesh_ask, mesh_finish and mesh_release beside the
	// context tools, and tools/call applies each as its mutation does
	// (mcp.go). Off, /mcp serves the context tools alone.
	HostedWorkTools bool
	// MCPHits counts /mcp requests.
	MCPHits int
	// Claims is every claimWorkItem the fake was sent, refused or not
	// (Calls holds only the ones it took): whether a node asked.
	Claims []Call
	// EmptyHeads are head shas that carry nothing past their base: a
	// built finish on one takes runsheet's empty-finish route
	// (empties-go-back-to-the-builder) — back to ready, attempts+1,
	// its node and claim cleared — rather than to evaluating.
	EmptyHeads map[string]bool
	// SessionCredentials maps a session's subject — its item, else its
	// goal, else its direction — to the rows the claim-time read
	// (itemCredentials, planCredentials, researchCredentials) hands the
	// node, in the platform's shape: id, label, vendor, kind
	// (subscription | api_key | oauth_token), serves, dailyCap,
	// monthlyCap, capUnit, secret, priorities, windowTargets. priorities
	// is a JSON list of urgent/high/normal/low (absent or empty serves
	// every priority); windowTargets is a list of {window, percent} —
	// window five_hour or seven_day (fiveHour/sevenDay also read),
	// percent 0..100 (adr-005's amendment 2026-10-02, a credential names
	// the priorities it serves): a row hands both as
	// `"priorities": []any{"urgent", "high"}, "windowTargets":
	// []any{map[string]any{"window": "five_hour", "percent": 50.0}}`.
	// A subject with none gets an empty
	// list. Unsupported[<field>] refuses the field, as a platform
	// without that read.
	SessionCredentials map[string][]map[string]any
	// DefaultSessionCredentials are the rows the claim-time read hands a
	// subject SessionCredentials has no entry for (an entry, even an
	// empty one, is that subject's alone): a node only runs sessions on
	// issued credentials, so a test fixture issues one by default.
	DefaultSessionCredentials []map[string]any
	// SessionTools maps a session's subject, as SessionCredentials does,
	// to the rows itemTools (planTools, researchTools) hands it: id,
	// slug, name, kind (remote_mcp | local_mcp | builtin), need, url,
	// command, args, runtime, builtIn, account, headers and env as
	// [{name, value} | {name, source}]. A subject with none gets an
	// empty list; UnknownFields[<field>] refuses a read selecting it, as
	// a platform older than tools-in-the-console.
	SessionTools map[string][]map[string]any
	// SessionUsage is every `usage` report the fake was handed on
	// FinishWorkItem, FinishWorkItemAlreadyDone, EvaluateWorkItem,
	// PlanGoal, FinishWorkItemRebase and RecordDirectionResearch, in
	// order: the mutation name and the report as a map. A command
	// carrying no usage (nil) adds no row.
	SessionUsage []Call
	// ToolUsage is every recordToolUsage the fake was handed, in order:
	// aggregateId, tool, toolCallId and usage.
	ToolUsage []map[string]any
	// After, when set, runs once a mutation has applied (under the lock,
	// on the fake's rows) — for a test whose platform reacts to it, as a
	// goal ending with its last item.
	After func(name string, in map[string]any)
	// Scorecard maps an org id to the Map the platform's scorecard(orgId)
	// answers (runsheet trd §6a). An org with none answers {"projects":
	// []}; Unsupported["scorecard"] refuses the field, as a platform
	// older than it.
	Scorecard map[string]map[string]any
	// Answer is the NodeAnswer registerNode and heartbeatNode hand back
	// on their payload beside status — the platform's answer to who owns
	// the node and which orgs it now serves (runsheet goal
	// a-node-has-an-owner). nil answers `status` alone, as a platform
	// before that goal does.
	Answer *gateway.NodeAnswer
	// AnswerJSON, when set, is served verbatim as registerNode's and
	// heartbeatNode's answer (nodeId, owner, scopes) in Answer's place —
	// a contract fixture's own bytes, not the fake's re-encoding of them.
	AnswerJSON json.RawMessage
	// OwnerFieldLegacy is a platform before runsheet goal
	// registernode-answers-with-the-node-it-made, which spelled
	// RegisterNodeInput's owner kind `owner`: introspection lists
	// `owner` in place of `ownerKind`, and a registerNode carrying the
	// other spelling is refused with "Unknown field".
	OwnerFieldLegacy bool
	// TakesNodeKind says whether the fake's RegisterNodeInput has `kind`
	// (gateway.NodeKindField), the node's own kind — "personal" for
	// `mesh mcp`'s node (ADR-015's owner form, ADR-023 mesh mcp). Unset
	// (the default) is a platform before that field: a registerNode
	// carrying `kind` is refused with "Unknown field".
	TakesNodeKind bool
	// TakesNodeCredential says whether the fake's RegisterNodeInput has
	// `credentialId` (gateway.NodeCredentialField), the Latchkey machine
	// id of the node's own credential. Unset (the default) is a
	// platform before runsheet#2290: introspection does not list it and
	// a registerNode carrying it is refused with "Unknown field". When
	// taken, the node row records it as sent.
	TakesNodeCredential bool
	// NodeCredentialID, when set, makes the fake's bearer (Token) a
	// machine credential whose machine id it is, modelling the machine
	// branch of runsheet gateway/scope_node.go's registersNode: a
	// registerNode on that bearer whose input's `credentialId` is
	// absent or differs from it — and a heartbeatNode for a row whose
	// recorded credentialId differs from it — is refused
	// `<mutation>: not yours`, recorded in Calls as the platform saw it.
	// Item bearers are not machines and are never refused for it.
	NodeCredentialID string
	// Enterprises by id: the enterprises the platform knows, each with
	// its admins' user ids. myEnterprises serves those the caller
	// (UserID) admins, and a registerNode with owner kind "enterprise"
	// is refused unless its ownerId is one of them (the platform's
	// adminOnEnterprise(ownerId)).
	Enterprises map[string]Enterprise
	// RegistersAt is the id the platform derives for the node's owner
	// row (ids.NodeOwnerID). When set, a registerNode under any other
	// aggregateId is refused with the platform's own wording, `node
	// <slug> registers at <RegistersAt>, the id its form names, not
	// <sent>` (platform/aggregates/node.go), and recorded in Calls as
	// the platform saw it; one under RegistersAt registers.
	RegistersAt string
	// PlanClaimTTL is how long a plan claim keeps another node off the
	// goal, as runsheet goal.go's PlanClaimTTL: claimPlan by another node
	// is refused while the holder's planClaimedAt is within it (or the
	// claim has no planClaimedAt) and takes the claim once it has passed.
	// The holder may always claim again, and its planClaimedAt is kept.
	// Zero is DefaultPlanClaimTTL.
	PlanClaimTTL time.Duration
	// Now is the fake's clock for planClaimedAt and the TTL; nil is
	// time.Now.
	Now func() time.Time

	requests int
}

// DefaultPlanClaimTTL is the platform's PlanClaimTTL (runsheet goal.go).
const DefaultPlanClaimTTL = 30 * time.Minute

// PlanClaimTTLInForce is PlanClaimTTL, or DefaultPlanClaimTTL when unset.
func (f *Fake) PlanClaimTTLInForce() time.Duration {
	if f.PlanClaimTTL > 0 {
		return f.PlanClaimTTL
	}
	return DefaultPlanClaimTTL
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Requests is how many GraphQL requests the fake has been sent, refused
// ones included.
func (f *Fake) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// Enterprise is one of the fake's enterprises.
type Enterprise struct {
	Slug        string
	DisplayName string
	Admins      []string
}

// New starts a fake accepting the given bearer token.
func New(token string) *Fake {
	f := &Fake{
		Token:     token,
		UserID:    "11111111-1111-4111-8111-111111111111",
		WorkItems: map[string]map[string]any{}, Goals: map[string]map[string]any{}, Projects: map[string]map[string]any{},
		Directions: map[string]map[string]any{},
		Repos:      map[string]map[string]any{}, Accounts: map[string]map[string]any{}, Nodes: map[string]map[string]any{},
		Documents: map[string][]map[string]any{}, Bodies: map[string]string{}, Refuse: map[string]string{}, FailReads: map[string]string{},
		GitHubTokens: map[string]map[string]any{}, Unsupported: map[string]bool{},
		PlanGitHubTokens: map[string][]map[string]any{}, ResearchGitHubTokens: map[string][]map[string]any{},
		Agents: map[string]map[string]any{}, UnknownFields: map[string]bool{},
		SessionCredentials: map[string][]map[string]any{}, SessionTools: map[string][]map[string]any{},
		Scorecard: map[string]map[string]any{}, Enterprises: map[string]Enterprise{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", f.graphql)
	mux.HandleFunc("/mesh/v1/", f.mesh)
	mux.HandleFunc("/prologue/v1/", f.prologue)
	mux.HandleFunc("/mcp", f.mcp)
	f.Server = httptest.NewServer(mux)
	return f
}

// Close stops the server.
func (f *Fake) Close() { f.Server.Close() }

// URL is the fake's base URL.
func (f *Fake) URL() string { return f.Server.URL }

// CallsNamed returns the recorded mutations with that name.
// PlanRequest is one requestPlan the fake was sent: the goal, who asked
// and the item a narrow re-plan replaces ("" for a whole one).
type PlanRequest struct {
	GoalID, By, ReplaceItem string
}

// PlanRelease is one releaseGoalPlan the fake was sent.
type PlanRelease struct {
	GoalID, NodeID, Reason string
}

// RequestPlans returns every requestPlan the fake was sent, refused ones
// included.
func (f *Fake) RequestPlans() []PlanRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PlanRequest(nil), f.PlanRequests...)
}

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

// ReadsNamed returns the recorded index lists with that name.
func (f *Fake) ReadsNamed(name string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.Reads {
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

// Goal is a copy of the goal's row, read under the fake's lock.
func (f *Fake) Goal(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyRow(f.Goals[id])
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
	mutationRe = regexp.MustCompile(`mutation\s*\(\s*\$(?:in|id):\s*(\w+)!\s*\)\s*\{\s*(\w+)\s*\(`)
	whereRe    = regexp.MustCompile(`\{\s*field:\s*"([a-z_]+)"\s*,\s*op:\s*EQ\s*,\s*value:\s*"([^"]*)"\s*\}`)
	listRe     = regexp.MustCompile(`\{\s*(\w+Indexs)\s*\(`)
	// the claim-time read: query($id: UUID!) { <x>Credentials(<arg>: $id) [<x>Tools(<arg>: $id)] }
	sessionReadRe = regexp.MustCompile(`\{\s*(\w+Credentials)\s*\(\s*(\w+):\s*\$id\s*\)(?:\s*(\w+Tools)\s*\(\s*\w+:\s*\$id\s*\))?\s*\}`)
	getRe         = regexp.MustCompile(`\{\s*(\w+Index)\s*\(\s*namespace:\s*"runsheet"\s*,\s*id:\s*"([^"]+)"`)
	scorecardRe   = regexp.MustCompile(`\{\s*scorecard\s*\(\s*orgId:\s*"([^"]+)"\s*\)\s*\}`)
	// the retry's read of an item's recorded evaluations
	// (WorkItemPriorAttempts): workItemIndex { evaluations { attempt
	// notes failedClauses { clause satisfy } } }. evaluations is a list
	// of EvaluationRecord, not a flat scalar, so it is not one of
	// WorkItemIndex's generated fields (fields/WorkItemIndex.txt holds
	// only fields a bare selection can name) — the fake answers this
	// query specially instead, as it already does for scorecard and the
	// claim-time credentials read.
	priorAttemptsRe = regexp.MustCompile(`\{\s*workItemIndex\s*\(\s*namespace:\s*"runsheet"\s*,\s*id:\s*"([^"]+)"\s*\)\s*\{\s*evaluations\s*\{`)
	// a plan's or a direction's own GitHub tokens (adr-022):
	// query($id: UUID!) { planGitHubToken(goalId: $id) } /
	// researchGitHubToken(directionId: $id)
	githubTokenRe = regexp.MustCompile(`\{\s*(planGitHubToken|researchGitHubToken)\s*\(\s*(\w+):\s*\$id\s*\)\s*\}`)
)

func (f *Fake) graphql(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer != f.Token && !slices.Contains(f.ItemBearers, bearer) {
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
	f.requests++
	switch {
	case strings.HasPrefix(q, "mutation { signIn"):
		writeData(w, map[string]any{"signIn": map[string]any{"id": f.UserID}})
	case strings.Contains(q, `__type(name: "UsageReportInput")`):
		f.Reads = append(f.Reads, Call{Name: "__type UsageReportInput", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		writeData(w, map[string]any{"__type": f.usageReportInputType()})
	case strings.Contains(q, "__type(") && req.Variables["t"] == "EvaluateWorkItemInput":
		f.Reads = append(f.Reads, Call{Name: "__type EvaluateWorkItemInput", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		var fields []any
		for _, n := range []string{"aggregateId", "namespace", "passed", "notes", "costCents", "usage", gateway.NoVerdictField, gateway.NoVerdictReasonField, gateway.FailedClausesField, gateway.ActorField} {
			if (n == gateway.NoVerdictField || n == gateway.NoVerdictReasonField) && f.NoNoVerdict {
				continue
			}
			if n == gateway.ActorField && f.NoActor {
				continue
			}
			if n == gateway.FailedClausesField && f.NoFailedClauses {
				continue
			}
			fields = append(fields, map[string]any{"name": n})
		}
		writeData(w, map[string]any{"__type": map[string]any{"inputFields": fields}})
	case strings.Contains(q, "__type(") && req.Variables["t"] == "PlanGoalInput":
		f.Reads = append(f.Reads, Call{Name: "__type PlanGoalInput", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		fields := []any{}
		for _, n := range []string{"aggregateId", "by", "costCents", "items", "namespace", "usage", gateway.PlanQuestionsField, gateway.ActorField} {
			if n == gateway.PlanQuestionsField && !f.PlanQuestions {
				continue
			}
			if n == gateway.ActorField && f.NoActor {
				continue
			}
			fields = append(fields, map[string]any{"name": n})
		}
		writeData(w, map[string]any{"__type": map[string]any{"inputFields": fields}})
	case strings.Contains(q, "__type(") && req.Variables["t"] == "PlannedItemInput":
		f.Reads = append(f.Reads, Call{Name: "__type PlannedItemInput", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		fields := []any{}
		for _, n := range []string{"acceptance", "agentSlug", "baseRef", "brief", "dependsOn", "model", "replacesDependents", "repoId", "seq", "title", gateway.ItemPreconditionField} {
			if n == gateway.ItemPreconditionField && !f.ItemPrecondition {
				continue
			}
			fields = append(fields, map[string]any{"name": n})
		}
		writeData(w, map[string]any{"__type": map[string]any{"inputFields": fields}})
	case strings.Contains(q, "__type(") && actorInputFields[fmt.Sprint(req.Variables["t"])] != nil:
		t := fmt.Sprint(req.Variables["t"])
		f.Reads = append(f.Reads, Call{Name: "__type " + t, Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		fields := []any{}
		for _, n := range append(actorInputFields[t], gateway.ActorField) {
			if n == gateway.ActorField && f.NoActor {
				continue
			}
			if n == gateway.DescriptionField && f.NoDescription {
				continue
			}
			fields = append(fields, map[string]any{"name": n})
		}
		writeData(w, map[string]any{"__type": map[string]any{"inputFields": fields}})
	case strings.Contains(q, "__type(") && req.Variables["t"] == "RequestPlanInput":
		f.Reads = append(f.Reads, Call{Name: "__type RequestPlanInput", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		fields := []any{}
		for _, n := range []string{"aggregateId", "by", "namespace", gateway.ReplaceItemField} {
			if n == gateway.ReplaceItemField && f.NoReplaceItem {
				continue
			}
			fields = append(fields, map[string]any{"name": n})
		}
		writeData(w, map[string]any{"__type": map[string]any{"inputFields": fields}})
	case strings.Contains(q, `__type(name: "RegisterNodeInput")`),
		strings.Contains(q, "__type(") && req.Variables["t"] == "RegisterNodeInput":
		f.Reads = append(f.Reads, Call{Name: "__type RegisterNodeInput", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		writeData(w, map[string]any{"__type": f.registerNodeInputType()})
	case strings.Contains(q, `__type(name: "Mutation")`):
		f.Reads = append(f.Reads, Call{Name: "__type Mutation", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		writeData(w, map[string]any{"__type": f.mutationType()})
	case strings.Contains(q, `__type(name: "DispatchResult")`):
		f.Reads = append(f.Reads, Call{Name: "__type DispatchResult", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		writeData(w, map[string]any{"__type": f.dispatchResultType()})
	case strings.HasPrefix(q, "mutation") && strings.Contains(q, "recordValidationAttestation("):
		f.recordValidationAttestation(w, q, req.Variables, bearer)
	case strings.Contains(q, "__type("):
		f.Reads = append(f.Reads, Call{Name: "__type", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		writeData(w, map[string]any{"__type": f.heartbeatInputType()})
	case strings.Contains(q, "myEnterprises"):
		if msg := f.FailReads["myEnterprises"]; msg != "" {
			writeErrors(w, 200, msg)
			return
		}
		writeData(w, map[string]any{"myEnterprises": f.myEnterprises()})
	case strings.Contains(q, "myOrgs"):
		if msg := f.FailReads["myOrgs"]; msg != "" {
			writeErrors(w, 200, msg)
			return
		}
		writeData(w, map[string]any{"myOrgs": f.Orgs})
	case strings.HasPrefix(q, "mutation"):
		m := mutationRe.FindStringSubmatch(q)
		if m == nil {
			writeErrors(w, 200, "unknown mutation")
			return
		}
		in, _ := req.Variables["in"].(map[string]any)
		// the two old usage mutations are refused as the platform refuses
		// them, and the attempt recorded, so CallsNamed finds any caller
		switch m[2] {
		case "recordSessionUsage":
			// never a field of the platform's schema: a session's usage
			// rides the command that ends it (`usage`)
			f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now(), Bearer: bearer})
			writeErrors(w, 200, `Cannot query field "recordSessionUsage" on type "Mutation".`)
			return
		case "recordUsage":
			// RegisterPlatformOnly on runsheet: the platform's own, never a
			// node's (priced tool calls go to recordToolUsage)
			f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now(), Bearer: bearer})
			writeErrors(w, 200, "recordUsage: permission denied: the field is the platform's own, not a node's")
			return
		}
		if f.Unsupported[m[2]] {
			writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type \"Mutation\".", m[2]))
			return
		}
		if m[2] == "proposeGoalSplit" && !f.GoalSplit {
			// goal a-goal-fits-in-eight-items is still queued: the
			// platform's Mutation has no proposeGoalSplit yet, the same
			// refusal an absent recordValidationAttestation gets
			writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type \"Mutation\".", m[2]))
			return
		}
		if t, ok := actorMutations[m[2]]; ok && f.NoActor {
			if _, ok := in[gateway.ActorField]; ok {
				writeErrors(w, 200, fmt.Sprintf(`Variable "$in" got invalid value; Unknown field "actor" is not defined by type %q.`, t))
				return
			}
		}
		if m[2] == "finishWorkItem" && f.NoDescription {
			if _, ok := in[gateway.DescriptionField]; ok {
				writeErrors(w, 200, `Variable "$in" got invalid value; Unknown field "description" is not defined by type "FinishWorkItemInput".`)
				return
			}
		}
		if m[2] == "requestPlan" {
			by, _ := in["by"].(string)
			replace, _ := in[gateway.ReplaceItemField].(string)
			goal, _ := in["aggregateId"].(string)
			f.PlanRequests = append(f.PlanRequests, PlanRequest{GoalID: goal, By: by, ReplaceItem: replace})
			if _, ok := in[gateway.ReplaceItemField]; ok && f.NoReplaceItem {
				writeErrors(w, 200, `Variable "$in" got invalid value; Unknown field "replaceItem" is not defined by type "RequestPlanInput".`)
				return
			}
		}
		if _, ok := in[gateway.PlanQuestionsField]; ok && m[2] == "planGoal" && !f.PlanQuestions {
			writeErrors(w, 200, `Variable "$in" got invalid value; Unknown field "questions" is not defined by type "PlanGoalInput".`)
			return
		}
		if m[2] == "planGoal" && !f.ItemPrecondition {
			items, _ := in["items"].([]any)
			for _, it := range items {
				if row, _ := it.(map[string]any); row != nil {
					if _, ok := row[gateway.ItemPreconditionField]; ok {
						writeErrors(w, 200, `Variable "$in" got invalid value; Unknown field "precondition" is not defined by type "PlannedItemInput".`)
						return
					}
				}
			}
		}
		if m[2] == "evaluateWorkItem" {
			// the platform's input, as runsheet ADR 035 defines it:
			// `passed` is required, and `outcome` was never a field
			if _, ok := in["outcome"]; ok {
				writeErrors(w, 200, `Variable "$in" got invalid value; Unknown field "outcome" is not defined by type "EvaluateWorkItemInput".`)
				return
			}
			if _, ok := in["passed"]; !ok {
				writeErrors(w, 200, `Variable "$in" got invalid value; Field "passed" of required type "Boolean!" was not provided.`)
				return
			}
			if _, ok := in[gateway.NoVerdictField]; ok && f.NoNoVerdict {
				writeErrors(w, 200, `Variable "$in" got invalid value; Unknown field "noVerdict" is not defined by type "EvaluateWorkItemInput".`)
				return
			}
		}
		// contextBytes is refused before the older fields, the order a
		// fallback that took them one by one got wrong.
		if usage, ok := in["usage"].(map[string]any); ok && f.NoContextUsage {
			if _, ok := usage["contextBytes"]; ok {
				writeErrors(w, 200, `Variable "$in" got invalid value; In field "usage": Unknown field "contextBytes" is not defined by type "UsageReportInput".`)
				return
			}
		}
		if usage, ok := in["usage"].(map[string]any); ok && f.NoDocumentsFetched {
			if _, ok := usage["documentsFetched"]; ok {
				writeErrors(w, 200, `Variable "$in" got invalid value; In field "usage": Unknown field "documentsFetched" is not defined by type "UsageReportInput".`)
				return
			}
		}
		if usage, ok := in["usage"].(map[string]any); ok && f.NoToolNames {
			if _, ok := usage["toolNames"]; ok {
				writeErrors(w, 200, `Variable "$in" got invalid value; In field "usage": Unknown field "toolNames" is not defined by type "UsageReportInput".`)
				return
			}
		}
		if usage, ok := in["usage"].(map[string]any); ok && f.NoUsageTools {
			if _, ok := usage["tools"]; ok {
				writeErrors(w, 200, `Variable "$in" got invalid value; In field "usage": In field "tools": Unknown field "tools" is not defined by type "UsageReportInput".`)
				return
			}
		}
		if usage, ok := in["usage"].(map[string]any); ok && f.NoValidationUsage {
			if _, ok := usage["validation"]; ok {
				writeErrors(w, 200, `Variable "$in" got invalid value; In field "usage": Unknown field "validation" is not defined by type "UsageReportInput".`)
				return
			}
		}
		if usage, ok := in["usage"].(map[string]any); ok && f.NoToolError {
			rows, _ := usage["tools"].([]any)
			for _, r := range rows {
				if row, _ := r.(map[string]any); row != nil {
					if _, ok := row["error"]; ok {
						writeErrors(w, 200, `Variable "$in" got invalid value; In field "usage": In field "tools": In element #0: Unknown field "error" is not defined by type "ToolCallReportInput".`)
						return
					}
				}
			}
		}
		if field := f.unknownInput(in); field != "" {
			typ := strings.ToUpper(m[2][:1]) + m[2][1:] + "Input"
			writeErrors(w, 200, fmt.Sprintf(`Variable "$in" got invalid value; In field %q: is not defined by type %q.`, field, typ))
			return
		}
		if f.UnknownFields["nodeAnswer"] && (m[2] == "registerNode" || m[2] == "heartbeatNode") && strings.Contains(q, "nodeId") {
			// a platform before a-node-has-an-owner: its CommandResult has
			// status alone, and the whole call is refused before it runs
			writeErrors(w, 200, `Cannot query field "nodeId" on type "CommandResult".`)
			return
		}
		if m[2] == "claimWorkItem" || m[2] == "claimWorkItemEvaluation" {
			f.Claims = append(f.Claims, Call{Name: m[2], Input: in, At: time.Now(), Bearer: bearer})
		}
		if msg := f.Refuse[m[2]]; msg != "" {
			writeErrors(w, 200, msg)
			return
		}
		if m[2] == "issueItemRepoToken" {
			// the platform's shape: issueItemRepoToken(workItemId: UUID!): Map
			// — an input object is the schema error production answered
			if strings.Contains(q, "IssueItemRepoTokenInput") {
				writeErrors(w, 200, `Variable "$in" cannot be non-input type "IssueItemRepoTokenInput!".`)
				return
			}
			id, _ := req.Variables["id"].(string)
			in = map[string]any{"workItemId": id}
			tok, err := f.issueItemRepoToken(in)
			if err != nil {
				writeErrors(w, 200, err.Error())
				return
			}
			f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now(), Bearer: bearer})
			writeData(w, map[string]any{m[2]: tok})
			return
		}
		if id, _ := in["aggregateId"].(string); m[2] == "registerNode" && f.RegistersAt != "" && id != f.RegistersAt {
			slug, _ := in["slug"].(string)
			f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now(), Bearer: bearer})
			writeErrors(w, 200, fmt.Sprintf("node %s registers at %s, the id its form names, not %s", slug, f.RegistersAt, id))
			return
		}
		if msg := f.notYours(m[2], in, bearer); msg != "" {
			f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now(), Bearer: bearer, Query: q})
			writeErrors(w, 200, msg)
			return
		}
		if err := f.apply(m[2], in); err != nil {
			writeErrors(w, 200, err.Error())
			return
		}
		if f.After != nil {
			f.After(m[2], in)
		}
		f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now(), Bearer: bearer, Query: q})
		if usage, ok := in["usage"].(map[string]any); ok && finishingMutations[m[2]] {
			f.SessionUsage = append(f.SessionUsage, Call{Name: m[2], Input: copyRow(usage), At: time.Now()})
		}
		writeData(w, map[string]any{m[2]: f.mutationPayload(m[2], q)})
	default:
		if m := scorecardRe.FindStringSubmatch(q); m != nil {
			f.Reads = append(f.Reads, Call{Name: "scorecard", Input: map[string]any{"orgId": m[1]}})
			if f.Unsupported["scorecard"] {
				writeErrors(w, 200, `Cannot query field "scorecard" on type "Query".`)
				return
			}
			sc, ok := f.Scorecard[m[1]]
			if !ok {
				sc = map[string]any{"projects": []any{}}
			}
			writeData(w, map[string]any{"scorecard": sc})
			return
		}
		if m := sessionReadRe.FindStringSubmatch(q); m != nil {
			f.sessionRead(w, m[1], m[2], m[3], req.Variables)
			return
		}
		if m := githubTokenRe.FindStringSubmatch(q); m != nil {
			f.githubTokens(w, m[1], m[2], req.Variables)
			return
		}
		if m := priorAttemptsRe.FindStringSubmatch(q); m != nil {
			f.Reads = append(f.Reads, Call{Name: "workItemIndex", Input: map[string]any{"id": m[1]}})
			if f.NoEvaluations {
				writeErrors(w, 200, `Cannot query field "evaluations" on type "WorkItemIndex".`)
				return
			}
			item := f.WorkItems[m[1]]
			if item == nil {
				writeData(w, map[string]any{"workItemIndex": nil})
				return
			}
			writeData(w, map[string]any{"workItemIndex": map[string]any{"evaluations": item["evaluations"]}})
			return
		}
		field, typ := f.unknownSelection(q)
		if m := getRe.FindStringSubmatch(q); m != nil {
			f.Reads = append(f.Reads, Call{Name: m[1], Input: map[string]any{"id": m[2]}})
			if field != "" {
				writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type %q.", field, typ))
				return
			}
			if msg := f.FailReads[m[1]]; msg != "" {
				writeErrors(w, 200, msg)
				return
			}
			writeData(w, map[string]any{m[1]: f.known(m[1], f.table(m[1] + "s")[m[2]])})
			return
		}
		m := listRe.FindStringSubmatch(q)
		if m == nil {
			writeErrors(w, 200, "unknown query")
			return
		}
		wheres := whereRe.FindAllStringSubmatch(q, -1)
		read := Call{Name: m[1], Input: map[string]any{}}
		for _, wm := range wheres {
			read.Input[wm[1]] = wm[2]
		}
		f.Reads = append(f.Reads, read)
		if field != "" {
			writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type %q.", field, typ))
			return
		}
		if msg := f.FailReads[m[1]]; msg != "" {
			writeErrors(w, 200, msg)
			return
		}
		for _, wm := range wheres {
			if msg := f.FailReads[m[1]+"/"+wm[1]+"="+wm[2]]; msg != "" {
				writeErrors(w, 200, msg)
				return
			}
		}
		rows := []map[string]any{}
		for _, row := range f.table(m[1]) {
			ok := true
			for _, wm := range wheres {
				if fmt.Sprint(row[camel(wm[1])]) != wm[2] {
					ok = false
				}
			}
			if ok {
				rows = append(rows, f.known(typeOf(m[1]), row))
			}
		}
		writeData(w, map[string]any{m[1]: rows})
	}
}

// sessionRead answers the claim-time read of one session subject: the
// credentials field and, when selected, the tools field, each recorded
// as a call named by the field with the argument it was given.
func (f *Fake) sessionRead(w http.ResponseWriter, credField, arg, toolsField string, vars map[string]any) {
	id, _ := vars["id"].(string)
	in := map[string]any{arg: id}
	if f.Unsupported[credField] {
		writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type \"Query\".", credField))
		return
	}
	if toolsField != "" && (f.UnknownFields[toolsField] || f.Unsupported[toolsField]) {
		writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type \"Query\".", toolsField))
		return
	}
	if msg := f.Refuse[credField]; msg != "" {
		writeErrors(w, 200, msg)
		return
	}
	creds := []map[string]any{}
	rows, ok := f.SessionCredentials[id]
	if !ok {
		rows = f.DefaultSessionCredentials
	}
	for _, c := range rows {
		creds = append(creds, copyRow(c))
	}
	data := map[string]any{credField: creds}
	f.Calls = append(f.Calls, Call{Name: credField, Input: in, At: time.Now()})
	if toolsField != "" {
		rows := []map[string]any{}
		for _, t := range f.SessionTools[id] {
			rows = append(rows, copyRow(t))
		}
		data[toolsField] = rows
		f.Calls = append(f.Calls, Call{Name: toolsField, Input: in, At: time.Now()})
	}
	writeData(w, data)
}

// githubTokens answers planGitHubToken(goalId)/researchGitHubToken(directionId)
// (adr-022): the schema's "Cannot query field" when Unsupported names
// the field — an older platform, the node's cue to fall back to the
// operator's gh — a named refusal from Refuse, a refusal naming the
// node when TokenNodeID is set and holds neither the goal's plan claim
// nor the direction's research claim, else one row per repo enrolled
// in the project; a project repo with no row among them is a refusal
// naming it, "no GitHub installation reaches <owner/name>", rather than
// a partial list.
func (f *Fake) githubTokens(w http.ResponseWriter, field, arg string, vars map[string]any) {
	id, _ := vars["id"].(string)
	in := map[string]any{arg: id}
	if f.Unsupported[field] {
		writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type \"Query\".", field))
		return
	}
	if msg := f.Refuse[field]; msg != "" {
		writeErrors(w, 200, msg)
		return
	}
	var projectID string
	var rows []map[string]any
	switch field {
	case "planGitHubToken":
		goal := f.Goals[id]
		if goal == nil {
			writeErrors(w, 200, "no such goal")
			return
		}
		if held, _ := goal["plannerNodeId"].(string); f.TokenNodeID != "" && held != f.TokenNodeID {
			writeErrors(w, 200, fmt.Sprintf("the goal is not node %s's to plan", f.TokenNodeID))
			return
		}
		projectID, _ = goal["projectId"].(string)
		rows = f.PlanGitHubTokens[id]
	case "researchGitHubToken":
		direction := f.Directions[id]
		if direction == nil {
			writeErrors(w, 200, "no such direction")
			return
		}
		if held, _ := direction["researchNodeId"].(string); f.TokenNodeID != "" && held != f.TokenNodeID {
			writeErrors(w, 200, fmt.Sprintf("the direction is not node %s's to research", f.TokenNodeID))
			return
		}
		projectID, _ = direction["projectId"].(string)
		rows = f.ResearchGitHubTokens[id]
	}
	have := map[string]bool{}
	for _, row := range rows {
		if name, _ := row["repository"].(string); name != "" {
			have[name] = true
		}
	}
	for _, name := range f.projectRepoNames(projectID) {
		if !have[name] {
			writeErrors(w, 200, fmt.Sprintf("no GitHub installation reaches %s", name))
			return
		}
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, copyRow(row))
	}
	f.Calls = append(f.Calls, Call{Name: field, Input: in, At: time.Now()})
	writeData(w, map[string]any{field: out})
}

// projectRepoNames is a project's repos as owner/name, in repoIds
// order — the same composition ProjectRepos reads client-side, done
// here server-side for githubTokens' missing-installation check.
func (f *Fake) projectRepoNames(projectID string) []string {
	project := f.Projects[projectID]
	if project == nil {
		return nil
	}
	var ids []string
	switch v := project["repoIds"].(type) {
	case []string:
		ids = v
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				ids = append(ids, s)
			}
		}
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, f.repoFullName(id))
	}
	return names
}

// repoFullName is one repo's owner/name, its owner its own account's
// login (never the org's) — a repo or account with no row resolves to
// whatever it has, so a missing fixture still names something.
func (f *Fake) repoFullName(repoID string) string {
	repo := f.Repos[repoID]
	if repo == nil {
		return repoID
	}
	name, _ := repo["name"].(string)
	if acc := f.Accounts[fmt.Sprint(repo["accountId"])]; acc != nil {
		if login, _ := acc["login"].(string); login != "" {
			return login + "/" + name
		}
	}
	return name
}

// heartbeatInputType is HeartbeatNodeInput as introspection answers it:
// every field a heartbeat may carry, `commit` among them, less
// UnknownFields — `scaling`
// without `accounts` when that is unknown, as the platform before
// adr-005's per-account slots, and without `host` when
// UnknownFields["host"] (a platform before adr-004's amendment
// 2026-10-03); `holding` left out entirely when
// UnknownFields["holding"] (a platform before node-says-why-work-waits);
// `limits`'s element type (NodeAccountLimitsInput) without `priorities`
// when UnknownFields["priorities"] (a platform before adr-005's
// amendment 2026-10, `limits[].priorities`), and without `target`,
// `distance` or `push` when UnknownFields names each one (a platform
// before adr-005's amendment 2026-10-03, `limits[].target`, `distance`,
// `push`); `holding.unclaimed`'s
// element type without `until` when NoUnclaimedUntil (a platform
// before adr-005's amendment 2026-10-03, `holding.unclaimed[].until`);
// `adopted`'s element type without goalId, directionId, sessionId and
// alive when NoAdoptedFields (a platform before mesh-cli ADR-009's
// amendment). Unsupported["__type"] refuses the probe.
func (f *Fake) heartbeatInputType() map[string]any {
	field := func(name string) map[string]any {
		return map[string]any{"name": name}
	}
	var fields []any
	for _, name := range []string{"aggregateId", "namespace", "version", "running", "draining", "updateError", "limits", "scaling", "runtimes", "adapters", "holding", "commit", "adopted"} {
		if f.UnknownFields[name] {
			continue
		}
		row := map[string]any{"name": name, "type": map[string]any{"kind": "SCALAR"}}
		if name == "adopted" {
			var sub []any
			for _, n := range []string{"itemId", "kind", "since", "goalId", "directionId", "sessionId", "alive"} {
				switch n {
				case "itemId", "kind", "since":
					sub = append(sub, field(n))
				default:
					if !f.NoAdoptedFields {
						sub = append(sub, field(n))
					}
				}
			}
			row["type"] = map[string]any{"kind": "LIST", "ofType": map[string]any{"kind": "NON_NULL", "ofType": map[string]any{"kind": "INPUT_OBJECT", "inputFields": sub}}}
		}
		if name == "scaling" {
			var sub []any
			for _, n := range []string{"capacity", "min", "max", "reason", "accounts", "host"} {
				if !f.UnknownFields[n] {
					sub = append(sub, field(n))
				}
			}
			row["type"] = map[string]any{"kind": "INPUT_OBJECT", "inputFields": sub}
		}
		if name == "holding" {
			var sub []any
			for _, n := range []string{"mode", "summary", "until", "accounts", "unclaimed", "orgs", "kinds"} {
				if n == "orgs" && f.NoHoldingOrgs {
					continue
				}
				if n == "kinds" && f.NoHoldingKinds {
					continue
				}
				if n == "unclaimed" {
					var usub []any
					for _, un := range []string{"goalId", "kind", "count", "reason", "until"} {
						if un == "until" && f.NoUnclaimedUntil {
							continue
						}
						usub = append(usub, field(un))
					}
					sub = append(sub, map[string]any{"name": n, "type": map[string]any{
						"kind": "LIST", "ofType": map[string]any{
							"kind": "NON_NULL", "ofType": map[string]any{"kind": "INPUT_OBJECT", "inputFields": usub},
						},
					}})
					continue
				}
				sub = append(sub, field(n))
			}
			row["type"] = map[string]any{"kind": "INPUT_OBJECT", "inputFields": sub}
		}
		if name == "limits" {
			var sub []any
			for _, n := range []string{"account", "status", "rateLimitType", "resetsAt", "overageStatus", "isUsingOverage", "fiveHour", "sevenDay", "observedAt", "priorities", "target", "distance", "push"} {
				if !f.UnknownFields[n] {
					sub = append(sub, field(n))
				}
			}
			row["type"] = map[string]any{"kind": "LIST", "ofType": map[string]any{"kind": "NON_NULL", "ofType": map[string]any{"kind": "INPUT_OBJECT", "inputFields": sub}}}
		}
		fields = append(fields, row)
	}
	return map[string]any{"inputFields": fields}
}

// notYours is the machine branch of the platform's registersNode and
// heartbeat rules (runsheet gateway/scope_node.go) when NodeCredentialID
// is set: the fake's Token is that machine's bearer, a registerNode on
// it must name it as credentialId, and a heartbeatNode is refused for a
// row whose recorded credentialId is another's. "" lets the call run.
func (f *Fake) notYours(name string, in map[string]any, bearer string) string {
	if f.NodeCredentialID == "" || bearer != f.Token {
		return ""
	}
	switch name {
	case "registerNode":
		if sent, _ := in[gateway.NodeCredentialField].(string); sent != f.NodeCredentialID {
			return "registerNode: not yours"
		}
	case "heartbeatNode":
		id, _ := in["aggregateId"].(string)
		if row := f.Nodes[id]; row != nil {
			if held, _ := row[gateway.NodeCredentialField].(string); held != "" && held != f.NodeCredentialID {
				return "heartbeatNode: not yours"
			}
		}
	}
	return ""
}

// registerNodeInputType is RegisterNodeInput as introspection answers
// it: every field a registration may carry, the owner kind among them
// as `ownerKind` — `owner` when OwnerFieldLegacy — unless UnknownFields
// names that spelling (UnknownFields["owner"] and
// UnknownFields["ownerKind"] both set is a platform before
// the-node-serves-its-owners-orgs). Unsupported["__type"] refuses the
// probe.
func (f *Fake) registerNodeInputType() map[string]any {
	names := []string{"aggregateId", "namespace", "orgId", "ownerId", "name", "slug", "harnesses", "capacity", "version", f.ownerField()}
	if f.TakesNodeKind {
		names = append(names, gateway.NodeKindField)
	}
	if f.TakesNodeCredential {
		names = append(names, gateway.NodeCredentialField)
	}
	var fields []any
	for _, name := range names {
		if f.UnknownFields[name] {
			continue
		}
		fields = append(fields, map[string]any{"name": name, "type": map[string]any{"kind": "SCALAR"}})
	}
	return map[string]any{"inputFields": fields}
}

// dispatchResultType is DispatchResult as introspection answers it:
// the shared mutation payload type registerNode and heartbeatNode
// return their hand-selected fields on, its `scopes` field a list of
// an element type carrying orgId, login, slug and accountId, plus
// `shares` unless NoScopeShares (a platform before
// every-org-gets-its-share-of-a-node item 3). Unsupported["__type"]
// refuses the probe.
func (f *Fake) dispatchResultType() map[string]any {
	names := []string{"orgId", "login", "slug", "accountId"}
	if !f.NoScopeShares {
		names = append(names, "shares")
	}
	var scopeFields []any
	for _, n := range names {
		scopeFields = append(scopeFields, map[string]any{"name": n})
	}
	scopesType := map[string]any{
		"kind": "LIST",
		"ofType": map[string]any{
			"kind": "NON_NULL",
			"ofType": map[string]any{
				"kind":   "OBJECT",
				"fields": scopeFields,
			},
		},
	}
	return map[string]any{"fields": []any{
		map[string]any{"name": "scopes", "type": scopesType},
	}}
}

// ownerField is the spelling of RegisterNodeInput's owner kind the
// fake's schema has (before UnknownFields hides it).
func (f *Fake) ownerField() string {
	if f.OwnerFieldLegacy {
		return "owner"
	}
	return "ownerKind"
}

// myEnterprises is the enterprises the caller (UserID) admins, as
// myEnterprises' Map rows.
func (f *Fake) myEnterprises() []map[string]any {
	ids := make([]string, 0, len(f.Enterprises))
	for id := range f.Enterprises {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := []map[string]any{}
	for _, id := range ids {
		e := f.Enterprises[id]
		if !slices.Contains(e.Admins, f.UserID) {
			continue
		}
		rows = append(rows, map[string]any{"id": id, "slug": e.Slug, "displayName": e.DisplayName, "tenantSlug": e.Slug, "status": "active"})
	}
	return rows
}

// usageReportInputType is UsageReportInput as introspection answers
// it: `tools` a non-null list of ToolCallReportInput, whose entry has
// `error` unless NoToolError; no `tools` at all when NoUsageTools; no
// `validation` at all when NoValidationUsage.
func (f *Fake) usageReportInputType() map[string]any {
	fields := []any{}
	for _, name := range []string{"sessionId", "adapter", "model", "credentialId", "credentialKind", "inputTokens", "outputTokens", "toolCalls", "wallMs"} {
		fields = append(fields, map[string]any{"name": name, "type": map[string]any{"kind": "SCALAR"}})
	}
	if !f.NoValidationUsage {
		fields = append(fields, map[string]any{"name": "validation", "type": map[string]any{"kind": "SCALAR"}})
	}
	if !f.NoUsageTools {
		var sub []any
		for _, n := range []string{"tool", "calls", "errors", "ms", "error"} {
			if n == "error" && f.NoToolError {
				continue
			}
			sub = append(sub, map[string]any{"name": n})
		}
		fields = append(fields, map[string]any{"name": "tools", "type": map[string]any{
			"kind": "LIST", "ofType": map[string]any{
				"kind": "NON_NULL", "ofType": map[string]any{"kind": "INPUT_OBJECT", "inputFields": sub}}}})
	}
	return map[string]any{"inputFields": fields}
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
	case "agentIndexs":
		return f.Agents
	case "credentialIndexs":
		return byID(f.Credentials)
	case "orgIndexs":
		return byID(f.Orgs)
	}
	return map[string]map[string]any{}
}

// byID keys rows held as a list by their id, as table answers.
func byID(rows []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(rows))
	for i, row := range rows {
		id := fmt.Sprint(row["id"])
		if row["id"] == nil {
			id = fmt.Sprint(i)
		}
		out[id] = row
	}
	return out
}

// protectedFields are the three fields NoProtectedFields refuses at
// once (goal one-evaluator-for-ordinary-work).
var protectedFields = []string{"protectedFiles", "protectedRulePaths", "protectedPaths"}

// fieldUnknown is whether the fake's schema refuses field, either named
// in UnknownFields or one of protectedFields while NoProtectedFields.
func (f *Fake) fieldUnknown(field string) bool {
	if f.UnknownFields[field] {
		return true
	}
	return f.NoProtectedFields && slices.Contains(protectedFields, field)
}

// unknownSelection is the first field of the query's selections the
// fake's schema does not have, and the type it was asked of.
func (f *Fake) unknownSelection(q string) (string, string) {
	typ := "Query"
	if m := listRe.FindStringSubmatch(q); m != nil {
		typ = strings.TrimSuffix(m[1], "s")
	} else if m := getRe.FindStringSubmatch(q); m != nil {
		typ = m[1]
	}
	typ = strings.ToUpper(typ[:1]) + typ[1:]
	i := strings.LastIndex(q, ")")
	if i < 0 {
		return "", ""
	}
	for _, word := range strings.FieldsFunc(q[i:], func(r rune) bool { return r == ' ' || r == '{' || r == '}' || r == ')' }) {
		if f.fieldUnknown(word) || !selectable(typ, word) {
			return word, typ
		}
	}
	return "", ""
}

// typeOf is the schema type of an index list or get (workItemIndexs,
// workItemIndex → WorkItemIndex).
func typeOf(name string) string {
	name = strings.TrimSuffix(name, "s")
	return strings.ToUpper(name[:1]) + name[1:]
}

// selectable is whether the real gateway serves the field on the type:
// any field of an entity without a generated list, else one of its
// generated required or optional fields.
func selectable(typ, field string) bool {
	req, opt := gateway.Fields(typ), gateway.OptionalFields(typ)
	if req == nil && opt == nil {
		return true
	}
	return slices.Contains(req, field) || slices.Contains(opt, field)
}

// known is the row of a type as the fake's schema serves it: every
// generated field of the entity, nil where the row sets none, and
// without the fields UnknownFields says it does not have.
func (f *Fake) known(typ string, row map[string]any) map[string]any {
	if row == nil {
		return row
	}
	typ = typeOf(typ)
	out := copyRow(row)
	for _, names := range [][]string{gateway.Fields(typ), gateway.OptionalFields(typ)} {
		for _, k := range names {
			if _, ok := out[k]; !ok {
				out[k] = nil
			}
		}
	}
	for k := range f.UnknownFields {
		delete(out, k)
	}
	if f.NoProtectedFields {
		for _, k := range protectedFields {
			delete(out, k)
		}
	}
	return out
}

// unknownInput is the first key of a command's input, or of a row of a
// list in it, the fake's schema does not have.
func (f *Fake) unknownInput(in map[string]any) string {
	for k, v := range in {
		if f.UnknownFields[k] {
			return k
		}
		if row, ok := v.(map[string]any); ok {
			if k := f.unknownInput(row); k != "" {
				return k
			}
		}
		if rows, ok := v.([]any); ok {
			for _, r := range rows {
				if row, ok := r.(map[string]any); ok {
					for rk := range row {
						if f.UnknownFields[rk] {
							return rk
						}
					}
				}
			}
		}
	}
	return ""
}

// finishingMutations are the commands whose input may carry a `usage`
// report (runsheet trd §6a): the ones that end a session.
var finishingMutations = map[string]bool{
	"finishWorkItem": true, "evaluateWorkItem": true, "planGoal": true,
	"finishWorkItemRebase": true, "recordDirectionResearch": true,
}

// mutationPayload is what the fake answers a mutation's payload with:
// {"status": "ok"} for any command, plus f.Answer's nodeId, owner and
// scopes on registerNode and heartbeatNode (runsheet goal
// a-node-has-an-owner) — nil leaves the payload at status alone, as a
// platform before that goal does.
//
// Only what the query selects is answered, as GraphQL does: a payload
// asked for `status` alone gets status alone.
func (f *Fake) mutationPayload(name, q string) map[string]any {
	payload := map[string]any{"status": "ok"}
	if name != "registerNode" && name != "heartbeatNode" {
		return payload
	}
	selected := func(field string) bool {
		return strings.Contains(q, " "+field+" ") || strings.Contains(q, " "+field+"{")
	}
	if len(f.AnswerJSON) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(f.AnswerJSON, &raw); err == nil {
			for k, v := range raw {
				if selected(k) {
					payload[k] = v
				}
			}
		}
		return payload
	}
	if f.Answer == nil || !selected("nodeId") {
		return payload
	}
	scopes := make([]map[string]any, len(f.Answer.Scopes))
	for i, s := range f.Answer.Scopes {
		scopes[i] = map[string]any{"orgId": s.OrgID, "login": s.Login, "slug": s.Slug, "accountId": s.AccountID}
		if selected("shares") {
			scopes[i]["shares"] = s.Shares
		}
	}
	payload["nodeId"] = f.Answer.NodeID
	payload["owner"] = map[string]any{"kind": f.Answer.Owner.Kind, "id": f.Answer.Owner.ID, "name": f.Answer.Owner.Name}
	payload["scopes"] = scopes
	return payload
}

func (f *Fake) apply(name string, in map[string]any) error {
	id, _ := in["aggregateId"].(string)
	item := f.WorkItems[id]
	switch name {
	case "recordToolUsage":
		f.ToolUsage = append(f.ToolUsage, copyRow(in))
		return nil
	case "claimWorkItem":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		if msg, ok := f.ClaimRefusals[id]; ok {
			return fmt.Errorf("%s", msg)
		}
		if item["status"] == "claimed" && item["nodeId"] == in["nodeId"] {
			return nil
		}
		if item["status"] != "ready" {
			return fmt.Errorf("item is %s, not ready", item["status"])
		}
		item["status"], item["nodeId"], item["harness"] = "claimed", in["nodeId"], in["harness"]
	case "claimWorkItemEvaluation":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		if item["status"] != "evaluating" {
			return fmt.Errorf("item is %s, not evaluating", item["status"])
		}
		// a node that did not build the item takes its evaluation
		// (goal a-node-says-which-sessions-it-takes): the holder is
		// tracked apart from `nodeId`, which still names the builder
		// until this claim succeeds, then the evaluator — the same
		// field issueItemRepoToken reads as "its builder, then its
		// evaluator"
		if holder, _ := item["evaluationNodeId"].(string); holder != "" && holder != in["nodeId"] {
			return fmt.Errorf("node %s has claimed the evaluation", holder)
		}
		item["evaluationNodeId"], item["nodeId"] = in["nodeId"], in["nodeId"]
	case "reportWorkItemProgress":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		// a claimed rebase's session reports on the item in review: the
		// rebase's heartbeat (runsheet ADR 008), its cents on the item
		rebasing := item["status"] == "review" && item["rebaseState"] == "claimed"
		// an evaluation's session reports on the item in evaluating — an
		// adopted one's "session adopted by the restarted node" — and the
		// status is left where it is, as the platform leaves it
		evaluating := item["status"] == "evaluating"
		if item["status"] != "claimed" && item["status"] != "running" && !rebasing && !evaluating {
			return fmt.Errorf("item is %s, not running", item["status"])
		}
		switch {
		case rebasing:
			item["rebaseHeartbeats"] = toInt(item["rebaseHeartbeats"]) + 1
		case !evaluating:
			item["status"] = "running"
		}
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
		outcome, _ := in["outcome"].(string)
		if d, ok := in[gateway.DescriptionField]; ok {
			item[gateway.DescriptionField] = d
		}
		if f.PairedFinish && item["status"] == "evaluating" && (outcome == "" || outcome == "built") && in["branch"] != nil && in["branch"] == item["branch"] {
			item["headSha"] = in["headSha"]
			item["costCents"] = toInt(item["costCents"]) + toInt(in["costCents"])
			return nil
		}
		if item["status"] != "claimed" && item["status"] != "running" {
			return fmt.Errorf("item is %s, not running", item["status"])
		}
		switch {
		case outcome == "" || outcome == "built":
			if in["branch"] == nil || in["branch"] == "" {
				return fmt.Errorf("a built finish needs a branch")
			}
			if sha, _ := in["headSha"].(string); f.EmptyHeads[sha] {
				item["status"], item["nodeId"], item["attempts"] = "ready", nil, toInt(item["attempts"])+1
				item["lastNote"] = "empty finish: no commits past the base"
				item["costCents"] = toInt(item["costCents"]) + toInt(in["costCents"])
				return nil
			}
			item["branch"], item["headSha"], item["outcome"] = in["branch"], in["headSha"], "built"
		case outcome == "already_done":
			if ev, _ := in["evidence"].(string); ev == "" {
				return fmt.Errorf("an already_done finish needs evidence")
			}
			if in["branch"] != nil {
				return fmt.Errorf("an already_done finish carries no branch")
			}
			item["outcome"], item["evidence"] = outcome, in["evidence"]
		default:
			return fmt.Errorf("unknown outcome %q", outcome)
		}
		item["status"] = "evaluating"
		item["costCents"] = toInt(item["costCents"]) + toInt(in["costCents"])
	case "failWorkItem":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		item["status"], item["lastError"] = "failed", in["error"]
	case "releaseWorkItem":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		return f.release(item, in)
	case "evaluateWorkItem":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		if m, ok := in[gateway.EvaluatedModelField]; ok {
			item["evaluatedModel"] = m
		}
		// an evaluation that reached no verdict costs the item nothing:
		// it stays in evaluating, its reason kept (runsheet ADR 035) — and
		// it is refused with `passed: true` or a blank reason
		if in[gateway.NoVerdictField] == true {
			if in["passed"] == true {
				return fmt.Errorf("evaluateWorkItem: a no-verdict evaluation cannot pass")
			}
			if r, _ := in[gateway.NoVerdictReasonField].(string); strings.TrimSpace(r) == "" {
				return fmt.Errorf("evaluateWorkItem: a no-verdict evaluation needs a reason")
			}
			item["noVerdictReason"] = in[gateway.NoVerdictReasonField]
			item["noVerdicts"] = toInt(item["noVerdicts"]) + 1
			item["costCents"] = toInt(item["costCents"]) + toInt(in["costCents"])
			return nil
		}
		// the attempt this verdict records, before this fail's own
		// increment below (so the first evaluation is attempt 1)
		attempt := toInt(item["attempts"]) + 1
		// a confirmed already_done item is merged with no pull request
		// (mesh ADR 003 decision 1); a built one goes to review, where
		// the platform opens its pull request
		if in["passed"] == true && item["outcome"] == "already_done" {
			item["status"] = "merged"
		} else if in["passed"] == true {
			item["status"] = "review"
		} else {
			// a verdict that did not pass is an attempt (mesh-specs trd)
			item["status"], item["attempts"] = "ready", toInt(item["attempts"])+1
		}
		item["evaluation"] = in["notes"]
		// the clauses a failed verdict named, for the retry (nil when the
		// node sent none)
		clauses, _ := in[gateway.FailedClausesField].([]any)
		if clauses == nil {
			clauses = []any{}
		}
		if cl, ok := in[gateway.FailedClausesField]; ok {
			item["failedClauses"] = cl
		}
		evals, _ := item["evaluations"].([]any)
		item["evaluations"] = append(evals, map[string]any{
			"attempt":       attempt,
			"notes":         in["notes"],
			"failedClauses": clauses,
		})
		item["costCents"] = toInt(item["costCents"]) + toInt(in["costCents"])
	case "claimWorkItemRebase", "finishWorkItemRebase", "failWorkItemRebase":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		return applyRebase(name, item, in)
	case "registerNode":
		// the owner kind under the spelling the schema does not have is
		// refused as the platform refuses an unknown input field
		for _, k := range []string{"owner", "ownerKind"} {
			if _, ok := in[k]; ok && (k != f.ownerField() || f.UnknownFields[k]) {
				return fmt.Errorf(`Variable "$in" got invalid value; Unknown field %q is not defined by type "RegisterNodeInput".`, k)
			}
		}
		if _, ok := in[gateway.NodeKindField]; ok && (!f.TakesNodeKind || f.UnknownFields[gateway.NodeKindField]) {
			return fmt.Errorf(`Variable "$in" got invalid value; Unknown field %q is not defined by type "RegisterNodeInput".`, gateway.NodeKindField)
		}
		if _, ok := in[gateway.NodeCredentialField]; ok && (!f.TakesNodeCredential || f.UnknownFields[gateway.NodeCredentialField]) {
			return fmt.Errorf(`Variable "$in" got invalid value; Unknown field %q is not defined by type "RegisterNodeInput".`, gateway.NodeCredentialField)
		}
		kind, _ := in[f.ownerField()].(string)
		if kind == "enterprise" {
			ownerID, _ := in["ownerId"].(string)
			if e, ok := f.Enterprises[ownerID]; !ok || !slices.Contains(e.Admins, f.UserID) {
				return fmt.Errorf("registerNode: permission denied: adminOnEnterprise(%s): the caller is not an admin of the enterprise", ownerID)
			}
		}
		row := map[string]any{"id": id, "status": "online", "running": 0}
		for k, v := range in {
			row[k] = v
		}
		if kind != "" {
			row["ownerKind"] = kind
		}
		f.Nodes[id] = row
	case "retireNode":
		if f.Nodes[id] == nil {
			return fmt.Errorf("no such node")
		}
		f.Nodes[id]["status"] = "retired"
	case "heartbeatNode":
		if f.Nodes[id] == nil {
			return fmt.Errorf("node not registered")
		}
		f.Nodes[id]["running"] = in["running"]
		f.Nodes[id]["version"] = in["version"]
		// what the node found on the machine this heartbeat; a heartbeat
		// without them leaves the last ones, as the platform does
		for _, k := range []string{"runtimes", "adapters", "sandbox", "reason"} {
			if v, ok := in[k]; ok {
				f.Nodes[id][k] = v
			}
		}
		// an empty status is the ordinary heartbeat: online again
		if status, _ := in["status"].(string); status != "" {
			f.Nodes[id]["status"] = status
		} else {
			f.Nodes[id]["status"] = "online"
		}
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
		direction["pitches"] = in["pitches"]
		direction["answers"] = in["answers"]
		direction["researchCostCents"] = toInt(in["costCents"])
		direction["researchNotes"] = in["notes"]
	case "pauseGoal", "resumeGoal", "cancelGoal", "approveGraph", "planGoal", "requestPlan", "claimPlan", "releasePlan", "setGoalCheckpoints", "proposeGoalSplit":
		if f.Goals[id] == nil {
			return fmt.Errorf("no such goal")
		}
		switch name {
		case "setGoalCheckpoints":
			f.Goals[id]["checkpoints"] = in["checkpoints"]
		case "proposeGoalSplit":
			f.Goals[id]["splitDrafts"] = in["drafts"]
			f.Goals[id]["splitCostCents"] = toInt(in["costCents"])
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
			if qs, ok := in["questions"]; ok {
				f.Goals[id]["questions"] = qs
			}
		case "requestPlan":
			f.Goals[id]["status"] = "planning"
			if r, _ := in[gateway.ReplaceItemField].(string); r != "" {
				f.Goals[id]["replacingItemId"] = r
			}
		case "claimPlan":
			// runsheet goal.go: the holder re-claims at will and keeps
			// its planClaimedAt; another node only once PlanClaimTTL has
			// passed since it, when the claim is its, freshly stamped
			held, _ := f.Goals[id]["plannerNodeId"].(string)
			if held != "" && held == in["nodeId"] {
				if at, _ := f.Goals[id]["planClaimedAt"].(string); at != "" {
					break
				}
			} else if held != "" {
				at, err := time.Parse(time.RFC3339, fmt.Sprint(f.Goals[id]["planClaimedAt"]))
				if err != nil || f.now().Sub(at) < f.PlanClaimTTLInForce() {
					return fmt.Errorf("another node is planning it")
				}
			}
			f.Goals[id]["plannerNodeId"] = in["nodeId"]
			f.Goals[id]["planClaimedAt"] = f.now().UTC().Format(time.RFC3339)
		case "releasePlan":
			// adr-022, A fetch that fails is reported: the claim is
			// cleared, the goal stays in planning, and the note is its
			// planError. nodeId is required, as on ReleaseWorkItemInput,
			// and only the holder may release.
			node, _ := in["nodeId"].(string)
			if node == "" {
				return fmt.Errorf("releasePlan: nodeId is required")
			}
			if held, _ := f.Goals[id]["plannerNodeId"].(string); held != "" && held != node {
				return fmt.Errorf("the plan claim is not node %s's", node)
			}
			if f.Goals[id]["status"] != "planning" {
				return fmt.Errorf("goal is %s, not planning", f.Goals[id]["status"])
			}
			f.Goals[id]["plannerNodeId"], f.Goals[id]["planClaimedAt"] = nil, nil
			f.Goals[id]["planError"] = in["note"]
		}
	case "releaseGoalPlan":
		// a second identical (normalised) plan-check refusal: the node
		// gives the claim up with the refusal's text as the reason,
		// recorded for a test to find, and clears whatever claimPlan set
		// (goal a-refused-plan-is-released-not-replanned).
		if f.Goals[id] == nil {
			return fmt.Errorf("no such goal")
		}
		node, _ := in["nodeId"].(string)
		reason, _ := in["reason"].(string)
		f.PlanReleases = append(f.PlanReleases, PlanRelease{GoalID: id, NodeID: node, Reason: reason})
		f.Goals[id]["plannerNodeId"], f.Goals[id]["planClaimedAt"] = nil, nil
	}
	return nil
}

// release is ReleaseWorkItem of trd §1: only a claimed or running item,
// only by the node holding it — the caller's credential, TokenNodeID
// (empty skips the check) — back to ready with its node, harness and
// session cleared, attempts unchanged, the reason its last note and not
// its last error. Anything else is refused and changes nothing.
func (f *Fake) release(item, in map[string]any) error {
	if f.RefuseReleases {
		return fmt.Errorf("releaseWorkItem is refused")
	}
	if item["status"] != "claimed" && item["status"] != "running" {
		return fmt.Errorf("item is %s, not claimed or running", item["status"])
	}
	if f.TokenNodeID != "" && item["nodeId"] != f.TokenNodeID {
		return fmt.Errorf("the item is not node %s's", f.TokenNodeID)
	}
	// nodeId is NON_NULL on the platform's ReleaseWorkItemInput, and the
	// note is `note`, not `reason`: a release that names neither is
	// refused there, so it is refused here too — a caller that sends the
	// wrong shape fails in the tests instead of in production.
	if id, _ := in["nodeId"].(string); id == "" {
		return fmt.Errorf("releaseWorkItem: nodeId is required")
	}
	item["status"], item["nodeId"], item["harness"], item["sessionId"] = "ready", nil, nil, nil
	item["lastNote"] = in["note"]
	return nil
}

// applyRebase is the rebase handshake of runsheet ADR 008 / trd §6a on
// the item: requested → claimed by one node → rebased, or given up and
// handed to the builder as a conflict rework (ready, attempts as they
// were, the reason as its last error, the rebase over).
func applyRebase(name string, item, in map[string]any) error {
	state, _ := item["rebaseState"].(string)
	holder, _ := item["rebaseNodeId"].(string)
	switch name {
	case "claimWorkItemRebase":
		if state == "claimed" && holder == in["nodeId"] {
			return nil
		}
		if state == "claimed" {
			return fmt.Errorf("node %s has claimed the rebase", holder)
		}
		if state != "requested" {
			return fmt.Errorf("no rebase is requested (rebase_state %q)", state)
		}
		item["rebaseState"], item["rebaseNodeId"] = "claimed", in["nodeId"]
	case "finishWorkItemRebase":
		if state != "claimed" && state != "requested" {
			return fmt.Errorf("no rebase is under way (rebase_state %q)", state)
		}
		item["rebaseState"], item["rebaseMethod"] = "rebased", in["method"]
		item["rebasedHeadSha"], item["headSha"] = in["headSha"], in["headSha"]
		item["rebaseFiles"] = in["files"]
		item["rebases"] = toInt(item["rebases"]) + 1
	case "failWorkItemRebase":
		if state != "claimed" && state != "requested" {
			return nil // converges on a rebase not under way
		}
		item["rebaseConflict"] = in["reason"]
		item["status"], item["lastError"], item["rebaseState"] = "ready", in["reason"], "none"
	}
	return nil
}

// HeldBy marks the item running on nodeID: held by another node, as a
// pair finds it (mesh-cli ADR-023, Pairing).
func (f *Fake) HeldBy(itemID, nodeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if item := f.WorkItems[itemID]; item != nil {
		item["status"], item["nodeId"] = "running", nodeID
	}
}

func (f *Fake) issueItemRepoToken(in map[string]any) (map[string]any, error) {
	id, _ := in["workItemId"].(string)
	item := f.WorkItems[id]
	if item == nil {
		return nil, fmt.Errorf("no such item")
	}
	holder := item["nodeId"]
	switch {
	case item["status"] == "claimed", item["status"] == "running", item["status"] == "evaluating": // its builder, then its evaluator
	case f.RebaseTokens && item["status"] == "review" && item["rebaseState"] == "claimed": // the node holding its rebase
		holder = item["rebaseNodeId"]
	default:
		return nil, fmt.Errorf("item is %s, not claimed", item["status"])
	}
	if f.TokenNodeID != "" && holder != f.TokenNodeID {
		return nil, fmt.Errorf("the item is not node %s's", f.TokenNodeID)
	}
	tok := f.GitHubTokens[id]
	if tok == nil {
		return nil, fmt.Errorf("no GitHub installation reaches the item's repo")
	}
	return copyRow(tok), nil
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

// mesh serves mesh-context's document endpoints under /mesh/v1, the
// platform's live path.
func (f *Fake) mesh(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.MeshV1Hits++
	no := f.NoMeshV1
	f.mu.Unlock()
	if no {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f.context(w, r, "/mesh/v1/projects/")
}

// prologue serves the same document endpoints under /prologue/v1, kept
// as the dated alias (mesh-specs trd §6c) so a node on an older
// platform, or one falling back off a 404 from /mesh/v1, still reads.
func (f *Fake) prologue(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.PrologueV1Hits++
	f.mu.Unlock()
	f.context(w, r, "/prologue/v1/projects/")
}

func (f *Fake) context(w http.ResponseWriter, r *http.Request, prefix string) {
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// <prefix>{org}/{slug}/documents[/{doc}]
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
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

// mutationType is the Mutation type's introspection as the
// attestation probe reads it: recordValidationAttestation(workItemId,
// input), unless Unsupported names it; claimWorkItemEvaluation, unless
// Unsupported names it (ADR-025); proposeGoalSplit(input) too, when
// GoalSplit is set (ADR-023's gateway amendment).
func (f *Fake) mutationType() map[string]any {
	fields := []any{}
	if !f.Unsupported["recordValidationAttestation"] {
		fields = append(fields, map[string]any{
			"name": "recordValidationAttestation",
			"args": []any{map[string]any{"name": "workItemId"}, map[string]any{"name": "input"}},
		})
	}
	if !f.Unsupported["claimWorkItemEvaluation"] {
		fields = append(fields, map[string]any{
			"name": "claimWorkItemEvaluation",
			"args": []any{map[string]any{"name": "in"}},
		})
	}
	if f.GoalSplit && !f.Unsupported["proposeGoalSplit"] {
		fields = append(fields, map[string]any{
			"name": "proposeGoalSplit",
			"args": []any{map[string]any{"name": "input"}},
		})
	}
	return map[string]any{"fields": fields}
}

// recordValidationAttestation is the platform's
// recordValidationAttestation(workItemId: UUID!, input: Map!): Map
// (specs/contracts/recordValidationAttestation.json): an input-object
// shape is the schema error production answers, every call is recorded
// with workItemId and input, and success answers AttestationReceipt.
// RateLimit429 answers a bare 429 before any of that, every time, for a
// test of the retry-and-give-up path, and TransientFirst the first N
// calls transiently, for the retry-then-succeed path.
func (f *Fake) recordValidationAttestation(w http.ResponseWriter, q string, vars map[string]any, bearer string) {
	const field = "recordValidationAttestation"
	if f.RateLimit429[field] {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
		return
	}
	if f.answeredTransiently(w, field) {
		return
	}
	if f.Unsupported[field] {
		writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type \"Mutation\".", field))
		return
	}
	if !strings.Contains(q, "recordValidationAttestation(workItemId: $id, input: $in)") || strings.Contains(q, "RecordValidationAttestationInput") {
		writeErrors(w, 200, `Unknown type "RecordValidationAttestationInput".`)
		return
	}
	id, _ := vars["id"].(string)
	in, _ := vars["in"].(map[string]any)
	f.Calls = append(f.Calls, Call{Name: field, Input: map[string]any{"workItemId": id, "input": in}, At: time.Now(), Bearer: bearer})
	if msg := f.Refuse[field]; msg != "" {
		writeErrors(w, 200, msg)
		return
	}
	receipt := f.AttestationReceipt
	if receipt == nil {
		receipt = map[string]any{"status": "ok", "commit": in["commit"]}
	}
	writeData(w, map[string]any{field: receipt})
}

// answeredTransiently answers field's call transiently while fewer than
// TransientFirst[field] have been, as TransientAs[field] says, and
// reports whether it did.
func (f *Fake) answeredTransiently(w http.ResponseWriter, field string) bool {
	if f.transientSent[field] >= f.TransientFirst[field] {
		return false
	}
	if f.transientSent == nil {
		f.transientSent = map[string]int{}
	}
	f.transientSent[field]++
	switch f.TransientAs[field] {
	case "close":
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return true
			}
		}
		panic(http.ErrAbortHandler)
	case "429":
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("service unavailable"))
	}
	return true
}

// actorInputFields are the input fields, beside `actor`, the fake's
// introspection names for the command inputs that carry the actor and
// had no case of their own (every-actor-has-a-name).
var actorInputFields = map[string][]string{
	"ClaimWorkItemInput":          {"aggregateId", "namespace", "nodeId", "harness"},
	"ReportWorkItemProgressInput": {"aggregateId", "namespace", "note", "costCents", "sessionId", "usage"},
	"FinishWorkItemInput":         {"aggregateId", "namespace", "branch", "headSha", "outcome", "evidence", "description", "costCents", "usage"},
}

// actorMutations are the five commands that carry the actor, and the
// input type a NoActor platform names when refusing one.
var actorMutations = map[string]string{
	"claimWorkItem":          "ClaimWorkItemInput",
	"reportWorkItemProgress": "ReportWorkItemProgressInput",
	"finishWorkItem":         "FinishWorkItemInput",
	"evaluateWorkItem":       "EvaluateWorkItemInput",
	"planGoal":               "PlanGoalInput",
}
