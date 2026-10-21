package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// ThreadTurn is one thread turn the fake holds (runsheet ThreadSession):
// raised until a node claims it as Kind, answered by its finish.
type ThreadTurn struct {
	ID, Product, ThreadKey, OrgID, ProjectID, Role, Kind, Body, Ask string
	Status                                                         string
	NodeID                                                         string
	// Answer and Proposal are the finish's.
	Answer, Proposal string
	// Credentials are the model accounts jobAccess serves the turn, as
	// itemCredentials rows.
	Credentials []map[string]any
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
	switch {
	case strings.Contains(q, "threadSessionIndexs("):
		rows := []map[string]any{}
		for _, t := range f.ThreadTurns {
			if t.Status != "raised" || !strings.Contains(q, fmt.Sprintf("%q", t.OrgID)) {
				continue
			}
			rows = append(rows, map[string]any{"id": t.ID, "product": t.Product, "thread_key": t.ThreadKey, "org_id": t.OrgID,
				"project_id": t.ProjectID, "role": t.Role, "status": t.Status})
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
		writeData(w, map[string]any{"claimJob": map[string]any{"status": "claimed", "job": map[string]any{
			"kind": kind, "subject": subject, "code": map[string]any{"form": "commit"}, "claim": field}}})
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
		var v map[string]any
		if v, err = part("verdict"); err == nil {
			err = send("evaluateWorkItem", v)
		}
	case "runsheet/plan goal":
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
	case "runsheet/validate candidate", "runsheet/validate main":
		var att map[string]any
		if att, err = part("attestation"); err != nil {
			return nil, err
		}
		arg, finish := "mergeBatchId", "finishMergeBatchValidation"
		value := map[string]any{"nodeId": in["nodeId"], "sha": att["commit"]}
		if typ == "main" {
			arg, finish = "workItemId", "finishWorkItemMainValidation"
			value = map[string]any{"sha": att["commit"]}
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
	case "runsheet/validate main":
		token("mutation($id: UUID!) { issueItemRepoToken(workItemId: $id) }", "issueItemRepoToken")
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
	t.Status, t.NodeID = "claimed", fmt.Sprint(in["nodeId"])
	writeData(w, map[string]any{"claimJob": map[string]any{"status": "claimed", "job": map[string]any{
		"kind": t.Kind, "subject": map[string]any{"type": "thread", "id": t.ID}, "code": map[string]any{"form": "none"},
		"harness": "claude-code", "harnesses": []string{"claude-code"}, "access": []string{"runsheet", "model"},
		"finish": "answer-or-proposal", "pods": []map[string]any{{"name": "goal-chat", "from": "kind", "code": false}},
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
	t.Answer, _ = fin["answer"].(string)
	t.Proposal, _ = fin["proposal"].(string)
	t.Status = "answered"
	writeData(w, map[string]any{"finishJob": map[string]any{"status": "finished"}})
}
