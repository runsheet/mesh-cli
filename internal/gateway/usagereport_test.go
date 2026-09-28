package gateway_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// sampleUsageReport is one session's usage, every field set, so a test
// can check each rode through as `usage`.
func sampleUsageReport() *gateway.UsageReport {
	fhBefore, fhAfter := 0.1, 0.42
	sdBefore, sdAfter := 0.2, 0.35
	fhResetBefore := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	fhResetAfter := time.Date(2026, 9, 27, 15, 0, 0, 0, time.UTC)
	sdResetBefore := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sdResetAfter := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return &gateway.UsageReport{
		SessionID: "session-1", Adapter: "claude-code", Model: "sonnet",
		CredentialID:   "dddddddd-5555-4555-8555-dddddddddddd",
		CredentialKind: gateway.CredentialSubscription,
		InputTokens:    1000, OutputTokens: 200, CacheReadTokens: 50, CacheWriteTokens: 10,
		ToolCalls: 3, Tools: []gateway.ToolCallReport{{Tool: "brave", Calls: 3, Errors: 1, Ms: 1500}},
		WallMs: 45000, ListCostCents: 120, ActualCostCents: 0,
		Turns: 7, InputPerTurnMean: 140, InputPerTurnMax: 900,
		LargestToolResults: []gateway.ToolResultSize{
			{Tool: "mcp__search__query", Bytes: 5000},
			{Tool: "Bash", Bytes: 1200},
		},
		FiveHourBefore: &fhBefore, FiveHourAfter: &fhAfter,
		SevenDayBefore: &sdBefore, SevenDayAfter: &sdAfter,
		FiveHourResetsAtBefore: &fhResetBefore, FiveHourResetsAtAfter: &fhResetAfter,
		SevenDayResetsAtBefore: &sdResetBefore, SevenDayResetsAtAfter: &sdResetAfter,
	}
}

// usageCase is one of the five finishing commands: its mutation name,
// the row it needs to land, and how to send it with a usage report.
type usageCase struct {
	name  string
	setup func(f *gatewaytest.Fake)
	send  func(gw *gateway.Client, u *gateway.UsageReport) error
}

func usageCases() []usageCase {
	return []usageCase{
		{
			name:  "finishWorkItem",
			setup: runningItem,
			send: func(gw *gateway.Client, u *gateway.UsageReport) error {
				return gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc123", 12, u)
			},
		},
		{
			name:  "finishWorkItem",
			setup: runningItem,
			send: func(gw *gateway.Client, u *gateway.UsageReport) error {
				return gw.FinishWorkItemAlreadyDone(context.Background(), finishItem, "abc123 on main", 12, u)
			},
		},
		{
			name:  "evaluateWorkItem",
			setup: runningItem,
			send: func(gw *gateway.Client, u *gateway.UsageReport) error {
				return gw.EvaluateWorkItem(context.Background(), finishItem, true, "looks good", 30, u)
			},
		},
		{
			name: "planGoal",
			setup: func(f *gatewaytest.Fake) {
				f.Goals["goal-1"] = map[string]any{"id": "goal-1", "status": "planning"}
			},
			send: func(gw *gateway.Client, u *gateway.UsageReport) error {
				return gw.PlanGoal(context.Background(), "goal-1", "", []gateway.PlannedItem{{Seq: 1, Title: "docs"}}, 0, u)
			},
		},
		{
			name: "finishWorkItemRebase",
			setup: func(f *gatewaytest.Fake) {
				rebaseItem(f, itemID, 1, "claimed")
				f.WorkItems[itemID]["rebaseNodeId"] = nodeID
			},
			send: func(gw *gateway.Client, u *gateway.UsageReport) error {
				return gw.FinishWorkItemRebase(context.Background(), itemID, "abc123", []string{"main.go"}, u)
			},
		},
		{
			name: "recordDirectionResearch",
			setup: func(f *gatewaytest.Fake) {
				f.Directions["d1"] = directionRow("d1", projID, "overnight")
				f.Directions["d1"]["researchNodeId"] = nodeID
			},
			send: func(gw *gateway.Client, u *gateway.UsageReport) error {
				return gw.RecordDirectionResearch(context.Background(), "d1", nodeID, gateway.Research{}, 50, "0 proposed", u)
			},
		},
	}
}

// Each of the five commands that end a session carries its usage report
// as `usage` on the input, and never a secret (UsageReport has none to
// carry).
func TestFinishingCommandsSendUsage(t *testing.T) {
	for _, c := range usageCases() {
		t.Run(c.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			c.setup(f)
			gw := gateway.New(f.URL(), "rs_test")

			u := sampleUsageReport()
			if err := c.send(gw, u); err != nil {
				t.Fatal(err)
			}
			calls := f.CallsNamed(c.name)
			if len(calls) != 1 {
				t.Fatalf("calls = %+v", calls)
			}
			usage, ok := calls[0].Input["usage"].(map[string]any)
			if !ok {
				t.Fatalf("no usage on the input: %+v", calls[0].Input)
			}
			if usage["sessionId"] != u.SessionID || usage["adapter"] != u.Adapter || usage["model"] != u.Model ||
				usage["credentialId"] != u.CredentialID || usage["credentialKind"] != u.CredentialKind {
				t.Fatalf("usage = %+v", usage)
			}
			if usage["inputTokens"] != float64(u.InputTokens) || usage["outputTokens"] != float64(u.OutputTokens) ||
				usage["cacheReadTokens"] != float64(u.CacheReadTokens) || usage["cacheWriteTokens"] != float64(u.CacheWriteTokens) ||
				usage["toolCalls"] != float64(u.ToolCalls) || usage["wallMs"] != float64(u.WallMs) ||
				usage["listCostCents"] != float64(u.ListCostCents) || usage["actualCostCents"] != float64(u.ActualCostCents) {
				t.Fatalf("usage tokens/cost = %+v", usage)
			}
			if usage["fiveHourBefore"] != *u.FiveHourBefore || usage["fiveHourAfter"] != *u.FiveHourAfter ||
				usage["sevenDayBefore"] != *u.SevenDayBefore || usage["sevenDayAfter"] != *u.SevenDayAfter {
				t.Fatalf("usage windows = %+v", usage)
			}
			assertToolsRow(t, usage)
			if usage["turns"] != float64(u.Turns) || usage["inputPerTurnMean"] != float64(u.InputPerTurnMean) ||
				usage["inputPerTurnMax"] != float64(u.InputPerTurnMax) {
				t.Fatalf("usage turns = %+v", usage)
			}
			largest, ok := usage["largestToolResults"].([]any)
			if !ok || len(largest) != len(u.LargestToolResults) {
				t.Fatalf("usage largestToolResults = %+v", usage["largestToolResults"])
			}
			for i, row := range largest {
				m, ok := row.(map[string]any)
				if !ok || m["tool"] != u.LargestToolResults[i].Tool || m["bytes"] != float64(u.LargestToolResults[i].Bytes) {
					t.Fatalf("largestToolResults[%d] = %+v, want %+v", i, m, u.LargestToolResults[i])
				}
			}
			for k := range usage {
				if strings.Contains(strings.ToLower(k), "secret") {
					t.Fatalf("usage carried a secret field %q", k)
				}
			}
			if len(f.SessionUsage) != 1 || f.SessionUsage[0].Name != c.name {
				t.Fatalf("SessionUsage = %+v", f.SessionUsage)
			}
		})
	}
}

// A gateway whose input has no `usage` (a platform older than the usage
// ledger) still takes the command bare, and the caller is told the
// report did not land, with ErrNoUsageLedger.
func TestFinishingCommandsWithoutUsageField(t *testing.T) {
	for _, c := range usageCases() {
		t.Run(c.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			f.UnknownFields = map[string]bool{"usage": true}
			c.setup(f)
			gw := gateway.New(f.URL(), "rs_test")

			err := c.send(gw, sampleUsageReport())
			if !errors.Is(err, gateway.ErrNoUsageLedger) {
				t.Fatalf("err = %v, want ErrNoUsageLedger", err)
			}
			calls := f.CallsNamed(c.name)
			if len(calls) != 1 {
				t.Fatalf("calls = %+v", calls)
			}
			if _, ok := calls[0].Input["usage"]; ok {
				t.Fatalf("the landed call still carried usage: %+v", calls[0].Input)
			}
			if len(f.SessionUsage) != 0 {
				t.Fatalf("SessionUsage = %+v", f.SessionUsage)
			}
		})
	}
}

// assertToolsRow checks a sent usage map carries the per-tool list
// [{brave, 3, 1, ms > 0}] beside the total toolCalls 3.
func assertToolsRow(t *testing.T, usage map[string]any) {
	t.Helper()
	if usage["toolCalls"] != float64(3) {
		t.Fatalf("toolCalls = %v", usage["toolCalls"])
	}
	rows, ok := usage["tools"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("tools = %#v", usage["tools"])
	}
	row, _ := rows[0].(map[string]any)
	if row["tool"] != "brave" || row["calls"] != float64(3) || row["errors"] != float64(1) {
		t.Fatalf("tools row = %#v", row)
	}
	if ms, _ := row["ms"].(float64); ms <= 0 {
		t.Fatalf("tools row ms = %#v", row["ms"])
	}
}

// A gateway whose UsageReportInput has no `tools` (a platform older than
// tools-show-their-use) is sent the command again with the list taken
// out and the total kept, and the caller is told with ErrNoToolUsage;
// the client remembers, so a later report goes without it at once.
func TestFinishingCommandsWithoutToolsField(t *testing.T) {
	for _, c := range usageCases() {
		t.Run(c.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			f.NoUsageTools = true
			c.setup(f)
			gw := gateway.New(f.URL(), "rs_test")

			err := c.send(gw, sampleUsageReport())
			if !errors.Is(err, gateway.ErrNoToolUsage) {
				t.Fatalf("err = %v, want ErrNoToolUsage", err)
			}
			calls := f.CallsNamed(c.name)
			if len(calls) != 1 {
				t.Fatalf("calls = %+v", calls)
			}
			usage, ok := calls[0].Input["usage"].(map[string]any)
			if !ok {
				t.Fatalf("no usage on the landed call: %+v", calls[0].Input)
			}
			if _, ok := usage["tools"]; ok {
				t.Fatalf("the landed usage still carried tools: %+v", usage)
			}
			if usage["toolCalls"] != float64(3) {
				t.Fatalf("toolCalls = %v, want the total kept", usage["toolCalls"])
			}
			if len(f.SessionUsage) != 1 {
				t.Fatalf("SessionUsage = %+v", f.SessionUsage)
			}
		})
	}
}

// A report with no per-tool list against the old platform is no
// fallback at all: nil, not ErrNoToolUsage.
func TestFinishingCommandWithoutToolsNeedsNoFallback(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoUsageTools = true
	runningItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	u := sampleUsageReport()
	u.Tools = nil
	if err := gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc123", 12, u); err != nil {
		t.Fatal(err)
	}
}

// A gateway whose usage input has no turns, inputPerTurnMean,
// inputPerTurnMax or largestToolResults (a platform older than
// mesh-spends-less-per-item) still takes the rest of the report and the
// command, and the caller is told the detail did not land, with
// ErrNoUsageDetail.
func TestFinishingCommandsWithoutUsageDetail(t *testing.T) {
	for _, c := range usageCases() {
		t.Run(c.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			f.UnknownFields = map[string]bool{
				"turns": true, "inputPerTurnMean": true, "inputPerTurnMax": true, "largestToolResults": true,
			}
			c.setup(f)
			gw := gateway.New(f.URL(), "rs_test")

			err := c.send(gw, sampleUsageReport())
			if !errors.Is(err, gateway.ErrNoUsageDetail) {
				t.Fatalf("err = %v, want ErrNoUsageDetail", err)
			}
			calls := f.CallsNamed(c.name)
			if len(calls) != 1 {
				t.Fatalf("calls = %+v", calls)
			}
			usage, ok := calls[0].Input["usage"].(map[string]any)
			if !ok {
				t.Fatalf("the landed call carried no usage: %+v", calls[0].Input)
			}
			for _, field := range []string{"turns", "inputPerTurnMean", "inputPerTurnMax", "largestToolResults"} {
				if _, ok := usage[field]; ok {
					t.Fatalf("usage still carried %q: %+v", field, usage)
				}
			}
			if usage["inputTokens"] == nil {
				t.Fatalf("usage lost the rest of the report: %+v", usage)
			}
			if len(f.SessionUsage) != 1 || f.SessionUsage[0].Name != c.name {
				t.Fatalf("SessionUsage = %+v", f.SessionUsage)
			}
		})
	}
}

// RecordToolUsage sends the item, the tool and its call id, plus the
// usage report, against recordToolUsage.
func TestRecordToolUsage(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")

	u := sampleUsageReport()
	if err := gw.RecordToolUsage(context.Background(), itemID, "call-1", "search", *u); err != nil {
		t.Fatal(err)
	}
	if len(f.ToolUsage) != 1 {
		t.Fatalf("tool usage = %+v", f.ToolUsage)
	}
	row := f.ToolUsage[0]
	if row["aggregateId"] != itemID || row["tool"] != "search" || row["toolCallId"] != "call-1" || row["namespace"] != "runsheet" {
		t.Fatalf("row = %+v", row)
	}
	usage, ok := row["usage"].(map[string]any)
	if !ok || usage["sessionId"] != u.SessionID || usage["model"] != u.Model {
		t.Fatalf("usage = %+v", row["usage"])
	}
	if _, ok := usage["tools"]; ok {
		t.Fatalf("a per-call record carried the per-tool list: %+v", usage)
	}
}

// A gateway whose usage input has no turns, inputPerTurnMean,
// inputPerTurnMax or largestToolResults still takes the priced tool
// call — a report the fields on lands without them, same as the five
// finishing commands — and the caller is told the detail did not land,
// with ErrNoUsageDetail.
func TestRecordToolUsageWithoutUsageDetail(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields = map[string]bool{
		"turns": true, "inputPerTurnMean": true, "inputPerTurnMax": true, "largestToolResults": true,
	}
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.RecordToolUsage(context.Background(), itemID, "call-1", "search", *sampleUsageReport())
	if !errors.Is(err, gateway.ErrNoUsageDetail) {
		t.Fatalf("err = %v, want ErrNoUsageDetail", err)
	}
	if len(f.ToolUsage) != 1 {
		t.Fatalf("tool usage = %+v", f.ToolUsage)
	}
	usage, ok := f.ToolUsage[0]["usage"].(map[string]any)
	if !ok {
		t.Fatalf("the landed call carried no usage: %+v", f.ToolUsage[0])
	}
	for _, field := range []string{"turns", "inputPerTurnMean", "inputPerTurnMax", "largestToolResults"} {
		if _, ok := usage[field]; ok {
			t.Fatalf("usage still carried %q: %+v", field, usage)
		}
	}
	if usage["sessionId"] == nil {
		t.Fatalf("usage lost the rest of the report: %+v", usage)
	}
}

// An older platform without recordToolUsage answers IsUnsupported.
func TestRecordToolUsageUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Unsupported["recordToolUsage"] = true
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.RecordToolUsage(context.Background(), itemID, "call-1", "search", *sampleUsageReport())
	if !gateway.IsUnsupported(err) {
		t.Fatalf("RecordToolUsage = %v, want unsupported", err)
	}
	if len(f.ToolUsage) != 0 {
		t.Fatalf("tool usage = %+v", f.ToolUsage)
	}
}

// The fake refuses the two old usage mutations as the platform does:
// recordSessionUsage is no field of the schema, and recordUsage is the
// platform's own (RegisterPlatformOnly), a permission error naming it.
// Either call is recorded, so a test's CallsNamed finds any caller.
func TestFakeRefusesTheOldUsageMutations(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	in := map[string]any{"in": map[string]any{"namespace": gateway.NS}}
	err := gw.Query(ctx, `mutation($in: RecordSessionUsageInput!) { recordSessionUsage(input: $in) { status } }`, in, nil)
	if err == nil || !strings.Contains(err.Error(), `Cannot query field "recordSessionUsage" on type "Mutation"`) {
		t.Fatalf("recordSessionUsage = %v", err)
	}
	err = gw.Query(ctx, `mutation($in: RecordUsageInput!) { recordUsage(input: $in) { status } }`, in, nil)
	if err == nil || !strings.Contains(err.Error(), "recordUsage: permission denied") || strings.Contains(err.Error(), "Cannot query field") {
		t.Fatalf("recordUsage = %v", err)
	}
	if len(f.CallsNamed("recordSessionUsage")) != 1 || len(f.CallsNamed("recordUsage")) != 1 {
		t.Fatalf("calls: %+v", f.Calls)
	}
}

// A tools entry's error rides the usage report when the gateway's
// UsageReportInput takes it, and is stripped when it does not: the
// report lands first time either way, with the list, and never goes to
// the no-tools resend (ErrNoToolUsage). The schema is asked once.
func TestUsageReportToolErrorFollowsTheSchema(t *testing.T) {
	for _, tc := range []struct {
		name        string
		noToolError bool
		wantError   bool
	}{
		{"takes error", false, true},
		{"no error field", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			f.NoToolError = tc.noToolError
			runningItem(f)
			gw := gateway.New(f.URL(), "rs_test")
			for range 2 {
				u := sampleUsageReport()
				u.Tools[0].Error = "tool call failed: 401 Unauthorized"
				if err := gw.EvaluateWorkItem(context.Background(), finishItem, true, "looks good", 30, u); err != nil {
					t.Fatalf("err = %v, want nil (no ErrNoToolUsage)", err)
				}
			}
			calls := f.CallsNamed("evaluateWorkItem")
			if len(calls) != 2 {
				t.Fatalf("calls = %+v, want each landed first time", calls)
			}
			for _, c := range calls {
				usage, _ := c.Input["usage"].(map[string]any)
				assertToolsRow(t, usage)
				row := usage["tools"].([]any)[0].(map[string]any)
				got, has := row["error"]
				if has != tc.wantError || (has && got != "tool call failed: 401 Unauthorized") {
					t.Fatalf("tools row = %#v, want error sent: %v", row, tc.wantError)
				}
			}
			if n := len(f.ReadsNamed("__type UsageReportInput")); n != 1 {
				t.Fatalf("schema probed %d times, want once", n)
			}
		})
	}
}

// A report whose tools carry no error asks the schema nothing.
func TestUsageReportWithoutToolErrorProbesNothing(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	runningItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.EvaluateWorkItem(context.Background(), finishItem, true, "ok", 30, sampleUsageReport()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.ReadsNamed("__type UsageReportInput")); n != 0 {
		t.Fatalf("schema probed %d times, want none", n)
	}
}
