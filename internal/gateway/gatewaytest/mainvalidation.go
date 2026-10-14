package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The platform's merge batch and main validations (runsheet ADR 048
// decision 3, ADR 054's amendment 2026-10-13), as the aggregates rule
// them: a validation is requested, claimed by one node — the same node
// again converges, another is refused — and finished on its sha or
// failed, a fail releasing the claim for the next node.

// mainValidationMutations are the six commands, as the Mutation type
// introspection lists them.
var mainValidationMutations = []string{
	"claimMergeBatchValidation", "finishMergeBatchValidation", "failMergeBatchValidation",
	"claimWorkItemMainValidation", "finishWorkItemMainValidation", "failWorkItemMainValidation",
}

var mainValidationCommands = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range mainValidationMutations {
		m[n] = true
	}
	return m
}()

// The fake's own keys on a seeded row, beside the served fields: never
// served by mainValidations.
const (
	validationState  = "validationState"
	validationNode   = "validationNodeId"
	validationReason = "validationReason"
	validationSha    = "validationFinishedSha"
)

// SeedMergeBatch puts a batch candidate on mainValidations, requested
// and held by no node.
func (f *Fake) SeedMergeBatch(b gateway.MainValidationBatch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MergeBatches[b.MergeBatchID] = seedRow(b)
}

// SeedMainValidationRequest puts a merged item's main validation on
// mainValidations, requested and held by no node.
func (f *Fake) SeedMainValidationRequest(r gateway.MainValidationRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MainValidationRequests[r.WorkItemID] = seedRow(r)
}

func seedRow(v any) map[string]any {
	raw, _ := json.Marshal(v)
	var row map[string]any
	_ = json.Unmarshal(raw, &row)
	row[validationState] = "requested"
	return row
}

// MergeBatch is a copy of the batch's row: its served fields and
// validationState (requested, claimed, finished), validationNodeId,
// validationReason (the last fail's) and validationFinishedSha.
func (f *Fake) MergeBatch(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyRow(f.MergeBatches[id])
}

// MainValidationRequest is a copy of the merged item's request row, as
// MergeBatch's.
func (f *Fake) MainValidationRequest(itemID string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyRow(f.MainValidationRequests[itemID])
}

// queryType is the Query type's introspection: mainValidations unless
// Unsupported names it.
func (f *Fake) queryType() map[string]any {
	fields := []any{}
	if !f.Unsupported["mainValidations"] {
		fields = append(fields, map[string]any{"name": "mainValidations"})
	}
	return map[string]any{"fields": fields}
}

// mainValidations answers mainValidations(nodeId): every row still
// requested, and those the asking node holds, without the fake's keys.
func (f *Fake) mainValidations(w http.ResponseWriter, vars map[string]any) {
	const field = "mainValidations"
	node, _ := vars["node"].(string)
	f.Reads = append(f.Reads, Call{Name: field, Input: map[string]any{"nodeId": node}, At: time.Now()})
	if f.Unsupported[field] {
		writeErrors(w, 200, fmt.Sprintf("Cannot query field %q on type \"Query\".", field))
		return
	}
	if msg := f.FailReads[field]; msg != "" {
		writeErrors(w, 200, msg)
		return
	}
	open := func(rows map[string]map[string]any) []any {
		out := []any{}
		for _, id := range sortedKeys(rows) {
			row := rows[id]
			switch {
			case row[validationState] == "requested":
			case row[validationState] == "claimed" && row[validationNode] == node:
			default:
				continue
			}
			served := copyRow(row)
			for _, k := range []string{validationState, validationNode, validationReason, validationSha} {
				delete(served, k)
			}
			out = append(out, served)
		}
		return out
	}
	writeData(w, map[string]any{field: map[string]any{
		"batches":  open(f.MergeBatches),
		"requests": open(f.MainValidationRequests),
	}})
}

func sortedKeys(rows map[string]map[string]any) []string {
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// applyMainValidation runs one of the six commands on its row.
func (f *Fake) applyMainValidation(name string, in map[string]any) error {
	id, _ := in["aggregateId"].(string)
	rows, what := f.MergeBatches, "merge batch"
	switch name {
	case "claimWorkItemMainValidation", "finishWorkItemMainValidation", "failWorkItemMainValidation":
		rows, what = f.MainValidationRequests, "main validation"
	}
	row := rows[id]
	if row == nil {
		return fmt.Errorf("no %s %s waits on a node", what, id)
	}
	node, _ := in["nodeId"].(string)
	holder, _ := row[validationNode].(string)
	switch name {
	case "claimMergeBatchValidation", "claimWorkItemMainValidation":
		switch row[validationState] {
		case "claimed":
			if holder == node {
				return nil
			}
			return fmt.Errorf("node %s has claimed the validation", holder)
		case "requested":
			row[validationState], row[validationNode] = "claimed", node
			return nil
		}
		return fmt.Errorf("the %s's validation is %s, not requested", what, row[validationState])
	case "finishMergeBatchValidation", "finishWorkItemMainValidation":
		if row[validationState] != "claimed" {
			return fmt.Errorf("the %s's validation is %s, not claimed", what, row[validationState])
		}
		if name == "finishMergeBatchValidation" && holder != node {
			return fmt.Errorf("node %s has claimed the validation, not %s", holder, node)
		}
		sha, _ := in["sha"].(string)
		if want, _ := row["sha"].(string); sha != want {
			return fmt.Errorf("sha %s is not the validation's %s", sha, want)
		}
		row[validationState], row[validationSha] = "finished", sha
	case "failMergeBatchValidation", "failWorkItemMainValidation":
		if row[validationState] != "claimed" {
			return fmt.Errorf("the %s's validation is %s, not claimed", what, row[validationState])
		}
		if name == "failMergeBatchValidation" && holder != node {
			return fmt.Errorf("node %s has claimed the validation, not %s", holder, node)
		}
		// a fail releases the claim: the validation waits for a node again
		row[validationState], row[validationReason] = "requested", in["reason"]
		delete(row, validationNode)
	}
	return nil
}

// recordBatchCandidateAttestation is recordValidationAttestation(
// mergeBatchId, input) (specs/contracts/
// recordValidationAttestation-batchCandidate.json): recorded as a call
// with mergeBatchId and input, refused like an item's, and answered
// BatchAttestationReceipt or the batch's candidate branch.
func (f *Fake) recordBatchCandidateAttestation(w http.ResponseWriter, vars map[string]any, bearer string) {
	const field = "recordValidationAttestation"
	id, _ := vars["id"].(string)
	in, _ := vars["in"].(map[string]any)
	f.Calls = append(f.Calls, Call{Name: field, Input: map[string]any{"mergeBatchId": id, "input": in}, At: time.Now(), Bearer: bearer})
	if q := f.RefuseOnce[field]; len(q) > 0 {
		f.RefuseOnce[field] = q[1:]
		writeErrors(w, 200, q[0])
		return
	}
	if msg := f.Refuse[field]; msg != "" {
		writeErrors(w, 200, msg)
		return
	}
	batch := f.MergeBatches[id]
	if batch == nil {
		writeErrors(w, 200, fmt.Sprintf("no merge batch %s", id))
		return
	}
	receipt := f.BatchAttestationReceipt
	if receipt == nil {
		receipt = map[string]any{"status": "ok", "commit": in["commit"], "branch": batch["candidateBranch"], "mergeBatchId": id}
	}
	writeData(w, map[string]any{field: receipt})
}
