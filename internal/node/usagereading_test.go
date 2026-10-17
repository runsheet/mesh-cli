package node

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// usageFixture is a node.log: labelled records of three kinds, a legacy
// record paired by cost to its finished line, a legacy one nothing
// pairs, an unparsable one, records carrying contextBytes, one (s1)
// carrying documentsFetched, and records either side of 2026-09-28.
const usageFixture = `mesh node: 2026/09/27 23:59:59 item ship-it/1: usage build {"sessionId":"s0","inputTokens":999,"cacheReadTokens":0,"listCostCents":1}
mesh node: 2026/09/28 09:00:00 item ship-it/1: "Write the failing test" on acme/widgets from main
mesh node: 2026/09/28 09:00:00 item ship-it/1: context 1000 bytes in 3 files, 2 documents to fetch
mesh node: 2026/09/28 09:10:00 item ship-it/1: finished mesh/ship-it-1@abc12345 ($1.25)
mesh node: 2026/09/28 09:10:00 usage: turns=12 input/turn mean=8000 max=9000 largest=Bash:1200
mesh node: 2026/09/28 09:10:00 item ship-it/1: usage build {"sessionId":"s1","inputTokens":100000,"cacheReadTokens":90000,"listCostCents":125,"contextBytes":1000,"documentsFetched":4}
mesh node: 2026/09/28 10:00:00 item ship-it/2: finished mesh/ship-it-2@def45678 ($0.50)
mesh node: 2026/09/28 10:00:00 usage: turns=30 input/turn mean=10000 max=20000 largest=
mesh node: 2026/09/28 10:00:01 usage: {"sessionId":"s2","inputTokens":300000,"cacheReadTokens":270000,"listCostCents":50}
mesh node: 2026/09/28 11:00:00 item ship-it/1: evaluation passed on Claude (opus) ($0.40)
mesh node: 2026/09/28 11:00:00 item ship-it/1: usage evaluate {"sessionId":"s3","inputTokens":50000,"cacheReadTokens":40000,"listCostCents":40}
mesh node: 2026/09/28 12:00:00 goal ship-it: planned, 3 items ($0.90)
mesh node: 2026/09/28 12:00:00 goal ship-it: usage plan {"sessionId":"s4","inputTokens":80000,"cacheReadTokens":60000,"listCostCents":90,"contextBytes":4000}
mesh node: 2026/09/28 13:00:00 goal other: usage plan {"sessionId":"s5","inputTokens":20000,"cacheReadTokens":0,"listCostCents":10,"contextBytes":2000}
mesh node: 2026/09/28 14:00:00 item ship-it/4: pushed 0badf00d; finished mesh/ship-it-4 ($0.76)
mesh node: 2026/09/28 14:00:01 usage: {"sessionId":"s6","inputTokens":7000,"cacheReadTokens":7000,"listCostCents":77}
mesh node: 2026/09/28 15:00:00 item ship-it/3: usage build {"sessionId":"s7","inputTo
mesh node: 2026/09/29 00:00:00 item ship-it/5: usage build {"sessionId":"s8","inputTokens":5000,"cacheReadTokens":1000,"listCostCents":5,"contextBytes":3000}
`

// testdata/usage-by-priority.log is a node.log of evaluate records: two
// logged before adr-016's amendment 2026-10-02 (no priority or
// evaluators), two urgent (one with a second opinion, one without),
// three high on two items (ship-it/4 retried, once on one evaluator
// and once on two), three normal (one with a second opinion) and one
// low. EvaluationCostByPriority groups them into one row per priority,
// unknown last, and Lines prints each as an `evaluate <priority>:` row
// under the kind table.
func TestEvaluationCostByPriority(t *testing.T) {
	f, err := os.Open("testdata/usage-by-priority.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	u, err := ReadUsage(f, UsageQuery{From: day("2026-09-27"), To: day("2026-09-29")})
	if err != nil {
		t.Fatal(err)
	}
	got := u.EvaluationCostByPriority()
	want := []PriorityCost{
		{Priority: "urgent", Items: 2, Evaluations: 2, CostTotalCents: 160, CostPerItemMedianCents: 80, CostPerItemMeanCents: 80, OneEvaluator: 1, TwoEvaluators: 1},
		{Priority: "high", Items: 2, Evaluations: 3, CostTotalCents: 230, CostPerItemMedianCents: 115, CostPerItemMeanCents: 115, OneEvaluator: 1, TwoEvaluators: 2},
		{Priority: "normal", Items: 3, Evaluations: 3, CostTotalCents: 154, CostPerItemMedianCents: 34, CostPerItemMeanCents: 51, OneEvaluator: 2, TwoEvaluators: 1},
		{Priority: "low", Items: 1, Evaluations: 1, CostTotalCents: 20, CostPerItemMedianCents: 20, CostPerItemMeanCents: 20, OneEvaluator: 1},
		{Priority: "", Items: 2, Evaluations: 2, CostTotalCents: 160, CostPerItemMedianCents: 80, CostPerItemMeanCents: 80},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("EvaluationCostByPriority =\n%+v\nwant\n%+v", got, want)
	}
	checkLines(t, u.Lines(), []string{
		"window 2026-09-27..2026-09-29  records 2026-09-27T10:00:00..2026-09-28T09:25:00",
		"kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool  tool calls  tools called",
		"evaluate  11        1300                 1554               50.0          -                     0                   -                         0                0           -",
		"evaluate urgent: 2 items, 1×1 / 1×2, median $0.80/item, total $1.60",
		"evaluate high: 2 items, 1×1 / 2×2, median $1.15/item, total $2.30",
		"evaluate normal: 3 items, 2×1 / 1×2, median $0.34/item, total $1.54",
		"evaluate low: 1 items, 1×1 / 0×2, median $0.20/item, total $0.20",
		"evaluate unknown: 2 items, 0×1 / 0×2, median $0.80/item, total $1.60",
		"unparsed 0",
	})
}

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
		"kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool  tool calls  tools called",
		"build     2         200000               200000             90.0          1000                  1                   4                         1                0           -",
		"evaluate  1         50000                50000              80.0          -                     0                   -                         0                0           -",
		"plan      2         50000                50000              60.0          3000                  2                   -                         0                0           -",
		"unknown   1         7000                 7000               100.0         -                     0                   -                         0                0           -",
		"evaluate unknown: 1 items, 0×1 / 0×2, median $0.40/item, total $0.40",
		"unparsed 1",
	})
}

// The window is [from, to): a record the second before it or at `to`
// is out.
func TestUsageReadingWindowExcludes(t *testing.T) {
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-29"), To: day("2026-09-30")}), []string{
		"window 2026-09-29..2026-09-30  records 2026-09-29T00:00:00..2026-09-29T00:00:00",
		"kind   sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool  tool calls  tools called",
		"build  1         5000                 5000               20.0          3000                  1                   -                         0                0           -",
		"unparsed 0",
	})
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-26"), To: day("2026-09-27")}), []string{
		"window 2026-09-26..2026-09-27  records -",
		"kind  sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool  tool calls  tools called",
		"unparsed 0",
	})
}

// --goal keeps the records whose label names the goal — its items' and
// its plan's; --kind one kind.
func TestUsageReadingByGoalAndKind(t *testing.T) {
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-28"), To: day("2026-09-29"), Goal: "ship-it"}), []string{
		"window 2026-09-28..2026-09-29  records 2026-09-28T09:10:00..2026-09-28T12:00:00",
		"kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool  tool calls  tools called",
		"build     2         200000               200000             90.0          1000                  1                   4                         1                0           -",
		"evaluate  1         50000                50000              80.0          -                     0                   -                         0                0           -",
		"plan      1         80000                80000              75.0          4000                  1                   -                         0                0           -",
		"evaluate unknown: 1 items, 0×1 / 0×2, median $0.40/item, total $0.40",
		"unparsed 1",
	})
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-28"), To: day("2026-09-29"), Kind: "plan"}), []string{
		"window 2026-09-28..2026-09-29  records 2026-09-28T12:00:00..2026-09-28T13:00:00",
		"kind  sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool  tool calls  tools called",
		"plan  2         50000                50000              60.0          3000                  2                   -                         0                0           -",
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

// Research's tool use reads from node.log: `tool calls` sums every
// session's calls to every tool but the built-in, and `tools called`
// names the distinct ones that were called — Brave's searches without
// the console (ADR-006's amendment 2026-10-03). A tool served and never
// called adds nothing to either.
func TestUsageReadingCountsToolCallsPerKind(t *testing.T) {
	const log = `mesh node: 2026/10/03 02:00:00 project runsheet/scout: usage research {"sessionId":"r1","inputTokens":10000,"cacheReadTokens":5000,"listCostCents":30,"tools":[{"tool":"brave","calls":3,"errors":1,"ms":1500},{"tool":"runsheet","calls":4,"errors":0,"ms":200},{"tool":"perplexity","calls":0,"errors":0,"ms":0}]}
mesh node: 2026/10/03 03:00:00 project runsheet/scout: usage research {"sessionId":"r2","inputTokens":20000,"cacheReadTokens":5000,"listCostCents":40,"tools":[{"tool":"brave","calls":2,"errors":0,"ms":900},{"tool":"perplexity","calls":1,"errors":0,"ms":700},{"tool":"mesh-context","calls":2,"errors":0,"ms":100}]}
`
	u, err := ReadUsage(strings.NewReader(log), UsageQuery{From: day("2026-10-03"), To: day("2026-10-04")})
	if err != nil {
		t.Fatal(err)
	}
	checkLines(t, u.Lines(), []string{
		"window 2026-10-03..2026-10-04  records 2026-10-03T02:00:00..2026-10-03T03:00:00",
		"kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool  tool calls  tools called",
		"research  2         15000                15000              33.3          -                     0                   -                         0                6           brave,perplexity",
		"unparsed 0",
	})
}
