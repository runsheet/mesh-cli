package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/node"
)

var (
	nodeUsageLog     string
	nodeUsageFrom    string
	nodeUsageTo      string
	nodeUsageGoal    string
	nodeUsageKind    string
	nodeUsageProject string
	nodeUsageGoals   string
)

// nodeUsageCmd is the node-side reading of what sessions cost
// (adr-016's amendment): node.ReadUsage over node.log's usage records,
// printed as node.UsageReading.Lines. The platform exposes no usage
// query, so this is the reading a goal doc quotes.
var nodeUsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Read node.log's usage records: sessions, input tokens, cache reads and context bytes by kind, for a window",
	Long: `Reads every session's usage record in node.log — ` + "`<label>: usage <kind> {…}`" + `,
and the unlabelled ` + "`usage: {…}`" + ` an older node wrote, paired by cost to the
session's end line — logged in [--from, --to), and prints one row per kind:
sessions, median and mean input tokens, the cache-read share, and the median
context bytes over the sessions that carry them; then, for each kind, its
coverage: the records read beside the sessions node.log says ended in the
same window and filter. --from and --to are dates (local midnight, as
node.log's timestamps are local) or RFC 3339 timestamps, so a window can
start at a release's first line. --project org/slug keeps the project's
goals, read from the platform, and its research; --goals <file> keeps the
goals the file lists (one slug a line, # comments), offline.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if nodeUsageProject != "" && nodeUsageGoals != "" {
			return errors.New("--project and --goals both name the goals to read: give one")
		}
		from, fromTimed, err := parseUsageTime(nodeUsageFrom)
		if err != nil {
			return fmt.Errorf("--from: %w", err)
		}
		to, toTimed, err := parseUsageTime(nodeUsageTo)
		if err != nil {
			return fmt.Errorf("--to: %w", err)
		}
		if !to.After(from) {
			return fmt.Errorf("--to %s is not after --from %s", nodeUsageTo, nodeUsageFrom)
		}
		q := node.UsageQuery{From: from, To: to, Goal: nodeUsageGoal, Kind: nodeUsageKind}
		if fromTimed || toTimed {
			q.Window = nodeUsageFrom + ".." + nodeUsageTo
		}
		switch {
		case nodeUsageProject != "":
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()
			q.Project, q.Goals, err = projectGoals(ctx, nodeUsageProject)
			if err != nil {
				return fmt.Errorf("--project %s: %w", nodeUsageProject, err)
			}
		case nodeUsageGoals != "":
			if q.Goals, err = readGoalList(nodeUsageGoals); err != nil {
				return fmt.Errorf("--goals: %w", err)
			}
		}
		path := nodeUsageLog
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			path = node.ServiceLog(home)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		u, err := node.ReadUsage(f, q)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, l := range u.Lines() {
			fmt.Fprintln(cmd.OutOrStdout(), l)
		}
		return nil
	},
}

// parseUsageTime reads --from or --to: YYYY-MM-DD is local midnight, an
// RFC 3339 timestamp (or one with no offset, local) its instant; timed
// says it had a time part. The result is local, since ReadUsage reads
// node.log's local stamps in From's location.
func parseUsageTime(s string) (t time.Time, timed bool, err error) {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, false, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(time.Local), true, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("want YYYY-MM-DD or RFC 3339, got %q", s)
}

// projectGoals resolves --project org/slug on the platform: the
// project's slug and the slugs of its goals.
func projectGoals(ctx context.Context, ref string) (string, []string, error) {
	login, slug, ok := strings.Cut(ref, "/")
	if !ok || login == "" || slug == "" {
		return "", nil, errors.New("want org/slug")
	}
	gw, err := platformClient()
	if err != nil {
		return "", nil, err
	}
	org, err := gw.OrgByLogin(ctx, login)
	if err != nil {
		return "", nil, err
	}
	projects, err := gw.Projects(ctx, org.ID)
	if err != nil {
		return "", nil, err
	}
	projectID := ""
	for _, p := range projects {
		if p.Slug == slug {
			projectID = p.ID
		}
	}
	if projectID == "" {
		return "", nil, fmt.Errorf("no project %q in %s", slug, org.Login)
	}
	goals, err := gw.Goals(ctx, org.ID)
	if err != nil {
		return "", nil, err
	}
	slugs := []string{}
	for _, g := range goals {
		if g.ProjectID == projectID {
			slugs = append(slugs, g.Slug)
		}
	}
	return slug, slugs, nil
}

// readGoalList reads --goals: one goal slug a line, blank lines and #
// comments skipped.
func readGoalList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	slugs := []string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if line = strings.TrimSpace(line); line != "" {
			slugs = append(slugs, line)
		}
	}
	return slugs, sc.Err()
}

func init() {
	nodeUsageCmd.Flags().StringVar(&nodeUsageLog, "log", "", "the node log to read (default ~/.mesh/node.log)")
	nodeUsageCmd.Flags().StringVar(&nodeUsageFrom, "from", "", "the window's start, YYYY-MM-DD or RFC 3339 (required)")
	nodeUsageCmd.Flags().StringVar(&nodeUsageTo, "to", "", "the window's end, exclusive, YYYY-MM-DD or RFC 3339 (required)")
	nodeUsageCmd.Flags().StringVar(&nodeUsageGoal, "goal", "", "only records whose label names this goal's slug (its items' and its plan's)")
	nodeUsageCmd.Flags().StringVar(&nodeUsageKind, "kind", "", "only records of this kind: build, evaluate, rebase, plan, research or unknown")
	nodeUsageCmd.Flags().StringVar(&nodeUsageProject, "project", "", "only this project's goals, org/slug, read from the platform, and its research")
	nodeUsageCmd.Flags().StringVar(&nodeUsageGoals, "goals", "", "only the goals this file lists, one slug a line, # comments (offline; not with --project)")
	_ = nodeUsageCmd.MarkFlagRequired("from")
	_ = nodeUsageCmd.MarkFlagRequired("to")
	nodeCmd.AddCommand(nodeUsageCmd)
}
