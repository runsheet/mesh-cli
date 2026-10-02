package node

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
// Goal or whose kind is Kind (adr-016's amendment).
type UsageQuery struct {
	From, To   time.Time
	Goal, Kind string
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
func (r UsageRecord) Goal() string {
	switch {
	case strings.HasPrefix(r.Label, "item "):
		ref := strings.TrimPrefix(r.Label, "item ")
		if i := strings.LastIndex(ref, "/"); i > 0 {
			return ref[:i]
		}
	case strings.HasPrefix(r.Label, "goal "):
		return strings.TrimPrefix(r.Label, "goal ")
	}
	return ""
}

// UsageReading is the records in the query's window and the count of
// record lines whose JSON did not parse.
type UsageReading struct {
	Query    UsageQuery
	Records  []UsageRecord
	Unparsed int
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
)

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
// or kind.
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
	out := UsageReading{Query: q}
	for i, l := range lines {
		if !l.ok || l.at.Before(q.From) || !l.at.Before(q.To) {
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
		if q.Kind != "" && rec.Kind != q.Kind {
			continue
		}
		if q.Goal != "" && rec.Goal() != q.Goal {
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
// cache reads, the median context bytes over the sessions that carry
// them, `-` when none does, the median documents fetched by tool
// (Report.DocumentsFetched, goal context-is-fetched-not-shipped) over
// the sessions that fetched any, `-` when none did, and how many
// sessions those were (`fetched by tool`; the count is omitted from a
// record at zero, so a session that fetched none and one from a node
// older than the count read alike) — and the count of records that did
// not parse.
func (u UsageReading) Lines() []string {
	span := "-"
	if n := len(u.Records); n > 0 {
		span = u.Records[0].At.Format("2006-01-02T15:04:05") + ".." + u.Records[n-1].At.Format("2006-01-02T15:04:05")
	}
	lines := []string{fmt.Sprintf("window %s..%s  records %s", u.Query.From.Format("2006-01-02"), u.Query.To.Format("2006-01-02"), span)}
	byKind := map[string][]UsageRecord{}
	for _, r := range u.Records {
		byKind[r.Kind] = append(byKind[r.Kind], r)
	}
	var kinds []string
	for k := range byKind {
		kinds = append(kinds, k)
	}
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
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "kind\tsessions\tinput tokens median\tinput tokens mean\tcache read %\tcontext bytes median\twith context bytes\tdocuments fetched median\tfetched by tool")
	for _, k := range kinds {
		recs := byKind[k]
		var inputs, ctx, docs []int64
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
			if r.Report.DocumentsFetched > 0 {
				docs = append(docs, r.Report.DocumentsFetched)
			}
		}
		share, ctxMedian, docsMedian := "-", "-", "-"
		if total > 0 {
			share = strconv.FormatFloat(float64(cached)*100/float64(total), 'f', 1, 64)
		}
		if len(ctx) > 0 {
			ctxMedian = strconv.FormatInt(median(ctx), 10)
		}
		if len(docs) > 0 {
			docsMedian = strconv.FormatInt(median(docs), 10)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\t%d\t%s\t%d\n", k, len(recs), median(inputs), total/int64(len(recs)), share, ctxMedian, len(ctx), docsMedian, len(docs))
	}
	w.Flush()
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		lines = append(lines, strings.TrimRight(l, " "))
	}
	return append(lines, fmt.Sprintf("unparsed %d", u.Unparsed))
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
