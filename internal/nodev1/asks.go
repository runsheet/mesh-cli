package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// asks.jsonl is the node's own durable ledger of every request_time
// call (requesttimetool.go), granted or refused, and of how each lease
// that made one ended — a plain audit trail kept for this node alone,
// never sent to Mesh and never confused with the signed evidence
// nodetools.Recorder carries for push, run_suite and apply_onto.
//
// It lives at WorkRoot/asks.jsonl, a sibling of leases/ and threads/,
// deliberately never inside a lease's own directory
// (Runtime.leaseDir): that directory is removed as soon as the lease's
// work is done — by work()'s own deferred cleanup before report() even
// runs, or by (*actor.Actor).Release() once reporting is over — so
// anything written there would be gone before, or shortly after, the
// lease's end is known. A record meant to be read together with how
// the job ended, possibly long after, has to live somewhere that
// outcome does not touch.
//
// Each line is one AskRecord, appended and fsynced the moment it
// happens, never rewritten: an "ask" line for the call itself (its
// args and what it answered), and, once the lease it came from
// concludes, one "end" line naming the lease's actor.End. The two join
// by JobID and LeaseID at read time (askEndsOf, askRecordsFor): nothing
// is ever written back into an earlier line, so a record already on
// disk when the session was cut off stays exactly as it was.

const (
	askKindAsk = "ask"
	askKindEnd = "end"
)

// AskRecord is one line of asks.jsonl.
type AskRecord struct {
	Type    string    `json:"type"`
	Time    time.Time `json:"time"`
	JobID   string    `json:"job_id"`
	LeaseID string    `json:"lease_id"`

	// Ask fields, set when Type is askKindAsk.
	Kind           string `json:"kind,omitempty"`
	Tenant         string `json:"tenant,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Model          string `json:"model,omitempty"`
	MinutesAsked   int    `json:"minutes_asked,omitempty"`
	MinutesGranted int    `json:"minutes_granted,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Progress       string `json:"progress,omitempty"`
	Granted        bool   `json:"granted,omitempty"`
	Refusal        string `json:"refusal,omitempty"`

	// End fields, set when Type is askKindEnd: the lease's actor.End,
	// named to read alike (Report is finish, fail, hand_back or none).
	Report    string `json:"report,omitempty"`
	Cause     string `json:"cause,omitempty"`
	EndReason string `json:"end_reason,omitempty"`
}

// asksPath is the node's one ask ledger.
func (r *Runtime) asksPath() string { return filepath.Join(r.WorkRoot, "asks.jsonl") }

// appendAsk appends rec to asks.jsonl, fsynced before it returns: the
// record is durable the instant the call returns, not merely buffered,
// so a session cut off right afterward — ctx cancelled, stopped by a
// rule, or killed — still leaves it on disk. A write that fails is
// logged and never stops the tool it came from from answering; the
// ledger observes, it does not gate.
func (r *Runtime) appendAsk(rec AskRecord) {
	if r.WorkRoot == "" {
		return
	}
	if err := appendAskLine(r.asksPath(), rec); err != nil {
		r.logf("nodev1: lease %s: asks.jsonl: %v", rec.LeaseID, err)
	}
}

// appendAskLine does the actual durable append: opened O_APPEND so the
// file is never rewritten and a line already read stays byte-equal,
// then Sync before the descriptor closes.
func appendAskLine(path string, rec AskRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// readAsks reads every record asks.jsonl holds, oldest first; none when
// the file does not exist yet. A line that fails to parse is skipped
// rather than failing the read: the ledger is best-effort local
// bookkeeping, not evidence a read must trust completely.
func readAsks(path string) ([]AskRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []AskRecord
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec AskRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// askRecordsFor is every record asks.jsonl holds for jobID+leaseID, in
// the order they were appended: its asks, then its end once the lease
// has one — how the job ended, read together with what it asked for
// along the way.
func askRecordsFor(path, jobID, leaseID string) ([]AskRecord, error) {
	all, err := readAsks(path)
	if err != nil {
		return nil, err
	}
	var out []AskRecord
	for _, rec := range all {
		if rec.JobID == jobID && rec.LeaseID == leaseID {
			out = append(out, rec)
		}
	}
	return out, nil
}

// ladderModel is the model an ask should be recorded against: the
// lease's current, possibly escalated, model when it has a ladder,
// else the kind's own default.
func ladderModel(ladder *escalate.Ladder, b *meshapi.Brief) string {
	if ladder != nil {
		if m := ladder.Model(); m != "" {
			return m
		}
	}
	return b.Harness.Model.Default
}

// recordRequestTime wraps tool's Call (requestTimeTool's own) so every
// attempt to ask for more time is written to asks.jsonl before the tool
// answers — granted, refused for the kind's cap, or refused for a
// missing or invalid argument alike. It never changes what the tool
// answers or whether it errors; it only observes and records.
func (r *Runtime) recordRequestTime(b *meshapi.Brief, model string, tool mcptool.Tool) mcptool.Tool {
	call := tool.Call
	tool.Call = func(ctx context.Context, args map[string]any) (string, error) {
		out, err := call(ctx, args)
		r.appendAsk(askRecordOf(b, model, args, out, err))
		return out, err
	}
	return tool
}

// askRecordOf builds asks.jsonl's record of one request_time call from
// its arguments and what requestTimeTool answered: an error names the
// refusal (a missing reason or progress, progress over the limit, or
// minutes not positive — requesttimetool.go's own validation, never
// re-done here), and the JSON answer's own "granted" field says whether
// an otherwise-valid ask was inside the kind's cap.
func askRecordOf(b *meshapi.Brief, model string, args map[string]any, out string, err error) AskRecord {
	rec := AskRecord{
		Type: askKindAsk, Time: time.Now().UTC(), JobID: b.JobID, LeaseID: b.LeaseID,
		Kind: b.Kind, Tenant: b.Tenant, Model: model, MinutesAsked: intArg(args["minutes"]),
	}
	if b.Git != nil {
		rec.Repo = b.Git.Repo
	}
	reason, _ := args["reason"].(string)
	rec.Reason = strings.TrimSpace(reason)
	progress, _ := args["progress"].(string)
	rec.Progress = strings.TrimSpace(firstLine(progress))
	if err != nil {
		rec.Granted = false
		rec.Refusal = err.Error()
		return rec
	}
	var answer struct {
		Granted bool   `json:"granted"`
		Minutes int    `json:"minutes"`
		Note    string `json:"note"`
	}
	_ = json.Unmarshal([]byte(out), &answer)
	rec.Granted = answer.Granted
	if answer.Granted {
		rec.MinutesGranted = answer.Minutes
	} else {
		rec.Refusal = answer.Note
	}
	return rec
}

// recordAskEnd appends asks.jsonl's "end" record for a lease once its
// outcome is decided (report's return, whichever path it took),
// joinable to its asks by JobID+LeaseID. Only a lease whose kind named
// request_time ever gets one, so a kind that never asks for more time
// leaves the ledger alone.
func (r *Runtime) recordAskEnd(b *meshapi.Brief, end actor.End) {
	if b == nil || !slices.Contains(b.NodeTools, RequestTimeTool) {
		return
	}
	r.appendAsk(AskRecord{
		Type: askKindEnd, Time: time.Now().UTC(), JobID: b.JobID, LeaseID: b.LeaseID,
		Report: end.Report, Cause: end.Cause, EndReason: end.Reason,
	})
}
