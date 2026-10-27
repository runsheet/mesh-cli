package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"time"
)

// ThreadTurn is one thread turn the fake holds (runsheet ThreadSession):
// raised until a node claims it as Kind, answered by its finish.
type ThreadTurn struct {
	ID, Product, ThreadKey, OrgID, ProjectID, Role, Kind, Body, Ask string
	Status                                                          string
	NodeID                                                          string
	// Answer and Proposal are the finish's.
	Answer, Proposal string
	// Credentials are the model accounts jobAccess serves the turn, as
	// itemCredentials rows.
	Credentials []map[string]any
	// Guidance and FinishSchema are the kind's, as the brief carries them
	// (runsheet ADR 067): the guidance text and the product's schema for
	// the proposal, "" for none.
	Guidance, FinishSchema string
	// Harness and Finish are the kind's as the catalogue lists it and the
	// brief carries it: the harnesses it runs on ([claude-code, codex]
	// when nil) and its finish (answer-or-proposal when ""). A turn of a
	// kind the fake's catalogue does not list adds the kind to it, so a
	// node meets a kind it has never heard of (ADR-041 decision 5).
	Harness []string
	Finish  string
	// Finished is the whole finish the node handed in, as finishJob took
	// it: the result of a kind whose finish is not answer-or-proposal.
	Finished map[string]any
}

// turnKinds is each thread turn's kind as a catalogue row, by slug: the
// rows the kinds listing adds or overrides. f.mu is not held.
func (f *Fake) turnKinds() map[string]map[string]any {
	rows := map[string]map[string]any{}
	for _, t := range f.ThreadTurns {
		if t.Kind == "" {
			continue
		}
		harness, finish := t.Harness, t.Finish
		if harness == nil {
			harness = []string{"claude-code", "codex"}
		}
		if finish == "" {
			finish = "answer-or-proposal"
		}
		product, kind, _ := strings.Cut(t.Kind, "/")
		rows[t.Kind] = map[string]any{"slug": t.Kind, "product": product, "kind": kind, "harness": harness, "finish": finish,
			"access": []string{product, "model"}, "guidance": t.Guidance, "finishSchema": t.FinishSchema, "finishTool": kindFinishTools[t.Kind]}
	}
	return rows
}

// RaiseThreadTurn adds a raised turn.
func (f *Fake) RaiseThreadTurn(t ThreadTurn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ThreadTurns == nil {
		f.ThreadTurns = map[string]*ThreadTurn{}
	}
	if t.Status == "" {
		t.Status = "raised"
	}
	f.ThreadTurns[t.ID] = &t
}

// Turn is the turn of id as it stands, nil for none.
func (f *Fake) Turn(id string) *ThreadTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.ThreadTurns[id]; t != nil {
		c := *t
		return &c
	}
	return nil
}

// jobAPI answers the job system's calls (runsheet ADR 061 §3–§4):
// threadSessionIndexs, and claimJob, jobAccess and finishJob — on a
// thread turn itself, on runsheet's other kinds by serving the older
// call each routes to (the platform's routing, gateway claimjob.go and
// finishjob.go), so every older handler, its refusals and its Calls are
// what the node meets. f.mu is held. It reports whether q was one.
func (f *Fake) jobAPI(w http.ResponseWriter, r *http.Request, q string, vars map[string]any, bearer string) bool {
	in, _ := vars["in"].(map[string]any)
	subject, _ := in["subject"].(map[string]any)
	isThread := subject["type"] == "thread"
	f.downstream = nil
	defer func() { f.downstream = nil }()
	w = &downstreamWriter{ResponseWriter: w, f: f}
	switch {
	case strings.Contains(q, "threadSessionIndexs("):
		// The gateway's ThreadSessionIndex is camelCase; a selection of a
		// field it does not serve is refused as the gateway refuses it.
		if bad := unservedThreadField(q); bad != "" {
			writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type \"ThreadSessionIndex\".", bad))
			return true
		}
		rows := []map[string]any{}
		for _, t := range f.ThreadTurns {
			if t.Status != "raised" || !strings.Contains(q, fmt.Sprintf("%q", t.OrgID)) {
				continue
			}
			rows = append(rows, map[string]any{"id": t.ID, "product": t.Product, "threadKey": t.ThreadKey, "orgId": t.OrgID,
				"projectId": t.ProjectID, "role": t.Role, "status": t.Status})
		}
		writeData(w, map[string]any{"threadSessionIndexs": rows})
	case strings.Contains(q, "claimJob(input") && isThread:
		f.claimThreadJob(w, in)
	case strings.Contains(q, "jobAccess(input") && isThread:
		f.threadJobAccess(w, in)
	case strings.Contains(q, "finishJob(input") && isThread:
		f.finishThreadJob(w, in)
	case strings.Contains(q, "claimJob(input"):
		f.Calls = append(f.Calls, Call{Name: "claimJob", Input: in, At: time.Now(), Bearer: bearer})
		if err := jobHarness(in); err != nil {
			writeErrors(w, 200, err.Error())
			return true
		}
		field, args, err := jobClaimAlias(in)
		if err != nil {
			writeErrors(w, 200, err.Error())
			return true
		}
		if _, errs := f.call(r, mutationQuery(field), map[string]any{"in": args}, bearer); errs != nil {
			writeErrors(w, 200, errs[0])
			return true
		}
		kind, _ := in["kind"].(string)
		job := map[string]any{
			"kind": kind, "subject": subject, "code": map[string]any{"form": "commit"}, "claim": field,
			"finish": kindFinishes[kind], "guidance": f.KindGuidance[kind], "finishSchema": f.KindFinishSchema[kind], "finishTool": kindFinishTools[kind]}
		if g := f.KindGit[kind]; g != nil {
			job["git"] = g
		}
		writeData(w, map[string]any{"claimJob": map[string]any{"status": "claimed", "job": job}})
	case strings.Contains(q, "finishJob(input"):
		f.Calls = append(f.Calls, Call{Name: "finishJob", Input: in, At: time.Now(), Bearer: bearer})
		answers, err := f.finishJobAliases(r, in, bearer)
		if err != nil {
			writeErrors(w, 200, err.Error())
			return true
		}
		writeData(w, map[string]any{"finishJob": map[string]any{"status": "finished", "answers": answers}})
	case strings.Contains(q, "jobAccess(input"):
		f.Calls = append(f.Calls, Call{Name: "jobAccess", Input: in, At: time.Now(), Bearer: bearer})
		answer, err := f.jobAccessAliases(r, in, bearer)
		if err != nil {
			writeErrors(w, 200, err.Error())
			return true
		}
		writeData(w, map[string]any{"jobAccess": answer})
	default:
		return false
	}
	return true
}

// call serves q as a request of its own and answers its data, or its
// errors' messages.
func (f *Fake) call(r *http.Request, q string, vars map[string]any, bearer string) (map[string]any, []string) {
	rec := httptest.NewRecorder()
	f.serve(rec, r, q, vars, bearer)
	if rec.Code != http.StatusOK && f.downstream == nil {
		// a gateway down for the older call is down for the job call
		f.downstream = rec
		return nil, []string{fmt.Sprintf("fake: the older call answered %d", rec.Code)}
	}
	var out struct {
		Data   map[string]any `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return nil, []string{"fake: " + err.Error()}
	}
	if len(out.Errors) > 0 {
		msgs := make([]string, len(out.Errors))
		for i, e := range out.Errors {
			msgs[i] = e.Message
		}
		return nil, msgs
	}
	return out.Data, nil
}

// mutationQuery is an older command's query as the node's Mutate sends it.
func mutationQuery(field string) string {
	return fmt.Sprintf("mutation($in: %sInput!) { %s(input: $in) { status } }", strings.ToUpper(field[:1])+field[1:], field)
}

// kindFinishes is each runsheet kind's finish, as its catalogue row
// declares it (runsheet finishjob.go's routes).
// kindFinishTools are the product tools runsheet's kinds hand their
// result to (finish_tool, runsheet #5470).
var kindFinishTools = map[string]string{
	"runsheet/plan":     "mesh_plan",
	"runsheet/evaluate": "mesh_verdict",
}

var kindFinishes = map[string]string{
	"runsheet/build":    "changeset",
	"runsheet/evaluate": "verdict",
	"runsheet/rebase":   "rebased-head",
	"runsheet/plan":     "plan",
	"runsheet/research": "research",
	"runsheet/validate": "attestation",
}

// SetKindGuidance sets the guidance text and the finish_schema a kind's
// brief carries (runsheet ADR 067), "" for none.
func (f *Fake) SetKindGuidance(kind, guidance, finishSchema string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.KindGuidance == nil {
		f.KindGuidance = map[string]string{}
	}
	if f.KindFinishSchema == nil {
		f.KindFinishSchema = map[string]string{}
	}
	f.KindGuidance[kind], f.KindFinishSchema[kind] = guidance, finishSchema
}

// kindHarnesses are the harnesses a runsheet kind's catalogue row lists
// where the fake checks them (runsheet .mesh/jobs/*.yml): validate runs
// no agent, so its harness is none.
var kindHarnesses = map[string][]string{
	"runsheet/validate": {"none"},
}

// jobHarness is the platform's harness check on a claimJob (runsheet
// claimjob.go jobHarness): a claim naming a harness its kind does not
// list is refused; one naming none takes the kind's first.
func jobHarness(in map[string]any) error {
	kind, _ := in["kind"].(string)
	asked, _ := in["harness"].(string)
	listed, ok := kindHarnesses[kind]
	if !ok || asked == "" || slices.Contains(listed, asked) {
		return nil
	}
	return fmt.Errorf("claimJob: a %s job runs with %s, not %s", kind, strings.Join(listed, " or "), asked)
}

// jobClaimAlias is the older claim a claimJob routes to, and its input.
func jobClaimAlias(in map[string]any) (string, map[string]any, error) {
	kind, _ := in["kind"].(string)
	subject, _ := in["subject"].(map[string]any)
	typ, _ := subject["type"].(string)
	id, _ := subject["id"].(string)
	args := map[string]any{"aggregateId": id, "namespace": "runsheet", "nodeId": in["nodeId"]}
	field := ""
	switch kind + " " + typ {
	case "runsheet/build item":
		field = "claimWorkItem"
		if h, _ := in["harness"].(string); h != "" {
			args["harness"] = h
		}
		if a, _ := in["actor"].(string); a != "" {
			args["actor"] = a
		}
	case "runsheet/evaluate item":
		field = "claimWorkItemEvaluation"
	case "runsheet/rebase item":
		field = "claimWorkItemRebase"
	case "runsheet/plan goal":
		field = "claimPlan"
	case "runsheet/research direction":
		field = "claimDirectionResearch"
		args["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	case "runsheet/validate candidate":
		field = "claimMergeBatchValidation"
		if h, _ := in["harness"].(string); h != "" {
			args["harness"] = h
		}
	case "runsheet/validate main":
		field = "claimWorkItemMainValidation"
		if h, _ := in["harness"].(string); h != "" {
			args["harness"] = h
		}
	case "runsheet/validate pull_request":
		field = "claimPullRequestValidation"
		if h, _ := in["harness"].(string); h != "" {
			args["harness"] = h
		}
	default:
		return "", nil, fmt.Errorf("claimJob: Mesh has no claim for a %s job on a %s", kind, typ)
	}
	return field, args, nil
}

// finishJobAliases serves a finishJob as the older finishes it routes
// to, in order, and answers each one's answer by name.
func (f *Fake) finishJobAliases(r *http.Request, in map[string]any, bearer string) (map[string]any, error) {
	kind, _ := in["kind"].(string)
	subject, _ := in["subject"].(map[string]any)
	typ, _ := subject["type"].(string)
	id, _ := subject["id"].(string)
	fin, _ := in["finish"].(map[string]any)
	if strings.TrimSpace(fmt.Sprint(in["nodeId"])) == "" || in["nodeId"] == nil {
		return nil, fmt.Errorf("finishJob: nodeId names the node that finishes")
	}
	answers := map[string]any{}
	attest := func(arg string, att map[string]any) error {
		q := fmt.Sprintf("mutation($id: UUID!, $in: Map!) { recordValidationAttestation(%s: $id, input: $in) }", arg)
		data, errs := f.call(r, q, map[string]any{"id": id, "in": att}, bearer)
		if errs != nil {
			return fmt.Errorf("%s", errs[0])
		}
		answers["recordValidationAttestation"] = data["recordValidationAttestation"]
		return nil
	}
	send := func(field string, value map[string]any) error {
		args := map[string]any{"aggregateId": id, "namespace": "runsheet"}
		for k, v := range value {
			args[k] = v
		}
		if _, errs := f.call(r, mutationQuery(field), map[string]any{"in": args}, bearer); errs != nil {
			return fmt.Errorf("%s", errs[0])
		}
		return nil
	}
	part := func(key string) (map[string]any, error) {
		v, ok := fin[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("finishJob: a %s job finishes with {%s}", kind, key)
		}
		return v, nil
	}
	var err error
	switch kind + " " + typ {
	case "runsheet/build item":
		var v map[string]any
		if v, err = part("changeset"); err == nil {
			if att, ok := fin["attestation"].(map[string]any); ok {
				if err = attest("workItemId", att); err != nil {
					return nil, err
				}
			}
			err = send("finishWorkItem", v)
		}
	case "runsheet/evaluate item":
		// an evaluator session hands its verdict to the product's own
		// tool (mesh_verdict); the node then closes the job with its notes
		// tail, which the product refuses when no verdict reached that tool
		// (EvaluationNotHandedIn). An older node's {verdict} is still served.
		if _, ok := fin["result"]; ok {
			if f.EvaluationNotHandedIn != "" {
				err = fmt.Errorf("%s", f.EvaluationNotHandedIn)
				break
			}
			if v := f.HandedInVerdicts[id]; v != nil {
				err = send("evaluateWorkItem", v)
			}
			break
		}
		var v map[string]any
		if v, err = part("verdict"); err == nil {
			err = send("evaluateWorkItem", v)
		}
	case "runsheet/plan goal":
		// a plan session hands its graph to the product's own tool
		// (finishTool); the node then closes the job with an empty result,
		// which the product refuses when no plan reached that tool
		// (PlanNotHandedIn). An older node's {plan} is still served.
		if _, ok := fin["result"]; ok {
			if f.PlanNotHandedIn != "" {
				err = fmt.Errorf("%s", f.PlanNotHandedIn)
				break
			}
			// what the product's tool did with the plan: the goal leaves
			// planning, as runsheet's mesh_plan moves it
			if g := f.Goals[id]; g != nil {
				g["status"] = "awaiting_approval"
			}
			break
		}
		var v map[string]any
		if v, err = part("plan"); err == nil {
			err = send("planGoal", v)
		}
	case "runsheet/research direction":
		var v map[string]any
		if v, err = part("research"); err == nil {
			err = send("recordDirectionResearch", v)
		}
	case "runsheet/rebase item":
		var v map[string]any
		if v, err = part("rebasedHead"); err == nil {
			err = send("finishWorkItemRebase", v)
		}
	case "runsheet/validate candidate", "runsheet/validate main", "runsheet/validate pull_request":
		var att map[string]any
		if att, err = part("attestation"); err != nil {
			return nil, err
		}
		arg, finish := "mergeBatchId", "finishMergeBatchValidation"
		value := map[string]any{"nodeId": in["nodeId"], "sha": att["commit"]}
		switch typ {
		case "main":
			arg, finish = "workItemId", "finishWorkItemMainValidation"
			value = map[string]any{"sha": att["commit"]}
		case "pull_request":
			// the platform records the attestation and sends the finish
			// itself (runsheet: Validate hand-opened pull requests on a node)
			arg, finish = "pullRequestValidationId", "finishPullRequestValidation"
		}
		if err = attest(arg, att); err == nil {
			err = send(finish, value)
		}
	default:
		err = fmt.Errorf("finishJob: Mesh has no finish for a %s job on a %s", kind, typ)
	}
	return answers, err
}

// jobAccessAliases serves a jobAccess as the older reads it replaces:
// the repository token, the model accounts and the runsheet tools.
func (f *Fake) jobAccessAliases(r *http.Request, in map[string]any, bearer string) (map[string]any, error) {
	kind, _ := in["kind"].(string)
	subject, _ := in["subject"].(map[string]any)
	typ, _ := subject["type"].(string)
	id, _ := subject["id"].(string)
	out := map[string]any{"kind": kind, "subject": subject}
	missing := []map[string]any{}
	token := func(q string, field string) {
		data, errs := f.call(r, q, map[string]any{"id": id}, bearer)
		if errs != nil {
			missing = append(missing, map[string]any{"access": "repo", "reason": errs[0]})
			return
		}
		out["repo"] = data[field]
	}
	session := func(creds, tools, arg string) error {
		q := fmt.Sprintf("query($id: UUID!) { %s(%s: $id) %s(%s: $id) }", creds, arg, tools, arg)
		data, errs := f.call(r, q, map[string]any{"id": id}, bearer)
		if errs != nil {
			return fmt.Errorf("%s", errs[0])
		}
		out["model"] = data[creds]
		out["mcp"] = map[string]any{"runsheet": data[tools]}
		return nil
	}
	switch kind + " " + typ {
	case "runsheet/build item", "runsheet/evaluate item", "runsheet/rebase item":
		token("mutation($id: UUID!) { issueItemRepoToken(workItemId: $id) }", "issueItemRepoToken")
		if err := session("itemCredentials", "itemTools", "itemId"); err != nil {
			return nil, err
		}
	case "runsheet/plan goal":
		token("query($id: UUID!) { planGitHubToken(goalId: $id) }", "planGitHubToken")
		if err := session("planCredentials", "planTools", "goalId"); err != nil {
			return nil, err
		}
	case "runsheet/research direction":
		token("query($id: UUID!) { researchGitHubToken(directionId: $id) }", "researchGitHubToken")
		if err := session("researchCredentials", "researchTools", "directionId"); err != nil {
			return nil, err
		}
	case "runsheet/validate candidate":
		token("mutation($id: UUID!) { issueItemRepoToken(mergeBatchId: $id) }", "issueItemRepoToken")
	case "runsheet/validate main", "runsheet/validate pull_request":
		// a read token of the merged item's repo, whatever the item's
		// status: the validation holds the claim, not the item
		if tok := f.GitHubTokens[id]; tok != nil {
			out["repo"] = copyRow(tok)
		} else {
			missing = append(missing, map[string]any{"access": "repo", "reason": "no GitHub installation reaches the item's repo"})
		}
	default:
		return nil, fmt.Errorf("jobAccess: no such job")
	}
	if len(missing) > 0 {
		out["notServed"] = missing
	}
	return out, nil
}

// turnOf is the turn an input's subject names.
func (f *Fake) turnOf(in map[string]any) *ThreadTurn {
	subject, _ := in["subject"].(map[string]any)
	id, _ := subject["id"].(string)
	return f.ThreadTurns[id]
}

// claimThreadJob claims a raised thread turn as its kind.
func (f *Fake) claimThreadJob(w http.ResponseWriter, in map[string]any) {
	t := f.turnOf(in)
	f.Calls = append(f.Calls, Call{Name: "claimJob", Input: in, At: time.Now()})
	if t == nil || t.Status != "raised" || in["kind"] != t.Kind {
		writeErrors(w, 200, "claimThreadSession: not yours")
		return
	}
	harnesses, finish := t.Harness, t.Finish
	if harnesses == nil {
		harnesses = []string{"claude-code", "codex"}
	}
	if finish == "" {
		finish = "answer-or-proposal"
	}
	// the platform claims a kind on a harness it runs; another is refused
	if adapter, _ := in["harness"].(string); adapter != "" && !slices.Contains(harnesses, adapter) {
		writeErrors(w, 200, "claimThreadSession: the kind does not run on "+adapter)
		return
	}
	t.Status, t.NodeID = "claimed", fmt.Sprint(in["nodeId"])
	writeData(w, map[string]any{"claimJob": map[string]any{"status": "claimed", "job": map[string]any{
		"kind": t.Kind, "subject": map[string]any{"type": "thread", "id": t.ID}, "code": map[string]any{"form": "none"},
		"harness": harnesses[0], "harnesses": harnesses, "access": []string{t.Product, "model"},
		"finish": finish, "pods": []map[string]any{{"name": t.Role, "from": "kind", "code": false}},
		"guidance": t.Guidance, "finishSchema": t.FinishSchema, "finishTool": kindFinishTools[t.Kind],
		"claim": "claimThreadSession", "thread": map[string]any{"product": t.Product, "role": t.Role, "threadKey": t.ThreadKey,
			"ask": t.Ask, "body": t.Body, "projectId": t.ProjectID}}}})
}

// threadJobAccess is a claimed turn's exchange: its model accounts.
func (f *Fake) threadJobAccess(w http.ResponseWriter, in map[string]any) {
	t := f.turnOf(in)
	f.Calls = append(f.Calls, Call{Name: "jobAccess", Input: in, At: time.Now()})
	if t == nil || t.Status != "claimed" {
		writeErrors(w, 200, "jobAccess: not yours")
		return
	}
	creds, _ := json.Marshal(t.Credentials)
	writeData(w, map[string]any{"jobAccess": map[string]any{"kind": t.Kind, "subject": map[string]any{"type": "thread", "id": t.ID},
		"access": []string{"runsheet", "model"}, "model": json.RawMessage(creds), "mcp": map[string]any{"runsheet": []any{}}}})
}

// finishThreadJob answers a claimed turn.
func (f *Fake) finishThreadJob(w http.ResponseWriter, in map[string]any) {
	t := f.turnOf(in)
	f.Calls = append(f.Calls, Call{Name: "finishJob", Input: in, At: time.Now()})
	if t == nil || t.Status != "claimed" || fmt.Sprint(in["nodeId"]) != t.NodeID {
		writeErrors(w, 200, "answerThreadSession: not yours")
		return
	}
	fin, _ := in["finish"].(map[string]any)
	t.Finished = fin
	t.Answer, _ = fin["answer"].(string)
	t.Proposal, _ = fin["proposal"].(string)
	t.Status = "answered"
	writeData(w, map[string]any{"finishJob": map[string]any{"status": "finished"}})
}

// downstreamWriter answers a job call as the older call it routed to
// answered when that was no 200 (a 503, a stall's status): the status
// and body are the older call's, not the job call's own.
type downstreamWriter struct {
	http.ResponseWriter
	f    *Fake
	done bool
}

func (d *downstreamWriter) WriteHeader(code int) {
	if d.passed() {
		return
	}
	d.ResponseWriter.WriteHeader(code)
}

func (d *downstreamWriter) Write(b []byte) (int, error) {
	if d.passed() {
		return len(b), nil
	}
	return d.ResponseWriter.Write(b)
}

// passed writes the older call's answer once, and reports whether it did.
func (d *downstreamWriter) passed() bool {
	rec := d.f.downstream
	if rec == nil {
		return false
	}
	if !d.done {
		d.done = true
		for k, vs := range rec.Header() {
			for _, v := range vs {
				d.ResponseWriter.Header().Add(k, v)
			}
		}
		d.ResponseWriter.WriteHeader(rec.Code)
		_, _ = d.ResponseWriter.Write(rec.Body.Bytes())
	}
	return true
}

// threadSessionFields are the ThreadSessionIndex fields the gateway
// serves (its schema.json type).
var threadSessionFields = map[string]bool{"answeredAt": true, "claimedAt": true, "id": true, "namespace": true,
	"nodeId": true, "orgId": true, "product": true, "projectId": true, "raisedAt": true, "role": true,
	"status": true, "threadKey": true, "updatedAt": true}

// unservedThreadField is the first field a threadSessionIndexs query
// selects that the gateway does not serve, or "".
func unservedThreadField(q string) string {
	i := strings.Index(q, "threadSessionIndexs(")
	if i < 0 {
		return ""
	}
	// The selection opens after the arguments' close, past any { in a
	// where filter.
	open := strings.Index(q[i:], ") {")
	if open < 0 {
		return ""
	}
	sel := q[i+open+3:]
	if end := strings.Index(sel, "}"); end >= 0 {
		sel = sel[:end]
	}
	for _, field := range strings.Fields(sel) {
		if !threadSessionFields[field] {
			return field
		}
	}
	return ""
}
