package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
)

var (
	goalOrgFlag    string
	goalReasonFlag string
	goalWatchEvery time.Duration
)

var goalCmd = &cobra.Command{
	Use:   "goal",
	Short: "Set, plan, approve and watch goals on the platform",
}

// goalFile is `mesh goal set -f goal.yaml`.
type goalFile struct {
	Slug             string   `yaml:"slug"`
	Title            string   `yaml:"title"`
	Statement        string   `yaml:"statement"`
	Acceptance       string   `yaml:"acceptance"`
	BudgetCents      int      `yaml:"budget_cents"`
	DailyBudgetCents int      `yaml:"daily_budget_cents"`
	Checkpoints      []string `yaml:"checkpoints"`
	Project          string   `yaml:"project"`
}

// planFile is `mesh goal plan <slug> -f plan.yaml`: `items:` or a bare list.
type planItem struct {
	Seq        int    `yaml:"seq"`
	Title      string `yaml:"title"`
	Brief      string `yaml:"brief"`
	Acceptance string `yaml:"acceptance"`
	DependsOn  []int  `yaml:"depends_on"`
	Repo       string `yaml:"repo"`
	BaseRef    string `yaml:"base_ref"`
	Model      string `yaml:"model"`
}

type goalScope struct {
	gw  *gateway.Client
	org *gateway.Org
	me  string
}

func goalContext(ctx context.Context) (*goalScope, error) {
	gw, err := platformClient()
	if err != nil {
		return nil, err
	}
	org, err := resolveOrg(ctx, gw, goalOrgFlag)
	if err != nil {
		return nil, err
	}
	return &goalScope{gw: gw, org: org}, nil
}

func (s *goalScope) signIn(ctx context.Context) string {
	if s.me == "" {
		s.me, _ = s.gw.SignIn(ctx)
	}
	return s.me
}

func (s *goalScope) goalID(slug string) (string, error) {
	orgID, err := uuid.Parse(s.org.ID)
	if err != nil {
		return "", err
	}
	return gateway.GoalID(orgID, strings.TrimSpace(slug)).String(), nil
}

func readYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, out)
}

var goalSetCmd = &cobra.Command{
	Use:   "set <goal.yaml>",
	Short: "Create or restate a goal from a YAML file (slug, title, statement, acceptance, budget_cents, daily_budget_cents, checkpoints, project)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var g goalFile
		if err := readYAML(args[0], &g); err != nil {
			return err
		}
		if g.Slug == "" || g.Title == "" || g.Project == "" {
			return errors.New("slug, title and project are required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s, err := goalContext(ctx)
		if err != nil {
			return err
		}
		projects, err := s.gw.Projects(ctx, s.org.ID)
		if err != nil {
			return err
		}
		projectID := ""
		for _, p := range projects {
			if p.Slug == g.Project {
				projectID = p.ID
			}
		}
		if projectID == "" {
			return fmt.Errorf("no project %q in %s", g.Project, s.org.Login)
		}
		id, err := s.goalID(g.Slug)
		if err != nil {
			return err
		}
		if err := s.gw.SetGoal(ctx, gateway.SetGoalInput{GoalID: id, OrgID: s.org.ID, ProjectID: projectID, Slug: g.Slug, Title: g.Title,
			Statement: g.Statement, Acceptance: g.Acceptance, BudgetCents: g.BudgetCents, DailyBudgetCents: g.DailyBudgetCents, Checkpoints: g.Checkpoints, By: s.signIn(ctx)}); err != nil {
			return err
		}
		fmt.Printf("Goal %s set (%s).\n", g.Slug, id)
		return nil
	},
}

var goalPlanCmd = &cobra.Command{
	Use:   "plan <slug> [plan.yaml]",
	Short: "Ask the planner for a goal's graph — or hand one in from a YAML file of items (seq, title, brief, acceptance, depends_on, repo, base_ref)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			s, err := goalContext(ctx)
			if err != nil {
				return err
			}
			id, err := s.goalID(args[0])
			if err != nil {
				return err
			}
			if err := s.gw.RequestPlan(ctx, id, s.signIn(ctx)); err != nil {
				return err
			}
			fmt.Printf("Goal %s is planning: a node's planner session will hand in the graph. Watch it with: mesh goal watch %s\n", args[0], args[0])
			return nil
		}
		var items []planItem
		var wrapped struct {
			Items []planItem `yaml:"items"`
		}
		if err := readYAML(args[1], &items); err != nil {
			if err2 := readYAML(args[1], &wrapped); err2 != nil {
				return err
			}
			items = wrapped.Items
		}
		if len(items) == 0 {
			return errors.New("no items in the plan")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s, err := goalContext(ctx)
		if err != nil {
			return err
		}
		repos, err := s.gw.Repos(ctx, s.org.AccountID, s.org.Login)
		if err != nil {
			return err
		}
		planned := make([]gateway.PlannedItem, 0, len(items))
		for _, it := range items {
			name := it.Repo
			if _, n, ok := strings.Cut(it.Repo, "/"); ok {
				name = n
			}
			repoID := ""
			for _, r := range repos {
				if strings.EqualFold(r.Name, name) {
					repoID = r.ID
				}
			}
			if repoID == "" {
				return fmt.Errorf("item %d: no repo %q under %s", it.Seq, it.Repo, s.org.Login)
			}
			deps := it.DependsOn
			if deps == nil {
				deps = []int{}
			}
			planned = append(planned, gateway.PlannedItem{Seq: it.Seq, Title: it.Title, Brief: it.Brief, Acceptance: it.Acceptance, DependsOn: deps, RepoID: repoID, BaseRef: it.BaseRef, Model: strings.TrimSpace(it.Model)})
		}
		id, err := s.goalID(args[0])
		if err != nil {
			return err
		}
		if err := s.gw.PlanGoal(ctx, id, s.signIn(ctx), planned, 0, nil); err != nil {
			return err
		}
		fmt.Printf("Goal %s planned with %d items.\n", args[0], len(planned))
		return nil
	},
}

func goalVerb(use, short string, run func(ctx context.Context, s *goalScope, id string) error) *cobra.Command {
	return &cobra.Command{
		Use: use + " <slug>", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			s, err := goalContext(ctx)
			if err != nil {
				return err
			}
			id, err := s.goalID(args[0])
			if err != nil {
				return err
			}
			if err := run(ctx, s, id); err != nil {
				return err
			}
			fmt.Printf("Goal %s: %s.\n", args[0], use)
			return nil
		},
	}
}

var (
	goalApproveCmd = goalVerb("approve", "Approve the goal's graph checkpoint", func(ctx context.Context, s *goalScope, id string) error {
		return s.gw.ApproveGraph(ctx, id, s.signIn(ctx))
	})
	goalPauseCmd = goalVerb("pause", "Pause the goal", func(ctx context.Context, s *goalScope, id string) error {
		return s.gw.PauseGoal(ctx, id, goalReasonFlag)
	})
	goalResumeCmd = goalVerb("resume", "Resume the goal", func(ctx context.Context, s *goalScope, id string) error {
		return s.gw.ResumeGoal(ctx, id)
	})
	goalCancelCmd = goalVerb("cancel", "Cancel the goal (running items are asked to stop)", func(ctx context.Context, s *goalScope, id string) error {
		return s.gw.CancelGoal(ctx, id, s.signIn(ctx))
	})
)

var goalListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the org's goals",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s, err := goalContext(ctx)
		if err != nil {
			return err
		}
		goals, err := s.gw.Goals(ctx, s.org.ID)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SLUG\tSTATUS\tSPENT\tBUDGET\tTITLE")
		for _, g := range goals {
			fmt.Fprintf(w, "%s\t%s\t$%.2f\t$%.2f\t%s\n", g.Slug, g.Status, float64(g.SpentCents)/100, float64(g.BudgetCents)/100, g.Title)
		}
		return w.Flush()
	},
}

var goalWatchCmd = &cobra.Command{
	Use:   "watch <slug>",
	Short: "Print the goal's items and every status change until interrupted",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		s, err := goalContext(ctx)
		if err != nil {
			return err
		}
		id, err := s.goalID(args[0])
		if err != nil {
			return err
		}
		last := map[string]string{}
		goalStatus := ""
		tick := func() error {
			g, err := s.gw.Goal(ctx, id)
			if err != nil {
				return err
			}
			if g.Status != goalStatus {
				goalStatus = g.Status
				fmt.Printf("%s goal %s: %s ($%.2f of $%.2f)\n", time.Now().Format("15:04:05"), g.Slug, g.Status, float64(g.SpentCents)/100, float64(g.BudgetCents)/100)
			}
			items, err := s.gw.GoalWorkItems(ctx, id)
			if err != nil {
				return err
			}
			for _, it := range items {
				line := it.Status
				if it.Branch != "" {
					line += " " + it.Branch
				}
				if it.PRURL != "" {
					line += " " + it.PRURL
				}
				if it.LastError != "" {
					line += " — " + it.LastError
				}
				if last[it.ID] != line {
					last[it.ID] = line
					fmt.Printf("%s   item %d %q: %s ($%.2f)\n", time.Now().Format("15:04:05"), it.Seq, it.Title, line, float64(it.CostCents)/100)
				}
			}
			return nil
		}
		if err := tick(); err != nil {
			return err
		}
		t := time.NewTicker(goalWatchEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				if err := tick(); err != nil && ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "watch: %v\n", err)
				}
			}
		}
	},
}

func init() {
	goalCmd.PersistentFlags().StringVar(&goalOrgFlag, "org", "", "the org's GitHub login (default: the registered node's)")
	goalCmd.PersistentFlags().StringVar(&gatewayFlag, "gateway", "", "platform gateway URL (default "+gateway.DefaultURL+", or MESH_GATEWAY)")
	goalPauseCmd.Flags().StringVar(&goalReasonFlag, "reason", "", "why")
	goalWatchCmd.Flags().DurationVar(&goalWatchEvery, "every", 5*time.Second, "poll interval")
	goalCmd.AddCommand(goalSetCmd, goalPlanCmd, goalApproveCmd, goalPauseCmd, goalResumeCmd, goalCancelCmd, goalListCmd, goalWatchCmd)
}
