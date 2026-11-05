package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/nodev1"
	"projectmesh.io/mesh-cli/internal/paths"
)

// mesh node asks reads the v1 node's own asks.jsonl (nodev1/asks.go) —
// every request_time call, granted or refused, and each lease's end —
// and reduces it to counts by kind and by repo, plus how many of the
// jobs that got a grant actually finished: a quick read of how the
// time-budget ask is being used, without a platform query for it.

var nodeAsksLog string

var nodeAsksCmd = &cobra.Command{
	Use:   "asks",
	Short: "Report asks.jsonl's request_time calls, by kind and by repo",
	Long: `Reads the v1 node's asks.jsonl ledger — every request_time call, granted or
refused for any reason, and each lease's end once it is known — and prints,
per kind and per repo: how many asks, how many granted, how many refused.
Then, among the jobs that got at least one grant, how many finished.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := nodeAsksLog
		if path == "" {
			dir, err := paths.MeshDir()
			if err != nil {
				return err
			}
			path = filepath.Join(dir, "v1", "asks.jsonl")
		}
		records, err := readAskRecords(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		report := aggregateAsks(records)
		for _, l := range report.Lines() {
			fmt.Fprintln(cmd.OutOrStdout(), l)
		}
		return nil
	},
}

func init() {
	nodeAsksCmd.Flags().StringVar(&nodeAsksLog, "log", "", "the asks ledger to read (default ~/.mesh/v1/asks.jsonl)")
	nodeCmd.AddCommand(nodeAsksCmd)
}

// readAskRecords reads every line of path as a nodev1.AskRecord, oldest
// first; none when the file does not exist yet. A line that fails to
// parse is skipped rather than failing the read, as nodev1's own reader
// does: this is a best-effort report over a local ledger, not a read
// that has to trust every byte.
func readAskRecords(path string) ([]nodev1.AskRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []nodev1.AskRecord
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec nodev1.AskRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// AskTally is one kind's or one repo's counts over the asks ledger.
type AskTally struct {
	Name    string
	Asks    int
	Granted int
	Refused int
}

// AsksReport is asks.jsonl reduced to counts by kind and by repo, plus
// how many of the jobs that got a grant actually finished.
type AsksReport struct {
	ByKind []AskTally
	ByRepo []AskTally

	// GrantedJobs is every job+lease that got at least one grant;
	// GrantedFinished is how many of those ended with report "finish"
	// (actor.ReportFinish) — never a job that asked and was refused
	// every time, and never one with no end record at all (still
	// running, or cut off before the end line was written).
	GrantedJobs     int
	GrantedFinished int
}

// askJob joins an ask to the lease it came from, the same way
// nodev1's own ledger joins an "ask" line to its "end" line: by job id
// and lease id together.
type askJob struct{ jobID, leaseID string }

// aggregateAsks reduces every record asks.jsonl holds to the report:
// an "ask" line tallies its kind and repo and, when granted, marks its
// job; an "end" line records how that job's lease ended. The two kinds
// of line can arrive in either order (a lease's end is appended once
// its outcome is known, well after its asks), so ends are read in full
// before any job is judged finished or not.
func aggregateAsks(records []nodev1.AskRecord) AsksReport {
	kinds := map[string]*AskTally{}
	repos := map[string]*AskTally{}
	ends := map[askJob]string{}
	granted := map[askJob]struct{}{}

	tally := func(m map[string]*AskTally, name string) *AskTally {
		t, ok := m[name]
		if !ok {
			t = &AskTally{Name: name}
			m[name] = t
		}
		return t
	}

	for _, rec := range records {
		switch rec.Type {
		case "end":
			ends[askJob{rec.JobID, rec.LeaseID}] = rec.Report
		case "ask":
			k, r := tally(kinds, rec.Kind), tally(repos, rec.Repo)
			k.Asks++
			r.Asks++
			if rec.Granted {
				k.Granted++
				r.Granted++
				granted[askJob{rec.JobID, rec.LeaseID}] = struct{}{}
			} else {
				k.Refused++
				r.Refused++
			}
		}
	}

	report := AsksReport{ByKind: sortedTallies(kinds), ByRepo: sortedTallies(repos), GrantedJobs: len(granted)}
	for job := range granted {
		if ends[job] == actor.ReportFinish {
			report.GrantedFinished++
		}
	}
	return report
}

// sortedTallies returns m's values sorted by name, for a report whose
// rows read the same way every time.
func sortedTallies(m map[string]*AskTally) []AskTally {
	out := make([]AskTally, 0, len(m))
	for _, t := range m {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lines is the report as mesh node asks prints it.
func (r AsksReport) Lines() []string {
	if len(r.ByKind) == 0 {
		return []string{"asks: none recorded"}
	}
	var lines []string
	lines = append(lines, "by kind:")
	for _, t := range r.ByKind {
		lines = append(lines, fmt.Sprintf("  %s: %d ask(s), %d granted, %d refused", t.Name, t.Asks, t.Granted, t.Refused))
	}
	lines = append(lines, "by repo:")
	for _, t := range r.ByRepo {
		lines = append(lines, fmt.Sprintf("  %s: %d ask(s), %d granted, %d refused", t.Name, t.Asks, t.Granted, t.Refused))
	}
	lines = append(lines, fmt.Sprintf("granted: %d of %d job(s) finished", r.GrantedFinished, r.GrantedJobs))
	return lines
}
