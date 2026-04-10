package main

import (
	"fmt"
	"github.com/spf13/cobra"
)

var workItemCmd = &cobra.Command{
	Use:   "work-item",
	Short: "Work item management commands",
}

var createCmd = &cobra.Command{
	Use:   "create <title> <sourceAgentId> [scope] [projectId]",
	Short: "Create a new work item",
	Args:  cobra.RangeArgs(2, 4),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)

		title := args[0]
		sourceAgentId := args[1]
		scope := ""
		projectId := ""
		if len(args) > 2 {
			scope = args[2]
		}
		if len(args) > 3 {
			projectId = args[3]
		}

		payload := map[string]interface{}{
			"title":        title,
			"sourceAgentId": sourceAgentId,
		}
		if scope != "" {
			payload["scope"] = scope
		}
		if projectId != "" {
			payload["projectId"] = projectId
		}

		var response map[string]interface{}
		err = client.doRequest("POST", "/v1/work-items", payload, &response)
		if err != nil {
			return err
		}

		fmt.Printf("Work item created: %v\n", response)
		return nil
	},
}

var updateCmd = &cobra.Command{
	Use:   "update <workItemId> <agentId> [status] [currentOwner]",
	Short: "Update a work item",
	Args:  cobra.RangeArgs(2, 4),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)

		workItemId := args[0]
		agentId := args[1]

		payload := map[string]interface{}{
			"agentId": agentId,
		}

		if len(args) > 2 {
			payload["status"] = args[2]
		}
		if len(args) > 3 {
			payload["currentOwner"] = args[3]
		}

		var response map[string]interface{}
		err = client.doRequest("PATCH", fmt.Sprintf("/v1/work-items/%s", workItemId), payload, &response)
		if err != nil {
			return err
		}

		fmt.Printf("Work item updated: %v\n", response)
		return nil
	},
}

var handoffCmd = &cobra.Command{
	Use:   "handoff <workItemId> <fromRole> <toRole> <agentId> [summary]",
	Short: "Handoff a work item",
	Args:  cobra.RangeArgs(4, 5),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)

		workItemId := args[0]
		fromRole := args[1]
		toRole := args[2]
		agentId := args[3]

		payload := map[string]interface{}{
			"fromRole": fromRole,
			"toRole":   toRole,
			"agentId":  agentId,
		}

		if len(args) > 4 {
			payload["summary"] = args[4]
		}

		var response map[string]interface{}
		err = client.doRequest("POST", fmt.Sprintf("/v1/work-items/%s/handoff", workItemId), payload, &response)
		if err != nil {
			return err
		}

		fmt.Printf("Work item handoffed: %v\n", response)
		return nil
	},
}

var reviewCmd = &cobra.Command{
	Use:   "review <workItemId> <reviewerRole> <decision> <agentId> [notes]",
	Short: "Review a work item",
	Args:  cobra.RangeArgs(4, 5),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)

		workItemId := args[0]
		reviewerRole := args[1]
		decision := args[2]
		agentId := args[3]

		payload := map[string]interface{}{
			"reviewerRole": reviewerRole,
			"decision":     decision,
			"agentId":      agentId,
		}

		if len(args) > 4 {
			payload["notes"] = args[4]
		}

		var response map[string]interface{}
		err = client.doRequest("POST", fmt.Sprintf("/v1/work-items/%s/review", workItemId), payload, &response)
		if err != nil {
			return err
		}

		fmt.Printf("Work item reviewed: %v\n", response)
		return nil
	},
}

var learningCmd = &cobra.Command{
	Use:   "learning <workItemId> <agentId> [outcome]",
	Short: "Mark learning on a work item",
	Args:  cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)

		workItemId := args[0]
		agentId := args[1]

		payload := map[string]interface{}{
			"agentId": agentId,
		}

		if len(args) > 2 {
			payload["outcome"] = args[2]
		}

		var response map[string]interface{}
		err = client.doRequest("POST", fmt.Sprintf("/v1/work-items/%s/learning", workItemId), payload, &response)
		if err != nil {
			return err
		}

		fmt.Printf("Work item learning recorded: %v\n", response)
		return nil
	},
}

func init() {
	workItemCmd.AddCommand(createCmd, updateCmd, handoffCmd, reviewCmd, learningCmd)
}
