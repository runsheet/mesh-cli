package main

import (
	"encoding/json"
	"fmt"

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

var updateCmd = &cobra.Command{
	Use:   "update <workItemId> [status] [currentOwner]",
	Short: "Update a work item using the signed-in user session",
	Args:  cobra.RangeArgs(1, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)
		workItemId := args[0]

		payload := map[string]interface{}{
			"workItemId": workItemId,
		}
		if len(args) > 1 {
			payload["status"] = args[1]
		}
		if len(args) > 2 {
			payload["currentOwner"] = args[2]
		}

		var response map[string]interface{}
		err = client.doRequest("POST", "/work-item/update", payload, &response)
		if err != nil {
			return err
		}

		return printJSON(response)
	},
}

var handoffCmd = &cobra.Command{
	Use:   "handoff <workItemId> <fromRole> <toRole> [summary]",
	Short: "Handoff a work item using the signed-in user session",
	Args:  cobra.RangeArgs(3, 4),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)

		payload := map[string]interface{}{
			"workItemId": args[0],
			"fromRole":   args[1],
			"toRole":     args[2],
		}
		if len(args) > 3 {
			payload["handoffSummary"] = args[3]
		}

		var response map[string]interface{}
		err = client.doRequest("POST", "/work-item/handoff", payload, &response)
		if err != nil {
			return err
		}

		return printJSON(response)
	},
}

var reviewCmd = &cobra.Command{
	Use:   "review <workItemId> <reviewerRole> <decision> [notes]",
	Short: "Review a work item using the signed-in user session",
	Args:  cobra.RangeArgs(3, 4),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)

		payload := map[string]interface{}{
			"workItemId":   args[0],
			"reviewerRole": args[1],
			"decision":     args[2],
		}
		if len(args) > 3 {
			payload["notes"] = args[3]
		}

		var response map[string]interface{}
		err = client.doRequest("POST", "/work-item/review", payload, &response)
		if err != nil {
			return err
		}

		return printJSON(response)
	},
}

var learningCmd = &cobra.Command{
	Use:   "learning <workItemId> [outcome]",
	Short: "Mark learning on a work item using the signed-in user session",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)

		payload := map[string]interface{}{
			"workItemId": args[0],
		}
		if len(args) > 1 {
			payload["outcome"] = args[1]
		}

		var response map[string]interface{}
		err = client.doRequest("POST", "/work-item/learning", payload, &response)
		if err != nil {
			return err
		}

		return printJSON(response)
	},
}

func init() {
	workItemCmd.AddCommand(createCmd, updateCmd, handoffCmd, reviewCmd, learningCmd)
}
