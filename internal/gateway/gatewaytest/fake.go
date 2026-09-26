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
	Calls     []Call
	// Reads are the index lists the fake served: Name is the list
	// (goalIndexs), Input its EQ wheres by field (org_id).
	Reads []Call
	// Refuse maps a mutation name to a refusal message (returned as a
	// GraphQL error) — for tests of "someone else has it".
	Refuse map[string]string
	// FailReads maps an index list name (goalIndexs) to an error it
	// answers with instead of rows.
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
	// SessionCredentials maps a session's subject — its workItemId, else
	// goalId, else directionId — to the credentials sessionCredentials
	// hands the node (id, label, kind, secret, capDailyUsd, …). A subject
	// with none gets an empty list.
	SessionCredentials map[string][]map[string]any
	// SessionTools maps a session's subject, as SessionCredentials does,
	// to the tools the claim-time read hands it (name, transport, url,
	// headers, runtime, command, args, env, secrets, required, roles,
	// pricePerCallCents). A subject with none gets no `tools` key at all;
	// UnknownFields["tools"] refuses a read selecting them, as a platform
	// older than tools-in-the-console.
	SessionTools map[string][]map[string]any
	// Usage are the priced tools' usage records the fake was handed
	// (recordUsage), in order: workItemId, kind, tool, calls,
	// listPriceCents.
	Usage []map[string]any
	// After, when set, runs once a mutation has applied (under the lock,
	// on the fake's rows) — for a test whose platform reacts to it, as a
	// goal ending with its last item.
	After func(name string, in map[string]any)
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
	case strings.Contains(q, "__type("):
		f.Reads = append(f.Reads, Call{Name: "__type", Input: map[string]any{}})
		if f.Unsupported["__type"] {
			writeErrors(w, 200, "GraphQL introspection is not allowed")
			return
		}
		writeData(w, map[string]any{"__type": f.heartbeatInputType()})
	case strings.Contains(q, "myOrgs"):
		writeData(w, map[string]any{"myOrgs": f.Orgs})
	case strings.HasPrefix(q, "mutation"):
		m := mutationRe.FindStringSubmatch(q)
		if m == nil {
			writeErrors(w, 200, "unknown mutation")
			return
		}
		in, _ := req.Variables["in"].(map[string]any)
		if f.Unsupported[m[2]] {
			writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type \"Mutation\".", m[2]))
			return
		}
		if field := f.unknownInput(in); field != "" {
			writeErrors(w, 200, fmt.Sprintf(`Variable "$in" got invalid value; In field %q: Unknown field.`, field))
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
			f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now()})
			writeData(w, map[string]any{m[2]: tok})
			return
		}
		if m[2] == "sessionCredentials" {
			subject := ""
			for _, k := range []string{"workItemId", "goalId", "directionId"} {
				if v, _ := in[k].(string); v != "" && subject == "" {
					subject = v
				}
			}
			selectsTools := strings.Contains(q, "tools {")
			if selectsTools && f.UnknownFields["tools"] {
				writeErrors(w, 200, `Cannot query field "tools" on type "SessionCredentialsResult".`)
				return
			}
			creds := []map[string]any{}
			for _, c := range f.SessionCredentials[subject] {
				creds = append(creds, copyRow(c))
			}
			res := map[string]any{"credentials": creds}
			if ts := f.SessionTools[subject]; selectsTools && ts != nil {
				var rows []map[string]any
				for _, t := range ts {
					rows = append(rows, copyRow(t))
				}
				res["tools"] = rows
			}
			f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now()})
			writeData(w, map[string]any{m[2]: res})
			return
		}
		if err := f.apply(m[2], in); err != nil {
			writeErrors(w, 200, err.Error())
			return
		}
		if f.After != nil {
			f.After(m[2], in)
		}
		f.Calls = append(f.Calls, Call{Name: m[2], Input: in, At: time.Now()})
		writeData(w, map[string]any{m[2]: map[string]any{"status": "ok"}})
	default:
		field, typ := f.unknownSelection(q)
		if m := getRe.FindStringSubmatch(q); m != nil {
			if field != "" {
				writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type %q.", field, typ))
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

// heartbeatInputType is HeartbeatNodeInput as introspection answers it:
// every field a heartbeat may carry, less UnknownFields — `scaling`
// without `accounts` when that is unknown, as the platform before
// adr-005's per-account slots. Unsupported["__type"] refuses the probe.
func (f *Fake) heartbeatInputType() map[string]any {
	field := func(name string) map[string]any {
		return map[string]any{"name": name}
	}
	var fields []any
	for _, name := range []string{"aggregateId", "namespace", "version", "running", "draining", "updateError", "limits", "scaling", "runtimes", "adapters"} {
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
		fields = append(fields, row)
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

func (f *Fake) apply(name string, in map[string]any) error {
	id, _ := in["aggregateId"].(string)
	item := f.WorkItems[id]
	switch name {
	case "recordUsage":
		f.Usage = append(f.Usage, copyRow(in))
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
		if in["passed"] == true {
			item["status"] = "review"
		} else {
			item["status"] = "ready"
		}
		item["evaluation"] = in["notes"]
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
	item["status"], item["nodeId"], item["harness"], item["sessionId"] = "ready", nil, nil, nil
	item["lastNote"] = in["reason"]
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
