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
		ToolCalls: 3, WallMs: 45000, ListCostCents: 120, ActualCostCents: 0,
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
