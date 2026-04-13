package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func printJSON(v interface{}) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(body))
	return nil
}

var workItemCmd = &cobra.Command{
	Use:   "work-item",
	Short: "Work item management commands",
}

var allowedWorkItemStatuses = map[string]bool{
	"inbox":              true,
	"clarifying":         true,
	"spec_ready":         true,
	"awaiting_approval":  true,
	"ready_for_devlead":  true,
	"in_execution":       true,
	"blocked":            true,
	"in_review":          true,
	"changes_requested":  true,
	"done":               true,
	"archived":           true,
}

var allowedOwnerRoles = map[string]bool{
	"human":   true,
	"pm":      true,
	"devlead": true,
	"builder": true,
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
	return fmt.Errorf("invalid %s %q, expected one of: %s", name, value, strings.Join(choices, ", "))
}

var createCmd = &cobra.Command{
	Use:   "create <title> [scope] [projectId]",
	Short: "Create a new work item using the signed-in user session",
	Args:  cobra.RangeArgs(1, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)

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

		payload := map[string]interface{}{
			"title": title,
			"scope": scope,
		}
		if projectId != "" {
			payload["projectId"] = projectId
		}

		var response map[string]interface{}
		err = client.doRequest("POST", "/work-item/create", payload, &response)
		if err != nil {
			return err
		}

		return printJSON(response)
	},
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
	handoffLinkedSpecVer int
	handoffNote          string

	reviewNotes             string
	reviewCriteriaChecklist string

	learningOutcome       string
	learningWhatWorked    string
	learningWhatFailed    string
	learningFollowUpNeed  bool
	learningFollowUpID    string
	learningNote          string
)

var updateCmd = &cobra.Command{
	Use:   "update <workItemId>",
	Short: "Update a work item using the signed-in user session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateEnum("status", normalizeOptional(updateStatus), allowedWorkItemStatuses); err != nil {
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

		sess, err := RequireSession()
		if err != nil {
			return err
		}

		payload := map[string]interface{}{
			"workItemId": args[0],
		}
		if v := normalizeOptional(updateTitle); v != "" {
			payload["title"] = v
		}
		if v := normalizeOptional(updateStatus); v != "" {
			payload["status"] = v
		}
		if v := normalizeOptional(updatePriority); v != "" {
			payload["priority"] = v
		}
		if v := normalizeOptional(updateOwnerRole); v != "" {
			payload["ownerRole"] = v
		}
		if cmd.Flags().Changed("current-owner") {
			payload["currentOwner"] = updateCurrentOwner
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

		client := NewClient(sess.BaseURL, sess.AccessToken)
		var response map[string]interface{}
		if err := client.doRequest("POST", "/work-item/update", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var handoffCmd = &cobra.Command{
	Use:   "handoff <workItemId> <fromRole> <toRole>",
	Short: "Record a Ralph Loop handoff using the signed-in user session",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateEnum("from role", args[1], allowedOwnerRoles); err != nil {
			return err
		}
		if err := validateEnum("to role", args[2], allowedOwnerRoles); err != nil {
			return err
		}

		sess, err := RequireSession()
		if err != nil {
			return err
		}

		payload := map[string]interface{}{
			"workItemId": args[0],
			"fromRole":   args[1],
			"toRole":     args[2],
		}
		if v := normalizeOptional(handoffSummary); v != "" {
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
		if cmd.Flags().Changed("linked-spec-version") {
			payload["linkedSpecVersion"] = handoffLinkedSpecVer
		}
		if v := normalizeOptional(handoffNote); v != "" {
			payload["note"] = v
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)
		var response map[string]interface{}
		if err := client.doRequest("POST", "/work-item/handoff", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var reviewCmd = &cobra.Command{
	Use:   "review <workItemId> <reviewerRole> <decision>",
	Short: "Record a Ralph Loop review using the signed-in user session",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateEnum("reviewer role", args[1], allowedOwnerRoles); err != nil {
			return err
		}
		if err := validateEnum("decision", args[2], allowedReviewDecisions); err != nil {
			return err
		}

		sess, err := RequireSession()
		if err != nil {
			return err
		}

		payload := map[string]interface{}{
			"workItemId":   args[0],
			"reviewerRole": args[1],
			"decision":     args[2],
		}
		if v := normalizeOptional(reviewNotes); v != "" {
			payload["notes"] = v
		}
		if v := normalizeOptional(reviewCriteriaChecklist); v != "" {
			payload["criteriaChecklist"] = v
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)
		var response map[string]interface{}
		if err := client.doRequest("POST", "/work-item/review", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var learningCmd = &cobra.Command{
	Use:   "learning <workItemId>",
	Short: "Record Ralph Loop learning on a work item using the signed-in user session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		payload := map[string]interface{}{
			"workItemId": args[0],
		}
		if v := normalizeOptional(learningOutcome); v != "" {
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

		client := NewClient(sess.BaseURL, sess.AccessToken)
		var response map[string]interface{}
		if err := client.doRequest("POST", "/work-item/learning", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

func init() {
	updateCmd.Flags().StringVar(&updateTitle, "title", "", "Updated work item title")
	updateCmd.Flags().StringVar(&updateStatus, "status", "", "Status: inbox, clarifying, spec_ready, awaiting_approval, ready_for_devlead, in_execution, blocked, in_review, changes_requested, done, archived")
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
	handoffCmd.Flags().IntVar(&handoffLinkedSpecVer, "linked-spec-version", 0, "Linked spec version")
	handoffCmd.Flags().StringVar(&handoffNote, "note", "", "Event note for the handoff")

	reviewCmd.Flags().StringVar(&reviewNotes, "notes", "", "Review notes")
	reviewCmd.Flags().StringVar(&reviewCriteriaChecklist, "criteria-checklist", "", "Review criteria checklist")

	learningCmd.Flags().StringVar(&learningOutcome, "outcome", "", "Overall learning outcome")
	learningCmd.Flags().StringVar(&learningWhatWorked, "what-worked", "", "What worked well")
	learningCmd.Flags().StringVar(&learningWhatFailed, "what-failed", "", "What failed")
	learningCmd.Flags().BoolVar(&learningFollowUpNeed, "follow-up-needed", false, "Whether follow-up work is needed")
	learningCmd.Flags().StringVar(&learningFollowUpID, "follow-up-item-id", "", "Follow-up work item ID")
	learningCmd.Flags().StringVar(&learningNote, "note", "", "Event note for the learning update")

	workItemCmd.AddCommand(createCmd, updateCmd, handoffCmd, reviewCmd, learningCmd)
}
