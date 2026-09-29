package node

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// A report its finishing command carried (handedIn) is still logged,
// once, on the session's label with its kind: a build, an evaluation
// and a plan each give one `<label>: usage <kind> {…}` line whose JSON
// is the report as sent.
func TestFinishLogsTheCarriedRecordOnItsLabel(t *testing.T) {
	for _, tc := range []struct {
		label, kind, want string
	}{
		{"item ship-it/1", "build", "item ship-it/1: usage build "},
		{"item ship-it/1", "evaluate", "item ship-it/1: usage evaluate "},
		{"goal ship-it", "plan", "goal ship-it: usage plan "},
	} {
		var logs strings.Builder
		t0 := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
		c := &Credentials{Log: log.New(&logs, "", 0), Now: func() time.Time { return t0 }}
		s := &SessionAccount{creds: c, account: AccountConfig{Label: "max-a", Kind: AccountSubscription}, started: t0}
		s.SetContextBytes(4096)
		s.SetLabel(tc.label, tc.kind)
		s.Report(harness.Result{SessionID: "s-1", CostUSD: 1.25, Usage: &harness.Usage{Model: "claude-opus-5", InputTokens: 100000, CachedInputTokens: 90000, OutputTokens: 700}})
		if err := s.Carried(nil); err != nil {
			t.Fatal(err)
		}
		if !s.handedIn {
			t.Fatal("the report did not ride its finish")
		}
		s.Finish(context.Background(), harness.Result{})
		var records []string
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, ": usage "+tc.kind+" ") || strings.HasPrefix(l, "usage: {") {
				records = append(records, l)
			}
		}
		if len(records) != 1 || !strings.HasPrefix(records[0], tc.want) {
			t.Fatalf("%s: records %q, want one starting %q:\n%s", tc.kind, records, tc.want, logs.String())
		}
		want := `{"sessionId":"s-1","adapter":"claude-code","model":"claude-opus-5","credentialKind":"subscription","inputTokens":100000,"outputTokens":700,"cacheReadTokens":90000,"cacheWriteTokens":0,"toolCalls":0,"wallMs":0,"listCostCents":125,"actualCostCents":0,"contextBytes":4096}`
		if got := strings.TrimPrefix(records[0], tc.want); got != want {
			t.Errorf("%s: record\n%s\nwant\n%s", tc.kind, got, want)
		}
		if !strings.HasPrefix(logs.String(), "usage: turns=0 input/turn mean=0 max=0 largest=\n") {
			t.Errorf("%s: the turns line changed:\n%s", tc.kind, logs.String())
		}
	}
}

// A session with no label (SetLabel never called) keeps the older
// `usage: {…}` line.
func TestFinishWithoutALabelLogsTheOlderLine(t *testing.T) {
	var logs strings.Builder
	c := &Credentials{Log: log.New(&logs, "", 0)}
	s := &SessionAccount{creds: c}
	s.Report(harness.Result{SessionID: "s-1"})
	s.Finish(context.Background(), harness.Result{})
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], `usage: {"sessionId":"s-1",`) {
		t.Fatalf("log:\n%s", logs.String())
	}
}

// usageFixture is a node.log: labelled records of three kinds, a legacy
// record paired by cost to its finished line, a legacy one nothing
// pairs, an unparsable one, records carrying contextBytes, and records
// either side of 2026-09-28.
const usageFixture = `mesh node: 2026/09/27 23:59:59 item ship-it/1: usage build {"sessionId":"s0","inputTokens":999,"cacheReadTokens":0,"listCostCents":1}
mesh node: 2026/09/28 09:00:00 item ship-it/1: "Write the failing test" on acme/widgets from main
mesh node: 2026/09/28 09:00:00 item ship-it/1: context 1000 bytes in 3 files, 2 documents to fetch
mesh node: 2026/09/28 09:10:00 item ship-it/1: finished mesh/ship-it-1@abc12345 ($1.25)
mesh node: 2026/09/28 09:10:00 usage: turns=12 input/turn mean=8000 max=9000 largest=Bash:1200
mesh node: 2026/09/28 09:10:00 item ship-it/1: usage build {"sessionId":"s1","inputTokens":100000,"cacheReadTokens":90000,"listCostCents":125,"contextBytes":1000}
mesh node: 2026/09/28 10:00:00 item ship-it/2: finished mesh/ship-it-2@def45678 ($0.50)
mesh node: 2026/09/28 10:00:00 usage: turns=30 input/turn mean=10000 max=20000 largest=
mesh node: 2026/09/28 10:00:01 usage: {"sessionId":"s2","inputTokens":300000,"cacheReadTokens":270000,"listCostCents":50}
mesh node: 2026/09/28 11:00:00 item ship-it/1: evaluation passed on Claude (opus) ($0.40)
mesh node: 2026/09/28 11:00:00 item ship-it/1: usage evaluate {"sessionId":"s3","inputTokens":50000,"cacheReadTokens":40000,"listCostCents":40}
mesh node: 2026/09/28 12:00:00 goal ship-it: planned, 3 items ($0.90)
mesh node: 2026/09/28 12:00:00 goal ship-it: usage plan {"sessionId":"s4","inputTokens":80000,"cacheReadTokens":60000,"listCostCents":90,"contextBytes":4000}
mesh node: 2026/09/28 13:00:00 goal other: usage plan {"sessionId":"s5","inputTokens":20000,"cacheReadTokens":0,"listCostCents":10,"contextBytes":2000}
mesh node: 2026/09/28 14:00:00 item ship-it/4: finished mesh/ship-it-4@0badf00d ($0.76)
mesh node: 2026/09/28 14:00:01 usage: {"sessionId":"s6","inputTokens":7000,"cacheReadTokens":7000,"listCostCents":77}
mesh node: 2026/09/28 15:00:00 item ship-it/3: usage build {"sessionId":"s7","inputTo
mesh node: 2026/09/29 00:00:00 item ship-it/5: usage build {"sessionId":"s8","inputTokens":5000,"cacheReadTokens":1000,"listCostCents":5,"contextBytes":3000}
`

func readUsageFixture(t *testing.T, q UsageQuery) []string {
	t.Helper()
	u, err := ReadUsage(strings.NewReader(usageFixture), q)
	if err != nil {
		t.Fatal(err)
	}
	return u.Lines()
}

func day(s string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return d
}

func checkLines(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The reading of a day: one row per kind — the legacy record paired by
// its cost to `finished …($0.50)` counted a build, the one nothing
// pairs (its $0.77 is no end line's) unknown — and the unparsable
// record counted, not read.
func TestUsageReadingByKind(t *testing.T) {
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-28"), To: day("2026-09-29")}), []string{
		"window 2026-09-28..2026-09-29  records 2026-09-28T09:10:00..2026-09-28T14:00:01",
		"kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes",
		"build     2         200000               200000             90.0          1000                  1",
		"evaluate  1         50000                50000              80.0          -                     0",
		"plan      2         50000                50000              60.0          3000                  2",
		"unknown   1         7000                 7000               100.0         -                     0",
		"unparsed 1",
	})
}

// The window is [from, to): a record the second before it or at `to`
// is out.
func TestUsageReadingWindowExcludes(t *testing.T) {
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-29"), To: day("2026-09-30")}), []string{
		"window 2026-09-29..2026-09-30  records 2026-09-29T00:00:00..2026-09-29T00:00:00",
		"kind   sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes",
		"build  1         5000                 5000               20.0          3000                  1",
		"unparsed 0",
	})
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-26"), To: day("2026-09-27")}), []string{
		"window 2026-09-26..2026-09-27  records -",
		"kind  sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes",
		"unparsed 0",
	})
}

// --goal keeps the records whose label names the goal — its items' and
// its plan's; --kind one kind.
func TestUsageReadingByGoalAndKind(t *testing.T) {
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-28"), To: day("2026-09-29"), Goal: "ship-it"}), []string{
		"window 2026-09-28..2026-09-29  records 2026-09-28T09:10:00..2026-09-28T12:00:00",
		"kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes",
		"build     2         200000               200000             90.0          1000                  1",
		"evaluate  1         50000                50000              80.0          -                     0",
		"plan      1         80000                80000              75.0          4000                  1",
		"unparsed 1",
	})
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-28"), To: day("2026-09-29"), Kind: "plan"}), []string{
		"window 2026-09-28..2026-09-29  records 2026-09-28T12:00:00..2026-09-28T13:00:00",
		"kind  sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes",
		"plan  2         50000                50000              60.0          3000                  2",
		"unparsed 1",
	})
}

// A legacy record paired to its end line takes that line's label, so
// --goal finds it.
func TestUsageReadingPairsLegacyByCost(t *testing.T) {
	u, err := ReadUsage(strings.NewReader(usageFixture), UsageQuery{From: day("2026-09-28"), To: day("2026-09-29")})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range u.Records {
		got = append(got, r.Report.SessionID+" "+r.Kind+" "+r.Label)
	}
	want := []string{"s1 build item ship-it/1", "s2 build item ship-it/2", "s3 evaluate item ship-it/1", "s4 plan goal ship-it", "s5 plan goal other", "s6 unknown "}
	if !slices.Equal(got, want) {
		t.Errorf("records %q, want %q", got, want)
	}
	if b, _ := json.Marshal(u.Records[1].Report); !strings.Contains(string(b), `"listCostCents":50`) {
		t.Errorf("s2 = %s", b)
	}
}
