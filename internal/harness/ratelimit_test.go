package harness

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The fixture's rate_limit_event reaches Result.RateLimit, both of its
// reset-time forms read.
func TestParseStreamRateLimit(t *testing.T) {
	f, err := os.Open("testdata/stream_rate_limit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before := time.Now()
	res := ParseStream(f, nil)
	if !res.Success || res.Text != "Done." {
		t.Fatalf("result = %+v", res)
	}
	rl := res.RateLimit
	if rl == nil {
		t.Fatal("no RateLimit from a stream with a rate_limit_event")
	}
	fiveReset := time.Unix(1790330400, 0).UTC()
	sevenReset := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	if rl.Status != "allowed" || rl.RateLimitType != "five_hour" || rl.OverageStatus != "rejected" || rl.IsUsingOverage {
		t.Errorf("status fields = %+v", rl)
	}
	if !rl.ResetsAt.Equal(fiveReset) {
		t.Errorf("ResetsAt = %v, want %v", rl.ResetsAt, fiveReset)
	}
	if rl.FiveHour.Utilization != 0.83 || !rl.FiveHour.ResetsAt.Equal(fiveReset) {
		t.Errorf("FiveHour = %+v, want 0.83 at %v", rl.FiveHour, fiveReset)
	}
	if rl.SevenDay.Utilization != 0.41 || !rl.SevenDay.ResetsAt.Equal(sevenReset) {
		t.Errorf("SevenDay = %+v, want 0.41 at %v", rl.SevenDay, sevenReset)
	}
	if rl.ObservedAt.Before(before) {
		t.Errorf("ObservedAt = %v, before the parse", rl.ObservedAt)
	}
}

// A stream without the event leaves RateLimit nil.
func TestParseStreamNoRateLimit(t *testing.T) {
	in := `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"result","subtype":"success","result":"ok","session_id":"s1"}
`
	if rl := ParseStream(strings.NewReader(in), nil).RateLimit; rl != nil {
		t.Fatalf("RateLimit = %+v, want nil", rl)
	}
}

// The latest of several events is the one kept.
func TestParseStreamRateLimitLatest(t *testing.T) {
	in := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.5}}}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","unifiedWindows":{"five_hour":{"utilization":0.9}}}}
{"type":"result","subtype":"success","result":"ok"}
`
	rl := ParseStream(strings.NewReader(in), nil).RateLimit
	if rl == nil || rl.Status != "allowed_warning" || rl.FiveHour.Utilization != 0.9 {
		t.Fatalf("RateLimit = %+v, want the second event", rl)
	}
}

// Utilization is a fraction (a percent is normalised); resetsAt is epoch
// seconds, epoch milliseconds, a numeric string or RFC3339.
func TestRateLimitForms(t *testing.T) {
	want := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	cases := []struct {
		name, util, reset string
		wantUtil          float64
		wantReset         time.Time
	}{
		{"fraction, seconds", `0.25`, `1790359200`, 0.25, want},
		{"percent, millis", `87`, `1790359200000`, 0.87, want},
		{"string percent, string seconds", `"95"`, `"1790359200"`, 0.95, want},
		{"full, RFC3339 offset", `1`, `"2026-09-26T04:00:00+10:00"`, 1, want},
		{"missing", `null`, `null`, 0, time.Time{}},
		{"garbage", `"lots"`, `"soon"`, 0, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":` + tc.util + `,"resetsAt":` + tc.reset + `}}}}`
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatal(err)
			}
			rl := ev.RateLimit(time.Now())
			if rl == nil {
				t.Fatal("nil RateLimit")
			}
			if rl.FiveHour.Utilization != tc.wantUtil {
				t.Errorf("utilization = %v, want %v", rl.FiveHour.Utilization, tc.wantUtil)
			}
			if !rl.FiveHour.ResetsAt.Equal(tc.wantReset) {
				t.Errorf("resetsAt = %v, want %v", rl.FiveHour.ResetsAt, tc.wantReset)
			}
			if rl.SevenDay != (RateLimitWindow{}) {
				t.Errorf("SevenDay = %+v, want zero", rl.SevenDay)
			}
		})
	}
}

// A running session reports its plan standing as soon as the event
// arrives, and the Result carries it too.
func TestClaudeCodeRunReportsRateLimit(t *testing.T) {
	fixture, err := os.ReadFile("testdata/stream_rate_limit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	h := &ClaudeCode{Binary: fakeClaude(t, "cat <<'JSONL'\n"+string(fixture)+"JSONL\n")}
	var got []Progress
	res, err := h.Run(context.Background(), Job{Kind: KindBuild, Workspace: t.TempDir(), Prompt: "build"}, func(p Progress) {
		got = append(got, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	var rl *RateLimit
	for _, p := range got {
		if p.RateLimit != nil {
			if p.Text != "" || p.CostUSD != 0 {
				t.Errorf("rate-limit progress carries more: %+v", p)
			}
			rl = p.RateLimit
			break
		}
	}
	if rl == nil {
		t.Fatalf("no Progress carried a RateLimit: %+v", got)
	}
	if rl.Status != "allowed" || rl.FiveHour.Utilization != 0.83 || rl.SevenDay.Utilization != 0.41 {
		t.Errorf("progress RateLimit = %+v", rl)
	}
	if res.RateLimit == nil || res.RateLimit.FiveHour.Utilization != 0.83 {
		t.Errorf("result RateLimit = %+v", res.RateLimit)
	}
}
