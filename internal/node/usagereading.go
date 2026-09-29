package node

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// UsageQuery is what `mesh node usage` reads out of node.log: the
// records logged in [From, To) — node.log's timestamps are read in
// From's location — and, when set, only those whose label's goal is
// Goal or whose kind is Kind (adr-016's amendment). Goals, when not
// nil, keeps only the records whose label's goal is one of them — a
// project's goals, which the command resolves — and a research record
// whose label is "project <Project>". Window is the window as the
// command was given it, printed by Lines when set.
type UsageQuery struct {
	From, To   time.Time
	Goal, Kind string
	Goals      []string
	Project    string
	Window     string
}

// keeps says whether a record or session end with this label and kind
// is in the query's filter (the window aside).
func (q UsageQuery) keeps(label, kind string) bool {
	if q.Kind != "" && kind != q.Kind {
		return false
	}
	goal := labelGoal(label)
	if q.Goal != "" && goal != q.Goal {
		return false
	}
	if q.Goals != nil {
		if goal != "" {
			return slices.Contains(q.Goals, goal)
		}
		return q.Project != "" && label == "project "+q.Project
	}
	return true
}

// UsageRecord is one session's usage record as node.log holds it: when
// it was logged, the session's label ("item <goal>/<seq>", "goal
// <slug>", "project <slug>"; empty for a legacy record nothing paired),
// its kind, and the report exactly as it was sent.
type UsageRecord struct {
	At     time.Time
	Label  string
	Kind   string
	Report gateway.UsageReport
}

// Goal is the goal the record's label names: an item's goal, a plan's
// goal; "" for research or an unlabelled record.
func (r UsageRecord) Goal() string { return labelGoal(r.Label) }

// labelGoal is the goal a node.log label names (UsageRecord.Goal).
func labelGoal(label string) string {
	switch {
	case strings.HasPrefix(label, "item "):
		ref := strings.TrimPrefix(label, "item ")
		if i := strings.LastIndex(ref, "/"); i > 0 {
			return ref[:i]
		}
	case strings.HasPrefix(label, "goal "):
		return strings.TrimPrefix(label, "goal ")
	}
	return ""
}

// UsageReading is the records in the query's window, the count of
// record lines whose JSON did not parse, and Ended: the labelled
// session-end lines of each kind in the same window and filter — the
// sessions the records are a sample of.
type UsageReading struct {
	Query    UsageQuery
	Records  []UsageRecord
	Unparsed int
	Ended    map[string]int
}

// usagePairWithin is how many lines away a legacy `usage: {…}` record
// looks for the labelled session-end line it belongs to.
const usagePairWithin = 12

// UsageUnknown is the kind of a legacy record no end line pairs.
const UsageUnknown = "unknown"

var (
	// logStamp is log.LstdFlags' stamp, after node.log's "mesh node: "
	// prefix (optional here).
	logStamp = regexp.MustCompile(`^(?:mesh node: )?(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) (.*)$`)
	// usageRecordLine is Finish's `<label>: usage <kind> {…}`.
	usageRecordLine = regexp.MustCompile(`^(.+?): usage ([a-z]+) (\{.*)$`)
	// sessionEndLine is a labelled session's end with its cost:
	// finished (build), evaluation, rebased.
	sessionEndLine = regexp.MustCompile(`^(item \S+): (finished|evaluation|rebased) .*?\(\$(\d+\.\d\d)\)`)
	// planEndLine is the planner's hand-in: `goal <slug>: planned, <n>
	// items ($x)`.
	planEndLine = regexp.MustCompile(`^(goal \S+): planned, \d+ items \(\$\d+\.\d\d\)`)
	// researchEndLine is the director's hand-in, `<project>/<direction>:
	// pitched <n>, answered <m> ($x)` or `… proposed <n> goal(s) ($x)`;
	// its usage record's label is "project <project>".
	researchEndLine = regexp.MustCompile(`^([^\s/]+)/\S+: (?:pitched \d+, answered \d+|proposed \d+ goal\(s\)) \(\$\d+\.\d\d\)`)
)

// sessionEnded is the label and kind of a labelled session-end line —
// build, evaluate and rebase by sessionEndLine, plan by planEndLine,
// research by researchEndLine — and false for any other line.
func sessionEnded(msg string) (string, string, bool) {
	if m := sessionEndLine.FindStringSubmatch(msg); m != nil {
		return m[1], sessionEndKind[m[2]], true
	}
	if m := planEndLine.FindStringSubmatch(msg); m != nil {
		return m[1], "plan", true
	}
	if m := researchEndLine.FindStringSubmatch(msg); m != nil {
		return "project " + m[1], "research", true
	}
	return "", "", false
}

var sessionEndKind = map[string]string{"finished": "build", "evaluation": "evaluate", "rebased": "rebase"}

type logLine struct {
	at  time.Time
	msg string
	ok  bool
}

// ReadUsage reads node.log's usage records for q: each
// `<label>: usage <kind> {…}` line, and each legacy `usage: {…}` line
// an older binary wrote, which takes the label and kind of the nearest
// labelled end line — `finished …($x)`, `evaluation …($x)`, `rebased
// …($x)` — within 12 lines whose $x is its listCostCents to the cent,
// else kind unknown and no label. A record in the window whose JSON
// does not parse is counted (Unparsed) and skipped, whatever its label
// or kind. Each labelled session-end line in the window that the
// query's filter keeps is counted in Ended by its kind.
func ReadUsage(r io.Reader, q UsageQuery) (UsageReading, error) {
	loc := q.From.Location()
	var lines []logLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		m := logStamp.FindStringSubmatch(sc.Text())
		if m == nil {
			lines = append(lines, logLine{})
			continue
		}
		at, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], loc)
		if err != nil {
			lines = append(lines, logLine{})
			continue
		}
		lines = append(lines, logLine{at: at, msg: m[2], ok: true})
	}
	if err := sc.Err(); err != nil {
		return UsageReading{}, err
	}
	out := UsageReading{Query: q, Ended: map[string]int{}}
	for i, l := range lines {
		if !l.ok || l.at.Before(q.From) || !l.at.Before(q.To) {
			continue
		}
		if label, kind, ok := sessionEnded(l.msg); ok {
			if q.keeps(label, kind) {
				out.Ended[kind]++
			}
			continue
		}
		var rec UsageRecord
		var js string
		legacy := false
		if rest, ok := strings.CutPrefix(l.msg, "usage: {"); ok {
			js, legacy = "{"+rest, true
		} else if m := usageRecordLine.FindStringSubmatch(l.msg); m != nil {
			rec.Label, rec.Kind, js = m[1], m[2], m[3]
		} else {
			continue
		}
		if err := json.Unmarshal([]byte(js), &rec.Report); err != nil {
			out.Unparsed++
			continue
		}
		rec.At = l.at
		if legacy {
			rec.Label, rec.Kind = pairLegacy(lines, i, rec.Report.ListCostCents)
		}
		if !q.keeps(rec.Label, rec.Kind) {
			continue
		}
		out.Records = append(out.Records, rec)
	}
	return out, nil
}

// pairLegacy is the label and kind of the labelled session-end line
// nearest line i (within usagePairWithin, the earlier on a tie) whose
// cost is cents to the cent; "" and UsageUnknown when none is.
func pairLegacy(lines []logLine, i int, cents int64) (string, string) {
	for d := 1; d <= usagePairWithin; d++ {
		for _, j := range []int{i - d, i + d} {
			if j < 0 || j >= len(lines) || !lines[j].ok {
				continue
			}
			m := sessionEndLine.FindStringSubmatch(lines[j].msg)
			if m == nil {
				continue
			}
			usd, err := strconv.ParseFloat(m[3], 64)
			if err != nil || int64(Cents(usd)) != cents {
				continue
			}
			return m[1], sessionEndKind[m[2]]
		}
	}
	return "", UsageUnknown
}

// usageKindOrder is the order of the reading's rows: the five session
// kinds, then any other alphabetically, unknown last.
var usageKindOrder = []string{"build", "evaluate", "rebase", "plan", "research"}

// Lines is the reading as `mesh node usage` prints it: the window and
// the span of the records in it, one row per kind — sessions, the
// median and mean input tokens per session (the report's inputTokens,
// its cache reads and writes among them), the share of those that were
// cache reads, and the median context bytes over the sessions that
// carry them, `-` when none does — then the coverage of each kind in
// either count but unknown, `coverage <kind>: <n> records of <m>
// sessions ended in the window`, and the count of records that did not
// parse. The window is the query's Window when set, else its dates.
func (u UsageReading) Lines() []string {
	span := "-"
	if n := len(u.Records); n > 0 {
		span = u.Records[0].At.Format("2006-01-02T15:04:05") + ".." + u.Records[n-1].At.Format("2006-01-02T15:04:05")
	}
	window := u.Query.Window
	if window == "" {
		window = u.Query.From.Format("2006-01-02") + ".." + u.Query.To.Format("2006-01-02")
	}
	lines := []string{fmt.Sprintf("window %s  records %s", window, span)}
	byKind := map[string][]UsageRecord{}
	for _, r := range u.Records {
		byKind[r.Kind] = append(byKind[r.Kind], r)
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "kind\tsessions\tinput tokens median\tinput tokens mean\tcache read %\tcontext bytes median\twith context bytes")
	for _, k := range sortKinds(slices.Collect(maps.Keys(byKind))) {
		recs := byKind[k]
		var inputs, ctx []int64
		var total, cached int64
		for _, r := range recs {
			// the report's inputTokens already holds its cache reads and writes
			in := r.Report.InputTokens
			inputs = append(inputs, in)
			total += in
			cached += r.Report.CacheReadTokens
			if r.Report.ContextBytes > 0 {
				ctx = append(ctx, r.Report.ContextBytes)
			}
		}
		share, ctxMedian := "-", "-"
		if total > 0 {
			share = strconv.FormatFloat(float64(cached)*100/float64(total), 'f', 1, 64)
		}
		if len(ctx) > 0 {
			ctxMedian = strconv.FormatInt(median(ctx), 10)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\t%d\n", k, len(recs), median(inputs), total/int64(len(recs)), share, ctxMedian, len(ctx))
	}
	w.Flush()
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		lines = append(lines, strings.TrimRight(l, " "))
	}
	covered := map[string]bool{}
	for k := range byKind {
		covered[k] = true
	}
	for k := range u.Ended {
		covered[k] = true
	}
	delete(covered, UsageUnknown)
	for _, k := range sortKinds(slices.Collect(maps.Keys(covered))) {
		lines = append(lines, fmt.Sprintf("coverage %s: %d records of %d sessions ended in the window", k, len(byKind[k]), u.Ended[k]))
	}
	return append(lines, fmt.Sprintf("unparsed %d", u.Unparsed))
}

// sortKinds orders kinds as the reading's rows are: the five session
// kinds, then any other alphabetically, unknown last.
func sortKinds(kinds []string) []string {
	rank := func(k string) int {
		if i := slices.Index(usageKindOrder, k); i >= 0 {
			return i
		}
		if k == UsageUnknown {
			return len(usageKindOrder) + 1
		}
		return len(usageKindOrder)
	}
	slices.SortFunc(kinds, func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	return kinds
}

// median is the middle of vs (the mean of the two middle ones, rounded
// down, for an even count); 0 for none.
func median(vs []int64) int64 {
	if len(vs) == 0 {
		return 0
	}
	s := slices.Clone(vs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
