package node

import (
	"encoding/json"
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
		"kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool",
		"build     2         200000               200000             90.0          1000                  1                   4                         1",
		"evaluate  1         50000                50000              80.0          -                     0                   -                         0",
		"plan      2         50000                50000              60.0          3000                  2                   -                         0",
		"unknown   1         7000                 7000               100.0         -                     0                   -                         0",
		"unparsed 1",
	})
}

// The window is [from, to): a record the second before it or at `to`
// is out.
func TestUsageReadingWindowExcludes(t *testing.T) {
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-29"), To: day("2026-09-30")}), []string{
		"window 2026-09-29..2026-09-30  records 2026-09-29T00:00:00..2026-09-29T00:00:00",
		"kind   sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool",
		"build  1         5000                 5000               20.0          3000                  1                   -                         0",
		"unparsed 0",
	})
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-26"), To: day("2026-09-27")}), []string{
		"window 2026-09-26..2026-09-27  records -",
		"kind  sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool",
		"unparsed 0",
	})
}

// --goal keeps the records whose label names the goal — its items' and
// its plan's; --kind one kind.
func TestUsageReadingByGoalAndKind(t *testing.T) {
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-28"), To: day("2026-09-29"), Goal: "ship-it"}), []string{
		"window 2026-09-28..2026-09-29  records 2026-09-28T09:10:00..2026-09-28T12:00:00",
		"kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool",
		"build     2         200000               200000             90.0          1000                  1                   4                         1",
		"evaluate  1         50000                50000              80.0          -                     0                   -                         0",
		"plan      1         80000                80000              75.0          4000                  1                   -                         0",
		"unparsed 1",
	})
	checkLines(t, readUsageFixture(t, UsageQuery{From: day("2026-09-28"), To: day("2026-09-29"), Kind: "plan"}), []string{
		"window 2026-09-28..2026-09-29  records 2026-09-28T12:00:00..2026-09-28T13:00:00",
		"kind  sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool",
		"plan  2         50000                50000              60.0          3000                  2                   -                         0",
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
