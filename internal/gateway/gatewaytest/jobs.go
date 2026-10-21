package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// jobAPI answers the job system's calls on thread turns (runsheet ADR
// 061 §3–§4): threadSessionIndexs, claimJob, jobAccess and finishJob.
// f.mu is held. It reports whether q was one of them.
func (f *Fake) jobAPI(w http.ResponseWriter, q string, vars map[string]any) bool {
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
	case strings.Contains(q, "claimJob(input"):
		in, _ := vars["in"].(map[string]any)
		t := f.turnOf(in)
		f.Calls = append(f.Calls, Call{Name: "claimJob", Input: in, At: time.Now()})
		if t == nil || t.Status != "raised" || in["kind"] != t.Kind {
			writeErrors(w, 200, "claimThreadSession: not yours")
			return true
		}
		t.Status, t.NodeID = "claimed", fmt.Sprint(in["nodeId"])
		writeData(w, map[string]any{"claimJob": map[string]any{"status": "claimed", "job": map[string]any{
			"kind": t.Kind, "subject": map[string]any{"type": "thread", "id": t.ID}, "code": map[string]any{"form": "none"},
			"harness": "claude-code", "harnesses": []string{"claude-code"}, "access": []string{"runsheet", "model"},
			"finish": "answer-or-proposal", "pods": []map[string]any{{"name": "goal-chat", "from": "kind", "code": false}},
			"claim": "claimThreadSession", "thread": map[string]any{"product": t.Product, "role": t.Role, "threadKey": t.ThreadKey,
				"ask": t.Ask, "body": t.Body, "projectId": t.ProjectID}}}})
	case strings.Contains(q, "jobAccess(input"):
		in, _ := vars["in"].(map[string]any)
		t := f.turnOf(in)
		f.Calls = append(f.Calls, Call{Name: "jobAccess", Input: in, At: time.Now()})
		if t == nil || t.Status != "claimed" {
			writeErrors(w, 200, "jobAccess: not yours")
			return true
		}
		creds, _ := json.Marshal(t.Credentials)
		writeData(w, map[string]any{"jobAccess": map[string]any{"kind": t.Kind, "subject": map[string]any{"type": "thread", "id": t.ID},
			"access": []string{"runsheet", "model"}, "model": json.RawMessage(creds), "mcp": map[string]any{"runsheet": []any{}}}})
	case strings.Contains(q, "finishJob(input"):
		in, _ := vars["in"].(map[string]any)
		t := f.turnOf(in)
		f.Calls = append(f.Calls, Call{Name: "finishJob", Input: in, At: time.Now()})
		if t == nil || t.Status != "claimed" || fmt.Sprint(in["nodeId"]) != t.NodeID {
			writeErrors(w, 200, "answerThreadSession: not yours")
			return true
		}
		fin, _ := in["finish"].(map[string]any)
		t.Answer, _ = fin["answer"].(string)
		t.Proposal, _ = fin["proposal"].(string)
		t.Status = "answered"
		writeData(w, map[string]any{"finishJob": map[string]any{"status": "finished"}})
	default:
		return false
	}
	return true
}

// turnOf is the turn an input's subject names.
func (f *Fake) turnOf(in map[string]any) *ThreadTurn {
	subject, _ := in["subject"].(map[string]any)
	id, _ := subject["id"].(string)
	return f.ThreadTurns[id]
}
