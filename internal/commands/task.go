package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
	Short:   "Ralph Loop task commands",
	Long:    "Create, move, hand off, review, and learn from Ralph Loop tasks using your signed-in Mesh session.",
}

var allowedTaskStatuses = map[string]bool{
	"inbox":             true,
	"clarifying":        true,
	"researching":       true,
	"research_done":     true,
	"spec_ready":        true,
	"awaiting_approval": true,
	"ready_for_devlead": true,
	"in_execution":      true,
	"blocked":           true,
	"in_review":         true,
	"in_test":           true,
	"changes_requested": true,
	"done":              true,
	"ready_to_ship":     true,
	"shipping":          true,
	"shipped":           true,
	"ship_failed":       true,
	"archived":          true,
	"in_progress":       true, // personal workflow (spec 35)
}

var allowedOwnerRoles = map[string]bool{
	"human":      true,
	"pm":         true,
	"researcher": true,
	"devlead":    true,
	"builder":    true,
	"tester":     true,
	"shipper":    true,
}

var allowedPriorities = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"urgent": true,
}

var allowedScopes = map[string]bool{
	"project":  true,
	"personal": true,
	"ops":      true,
}

var allowedReviewDecisions = map[string]bool{
	"accept":         true,
	"change_request": true,
	"reject":         true,
}

func normalizeOptional(value string) string {
	return strings.TrimSpace(value)
}

func normalizeStatus(value string) string {
	switch normalizeOptional(value) {
	case "ready":
		return "ready_for_devlead"
	case "executing", "active":
		return "in_execution"
	case "review":
		return "in_review"
	case "changes", "changes-needed":
		return "changes_requested"
	case "research-done":
		return "research_done"
	case "in-test":
		return "in_test"
	case "ready-to-ship", "rts":
		return "ready_to_ship"
	case "ship-failed", "failed":
		return "ship_failed"
	case "in-progress": // personal workflow alias (spec 35)
		return "in_progress"
	default:
		return normalizeOptional(value)
	}
}

func normalizeDecision(value string) string {
	switch normalizeOptional(value) {
	case "approve", "approved":
		return "accept"
	case "changes", "request_changes", "request-changes":
		return "change_request"
	default:
		return normalizeOptional(value)
	}
}

func validateEnum(name, value string, allowed map[string]bool) error {
	if value == "" {
		return nil
	}
	if allowed[value] {
		return nil
	}
	choices := make([]string, 0, len(allowed))
	for k := range allowed {
		choices = append(choices, k)
	}
	sort.Strings(choices)
	return fmt.Errorf("invalid %s %q, expected one of: %s", name, value, strings.Join(choices, ", "))
}

var (
	updateTitle        string
	updateStatus       string
	updatePriority     string
	updateOwnerRole    string
	updateCurrentOwner string
	updateScope        string
	updateProjectID    string
	updateRequester    string
	updateNote         string

	handoffSummary       string
	handoffRequiredOut   string
	handoffRepoPath      string
	handoffSystemTarget  string
	handoffConstraints   string
	handoffTestExpect    string
	handoffNote          string

	reviewNotes             string
	reviewCriteriaChecklist string

	learningOutcome      string
	learningWhatWorked   string
	learningWhatFailed   string
	learningFollowUpNeed bool
	learningFollowUpID   string
	learningNote         string
)

var createTaskOrg string
var createTaskWorkflow string

var createCmd = &cobra.Command{
	Use:     "create <title> [scope] [projectId]",
	Short:   "Create a new Ralph Loop task",
	Long:    "Create a Ralph Loop task as the signed-in user. Use project scope for normal delivery work, personal for operator tasks, and ops for internal operations.",
	Example: "mesh task create \"Investigate deploy flake\" project <project-id>\nmesh task create \"Inbox triage\" personal\nmesh task create \"Gateway cleanup\" ops",
	Args:    cobra.RangeArgs(1, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		client := newMeshClientForSession(sess)
		if cmd.Flags().Changed("org") {
			client.OrgSlug = createTaskOrg
		}
		title := args[0]
		scope := "project"
		projectId := ""
		if len(args) > 1 {
			scope = args[1]
		}
		if len(args) > 2 {
			projectId = args[2]
		}
		if err := validateEnum("scope", scope, allowedScopes); err != nil {
			return err
		}

		payload := map[string]any{
			"title": title,
			"scope": scope,
		}
		if projectId != "" {
			payload["projectId"] = projectId
		}
		if v := strings.TrimSpace(createTaskWorkflow); v != "" {
			payload["workflowSlug"] = v
		}

		var response map[string]any
		if err := client.doRequest("POST", "/task/create", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var updateCmd = &cobra.Command{
	Use:     "update <taskId> [status] [currentOwner]",
	Aliases: []string{"move"},
	Short:   "Update task state or ownership",
	Long:    "Update a Ralph Loop task. Use the quick positional form for common moves, or flags for the fuller contract.",
	Example: "mesh task update <id> ready\nmesh task update <id> in_review builder\nmesh task update <id> --priority high --owner-role devlead --note \"Escalating for review\"",
	Args:    cobra.RangeArgs(1, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		status := updateStatus
		currentOwner := updateCurrentOwner
		if len(args) > 1 && normalizeOptional(status) == "" {
			status = args[1]
		}
		if len(args) > 2 && !cmd.Flags().Changed("current-owner") {
			currentOwner = args[2]
		}
		status = normalizeStatus(status)

		if err := validateEnum("status", normalizeOptional(status), allowedTaskStatuses); err != nil {
			return err
		}
		if err := validateEnum("priority", normalizeOptional(updatePriority), allowedPriorities); err != nil {
			return err
		}
		if err := validateEnum("owner role", normalizeOptional(updateOwnerRole), allowedOwnerRoles); err != nil {
			return err
		}
		if err := validateEnum("scope", normalizeOptional(updateScope), allowedScopes); err != nil {
			return err
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}

		payload := map[string]any{"taskId": args[0]}
		if v := normalizeOptional(updateTitle); v != "" {
			payload["title"] = v
		}
		if v := normalizeOptional(status); v != "" {
			payload["status"] = v
		}
		if v := normalizeOptional(updatePriority); v != "" {
			payload["priority"] = v
		}
		if v := normalizeOptional(updateOwnerRole); v != "" {
			payload["ownerRole"] = v
		}
		if cmd.Flags().Changed("current-owner") || len(args) > 2 {
			payload["currentOwner"] = currentOwner
		}
		if v := normalizeOptional(updateScope); v != "" {
			payload["scope"] = v
		}
		if cmd.Flags().Changed("project-id") {
			payload["projectId"] = normalizeOptional(updateProjectID)
		}
		if cmd.Flags().Changed("requester") {
			payload["requester"] = updateRequester
		}
		if v := normalizeOptional(updateNote); v != "" {
			payload["note"] = v
		}
		if len(payload) == 1 {
			return fmt.Errorf("no update fields provided")
		}

		client := newMeshClientForSession(sess)
		var response map[string]any
		if err := client.doRequest("POST", "/task/update", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var handoffCmd = &cobra.Command{
	Use:     "handoff <taskId> <fromRole> <toRole> [summary]",
	Short:   "Record a workflow handoff",
	Long:    "Record the handoff packet between workflow roles. Use the optional summary positional arg for the common case, and flags when you want the fuller packet.",
	Example: "mesh task handoff <id> pm devlead \"Ready for review\"\nmesh task handoff <id> devlead builder --summary \"Implement portal auth fix\" --required-outputs \"PR plus test notes\" --repo-path /repo/path",
	Args:    cobra.RangeArgs(3, 4),
	RunE: func(cmd *cobra.Command, args []string) error {
		summary := handoffSummary
		if len(args) > 3 && normalizeOptional(summary) == "" {
			summary = args[3]
		}
		if err := validateEnum("from role", args[1], allowedOwnerRoles); err != nil {
			return err
		}
		if err := validateEnum("to role", args[2], allowedOwnerRoles); err != nil {
			return err
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}

		payload := map[string]any{
			"taskId": args[0],
			"fromRole":   args[1],
			"toRole":     args[2],
		}
		if v := normalizeOptional(summary); v != "" {
			payload["handoffSummary"] = v
		}
		if v := normalizeOptional(handoffRequiredOut); v != "" {
			payload["requiredOutputs"] = v
		}
		if v := normalizeOptional(handoffRepoPath); v != "" {
			payload["repoPath"] = v
		}
		if v := normalizeOptional(handoffSystemTarget); v != "" {
			payload["systemTarget"] = v
		}
		if v := normalizeOptional(handoffConstraints); v != "" {
			payload["constraints"] = v
		}
		if v := normalizeOptional(handoffTestExpect); v != "" {
			payload["testExpectations"] = v
		}
		if v := normalizeOptional(handoffNote); v != "" {
			payload["note"] = v
		}

		client := newMeshClientForSession(sess)
		var response map[string]any
		if err := client.doRequest("POST", "/task/handoff", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var reviewCmd = &cobra.Command{
	Use:     "review <taskId> <reviewerRole> <decision> [notes]",
	Short:   "Record a Ralph Loop review",
	Long:    "Record the review decision for a task. Use accept, change_request, or reject. Friendly aliases like approve and changes are also accepted.",
	Example: "mesh task review <id> devlead approve \"Looks good\"\nmesh task review <id> human changes --criteria-checklist \"tests, docs\"",
	Args:    cobra.RangeArgs(3, 4),
	RunE: func(cmd *cobra.Command, args []string) error {
		notes := reviewNotes
		if len(args) > 3 && normalizeOptional(notes) == "" {
			notes = args[3]
		}
		decision := normalizeDecision(args[2])
		if err := validateEnum("reviewer role", args[1], allowedOwnerRoles); err != nil {
			return err
		}
		if err := validateEnum("decision", decision, allowedReviewDecisions); err != nil {
			return err
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}

		payload := map[string]any{
			"taskId":   args[0],
			"reviewerRole": args[1],
			"decision":     decision,
		}
		if v := normalizeOptional(notes); v != "" {
			payload["notes"] = v
		}
		if v := normalizeOptional(reviewCriteriaChecklist); v != "" {
			payload["criteriaChecklist"] = v
		}

		client := newMeshClientForSession(sess)
		var response map[string]any
		if err := client.doRequest("POST", "/task/review", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var learningCmd = &cobra.Command{
	Use:     "learning <taskId> [outcome]",
	Aliases: []string{"learn"},
	Short:   "Record Ralph Loop learning",
	Long:    "Capture the learning packet for a task. Use the quick outcome positional arg for the common case, or add structured flags for deeper learnings.",
	Example: "mesh task learning <id> \"Shipped cleanly\"\nmesh task learning <id> --what-worked \"Portal handoff login\" --what-failed \"Schema drift\" --follow-up-needed --follow-up-item-id <id>",
	Args:    cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		outcome := learningOutcome
		if len(args) > 1 && normalizeOptional(outcome) == "" {
			outcome = args[1]
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}

		payload := map[string]any{"taskId": args[0]}
		if v := normalizeOptional(outcome); v != "" {
			payload["outcome"] = v
		}
		if v := normalizeOptional(learningWhatWorked); v != "" {
			payload["whatWorked"] = v
		}
		if v := normalizeOptional(learningWhatFailed); v != "" {
			payload["whatFailed"] = v
		}
		if cmd.Flags().Changed("follow-up-needed") {
			payload["followUpNeeded"] = learningFollowUpNeed
		}
		if cmd.Flags().Changed("follow-up-item-id") {
			payload["followUpItemId"] = normalizeOptional(learningFollowUpID)
		}
		if v := normalizeOptional(learningNote); v != "" {
			payload["note"] = v
		}
		if len(payload) == 1 {
			return fmt.Errorf("no learning fields provided")
		}

		client := newMeshClientForSession(sess)
		var response map[string]any
		if err := client.doRequest("POST", "/task/learning", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

func init() {
	createCmd.Flags().StringVar(&createTaskOrg, "org", "", "Org slug to scope the task (overrides session default)")
	createCmd.Flags().StringVar(&createTaskWorkflow, "workflow", "", "Workflow slug (e.g. full, quick, doc, research, personal); overrides project default")
	updateCmd.Flags().StringVar(&updateTitle, "title", "", "Updated task title")
	updateCmd.Flags().StringVar(&updateStatus, "status", "", "Status, for example ready, in_review, blocked, done")
	updateCmd.Flags().StringVar(&updatePriority, "priority", "", "Priority: low, medium, high, urgent")
	updateCmd.Flags().StringVar(&updateOwnerRole, "owner-role", "", "Owner role: human, pm, devlead, builder")
	updateCmd.Flags().StringVar(&updateCurrentOwner, "current-owner", "", "Current owner value, pass empty string to clear")
	updateCmd.Flags().StringVar(&updateScope, "scope", "", "Scope: project, personal, ops")
	updateCmd.Flags().StringVar(&updateProjectID, "project-id", "", "Project ID, pass empty string to clear when scope is personal or ops")
	updateCmd.Flags().StringVar(&updateRequester, "requester", "", "Requester, pass empty string to clear")
	updateCmd.Flags().StringVar(&updateNote, "note", "", "Event note for the update")

	handoffCmd.Flags().StringVar(&handoffSummary, "summary", "", "Handoff summary")
	handoffCmd.Flags().StringVar(&handoffRequiredOut, "required-outputs", "", "Required outputs for the next role")
	handoffCmd.Flags().StringVar(&handoffRepoPath, "repo-path", "", "Relevant repository path")
	handoffCmd.Flags().StringVar(&handoffSystemTarget, "system-target", "", "System or surface being targeted")
	handoffCmd.Flags().StringVar(&handoffConstraints, "constraints", "", "Constraints for the handoff")
	handoffCmd.Flags().StringVar(&handoffTestExpect, "test-expectations", "", "Expected verification or tests")
	handoffCmd.Flags().StringVar(&handoffNote, "note", "", "Event note for the handoff")

	reviewCmd.Flags().StringVar(&reviewNotes, "notes", "", "Review notes")
	reviewCmd.Flags().StringVar(&reviewCriteriaChecklist, "criteria-checklist", "", "Review criteria checklist")

	learningCmd.Flags().StringVar(&learningOutcome, "outcome", "", "Overall learning outcome")
	learningCmd.Flags().StringVar(&learningWhatWorked, "what-worked", "", "What worked well")
	learningCmd.Flags().StringVar(&learningWhatFailed, "what-failed", "", "What failed")
	learningCmd.Flags().BoolVar(&learningFollowUpNeed, "follow-up-needed", false, "Whether follow-up work is needed")
	learningCmd.Flags().StringVar(&learningFollowUpID, "follow-up-item-id", "", "Follow-up task ID")
	learningCmd.Flags().StringVar(&learningNote, "note", "", "Event note for the learning update")

	taskCmd.AddCommand(listCmd, getTaskCmd, createCmd, updateCmd, handoffCmd, reviewCmd, learningCmd)
}

var (
	listStatusFlag    string
	listProjectIDFlag string
	listAllFlag       bool
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List Ralph Loop tasks",
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
				ID           string `json:"id"`
				Title        string `json:"title"`
				Status       string `json:"status"`
				CurrentOwner string `json:"current_owner"`
				UpdatedAt    string `json:"updated_at"`
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
		fmt.Fprintln(w, "ID\tSTATUS\tOWNER\tUPDATED\tTITLE")
		for _, wi := range resp.Items {
			updated := wi.UpdatedAt
			if len(updated) > 16 {
				updated = updated[:16]
			}
			owner := wi.CurrentOwner
			if owner == "" {
				owner = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", wi.ID, wi.Status, owner, updated, wi.Title)
		}
		return w.Flush()
	},
}

func init() {
	listCmd.Flags().StringVar(&listStatusFlag, "status", "", "Filter by status (e.g. ready_for_devlead,in_execution)")
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
