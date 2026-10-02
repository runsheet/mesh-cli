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
	// monthlyCap, capUnit, secret. A subject with none gets an empty
	// list. Unsupported[<field>] refuses the field, as a platform
	// without that read.
	SessionCredentials map[string][]map[string]any
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

	requests int
}

// Requests is how many GraphQL requests the fake has been sent, refused
// ones included.
func (f *Fake) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
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
		Agents: map[string]map[string]any{}, UnknownFields: map[string]bool{},
		SessionCredentials: map[string][]map[string]any{}, SessionTools: map[string][]map[string]any{},
		Scorecard: map[string]map[string]any{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", f.graphql)
	mux.HandleFunc("/mesh/v1/", f.mesh)
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
		for _, n := range []string{"aggregateId", "namespace", "passed", "notes", "costCents", "usage", gateway.NoVerdictField, gateway.NoVerdictReasonField, gateway.FailedClausesField} {
			if (n == gateway.NoVerdictField || n == gateway.NoVerdictReasonField) && f.NoNoVerdict {
				continue
			}
			if n == gateway.FailedClausesField && f.NoFailedClauses {
				continue
			}
			fields = append(fields, map[string]any{"name": n})
		}
		writeData(w, map[string]any{"__type": map[string]any{"inputFields": fields}})
	case strings.Contains(q, `__type(name: "RegisterNodeInput")`):
		f.Reads = append(f.Reads, Call{Name: "__type RegisterNodeInput", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		writeData(w, map[string]any{"__type": f.registerNodeInputType()})
	case strings.Contains(q, "__type("):
		f.Reads = append(f.Reads, Call{Name: "__type", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		writeData(w, map[string]any{"__type": f.heartbeatInputType()})
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
		if err := f.apply(m[2], in); err != nil {
			writeErrors(w, 200, err.Error())
			return
		}
		if f.After != nil {
			f.After(m[2], in)
		}
		f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now(), Bearer: bearer})
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
	for _, c := range f.SessionCredentials[id] {
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

// heartbeatInputType is HeartbeatNodeInput as introspection answers it:
// every field a heartbeat may carry, `commit` among them, less
// UnknownFields — `scaling`
// without `accounts` when that is unknown, as the platform before
// adr-005's per-account slots; `holding` left out entirely when
// UnknownFields["holding"] (a platform before node-says-why-work-waits).
// Unsupported["__type"] refuses the probe.
func (f *Fake) heartbeatInputType() map[string]any {
	field := func(name string) map[string]any {
		return map[string]any{"name": name}
	}
	var fields []any
	for _, name := range []string{"aggregateId", "namespace", "version", "running", "draining", "updateError", "limits", "scaling", "runtimes", "adapters", "holding", "commit"} {
		if f.UnknownFields[name] {
			continue
		}
		row := map[string]any{"name": name, "type": map[string]any{"kind": "SCALAR"}}
		if name == "scaling" {
			var sub []any
			for _, n := range []string{"capacity", "min", "max", "reason", "accounts"} {
				if !f.UnknownFields[n] {
					sub = append(sub, field(n))
				}
			}
			row["type"] = map[string]any{"kind": "INPUT_OBJECT", "inputFields": sub}
		}
		if name == "holding" {
			var sub []any
			for _, n := range []string{"mode", "summary", "until", "accounts", "unclaimed"} {
				sub = append(sub, field(n))
			}
			row["type"] = map[string]any{"kind": "INPUT_OBJECT", "inputFields": sub}
		}
		fields = append(fields, row)
	}
	return map[string]any{"inputFields": fields}
}

// registerNodeInputType is RegisterNodeInput as introspection answers
// it: every field a registration may carry, `owner` among them unless
// UnknownFields["owner"] (a platform before
// the-node-serves-its-owners-orgs). Unsupported["__type"] refuses the
// probe.
func (f *Fake) registerNodeInputType() map[string]any {
	var fields []any
	for _, name := range []string{"aggregateId", "namespace", "orgId", "ownerId", "name", "slug", "harnesses", "capacity", "version", "owner"} {
		if f.UnknownFields[name] {
			continue
		}
		fields = append(fields, map[string]any{"name": name, "type": map[string]any{"kind": "SCALAR"}})
	}
	return map[string]any{"inputFields": fields}
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
	}
	return map[string]map[string]any{}
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
		if f.UnknownFields[word] || !selectable(typ, word) {
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
		// a claimed rebase's session reports on the item in review: the
		// rebase's heartbeat (runsheet ADR 008), its cents on the item
		rebasing := item["status"] == "review" && item["rebaseState"] == "claimed"
		if item["status"] != "claimed" && item["status"] != "running" && !rebasing {
			return fmt.Errorf("item is %s, not running", item["status"])
		}
		if rebasing {
			item["rebaseHeartbeats"] = toInt(item["rebaseHeartbeats"]) + 1
		} else {
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
		if item["status"] != "claimed" && item["status"] != "running" {
			return fmt.Errorf("item is %s, not running", item["status"])
		}
		outcome, _ := in["outcome"].(string)
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
		if cl, ok := in[gateway.FailedClausesField]; ok {
			item["failedClauses"] = cl
		}
		item["costCents"] = toInt(item["costCents"]) + toInt(in["costCents"])
	case "claimWorkItemRebase", "finishWorkItemRebase", "failWorkItemRebase":
		if item == nil {
			return fmt.Errorf("no such item")
		}
		return applyRebase(name, item, in)
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
