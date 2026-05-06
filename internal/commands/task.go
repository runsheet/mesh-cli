package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/auth"
)

func printJSON(v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(body))
	return nil
}

var taskCmd = &cobra.Command{
	Use:     "task",
	Aliases: []string{"work", "wi"},
	Short:   "DAG-runtime task commands",
	Long:    "List, fetch, and create DAG-runtime tasks (Phase 1 SCH-12). Tasks are user-facing primitives that spawn a workflow_run + first step_run pending.",
}

var (
	createTaskOrg             string
	createTaskWorkflowID      string
	createTaskFirstStepInputs string
)

var createCmd = &cobra.Command{
	Use:     "create <title>",
	Short:   "Create a new DAG-runtime task",
	Long:    "Create a task as the signed-in user. The task spawns a workflow_run and a pending first step_run for the workflow's entry node.",
	Example: "mesh task create \"Investigate deploy flake\" --workflow-id <uuid>\nmesh task create \"Lead enrichment\" --workflow-id <uuid> --first-step-inputs '{\"company\":\"acme\"}'",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		client := newMeshClientForSession(sess)
		if cmd.Flags().Changed("org") {
			client.OrgSlug = createTaskOrg
		}
		title := strings.TrimSpace(args[0])
		if title == "" {
			return fmt.Errorf("title is required")
		}

		workflowID := strings.TrimSpace(createTaskWorkflowID)
		if workflowID == "" {
			return fmt.Errorf("--workflow-id is required")
		}

		// Parse --first-step-inputs as a JSON object. Default = empty object.
		firstStepInputs := map[string]any{}
		if v := strings.TrimSpace(createTaskFirstStepInputs); v != "" {
			if err := json.Unmarshal([]byte(v), &firstStepInputs); err != nil {
				return fmt.Errorf("--first-step-inputs must be a JSON object: %w", err)
			}
		}

		payload := map[string]any{
			"title":           title,
			"workflowId":      workflowID,
			"firstStepInputs": firstStepInputs,
		}

		var response map[string]any
		if err := client.doRequest("POST", "/task/create", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

func init() {
	createCmd.Flags().StringVar(&createTaskOrg, "org", "", "Org slug to scope the task (overrides session default)")
	createCmd.Flags().StringVar(&createTaskWorkflowID, "workflow-id", "", "Workflow UUID (required)")
	createCmd.Flags().StringVar(&createTaskFirstStepInputs, "first-step-inputs", "", "JSON object passed to the workflow's first step (default {})")
	_ = createCmd.MarkFlagRequired("workflow-id")

	taskCmd.AddCommand(listCmd, getTaskCmd, createCmd)
}

var (
	listStatusFlag    string
	listProjectIDFlag string
	listAllFlag       bool
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List DAG-runtime tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		q := ""
		if !listAllFlag {
			q = "assignedToMe=true"
		}
		if v := strings.TrimSpace(listStatusFlag); v != "" {
			if q != "" {
				q += "&"
			}
			q += "status=" + v
		}
		if v := strings.TrimSpace(listProjectIDFlag); v != "" {
			if q != "" {
				q += "&"
			}
			q += "projectId=" + v
		}
		path := "/task/list"
		if q != "" {
			path += "?" + q
		}

		client := newMeshClientForSession(sess)
		var resp struct {
			OK    bool `json:"ok"`
			Items []struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				Status    string `json:"status"`
				UpdatedAt string `json:"updated_at"`
			} `json:"items"`
		}
		if err := client.doRequest("GET", path, nil, &resp); err != nil {
			return err
		}

		if len(resp.Items) == 0 {
			fmt.Println("no tasks found")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATUS\tUPDATED\tTITLE")
		for _, wi := range resp.Items {
			updated := wi.UpdatedAt
			if len(updated) > 16 {
				updated = updated[:16]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", wi.ID, wi.Status, updated, wi.Title)
		}
		return w.Flush()
	},
}

func init() {
	listCmd.Flags().StringVar(&listStatusFlag, "status", "", "Filter by status (e.g. pending, active, completed, archived)")
	listCmd.Flags().StringVar(&listProjectIDFlag, "project-id", "", "Filter by project ID")
	listCmd.Flags().BoolVar(&listAllFlag, "all", false, "Show all tasks, not just those assigned to me")
}

var getTaskCmd = &cobra.Command{
	Use:     "get <taskId>",
	Short:   "Fetch a single task by ID",
	Example: "mesh task get <id>",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		client := newMeshClientForSession(sess)

		var resp struct {
			OK   bool           `json:"ok"`
			Item map[string]any `json:"item"`
		}
		if err := client.doRequest("GET", "/task/get/"+args[0], nil, &resp); err != nil {
			return err
		}

		return printJSON(resp)
	},
}

// testSessionOverride, when non-nil, is returned directly by requireSession
// instead of loading from disk. Set by tests only.
var testSessionOverride *auth.Session

// overrideSessionForTest injects a session for unit tests. Pass nil to clear.
func overrideSessionForTest(s *auth.Session) {
	testSessionOverride = s
}

// requireSession is the commands-package version. It calls internal/auth directly
// and handles token refresh via refreshAccessToken (defined in auth.go).
func requireSession() (*auth.Session, error) {
	if testSessionOverride != nil {
		return testSessionOverride, nil
	}
	return requireSessionImpl()
}
