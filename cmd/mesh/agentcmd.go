package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// TOOL_ALLOWLIST mirrors mesh-api/src/lib/models.ts. Kept in sync manually;
// `--force` bypasses the check (E.25).
var cliToolAllowlist = map[string]bool{
	"Read":          true,
	"Edit":          true,
	"Write":         true,
	"Bash":          true,
	"Grep":          true,
	"Glob":          true,
	"WebFetch":      true,
	"WebSearch":     true,
	"Task":          true,
	"TodoWrite":     true,
	"NotebookEdit":  true,
	"BashOutput":    true,
	"KillShell":     true,
	"SlashCommand":  true,
	"ExitPlanMode":  true,
}

var (
	agentListProjectID string

	agentCreateName         string
	agentCreateDescription  string
	agentCreateSystemPrompt string
	agentCreateTools        string
	agentCreateModel        string
	agentCreateProjectID    string
	agentCreateNodeID       string
	agentCreateForce        bool

	agentUpdateName         string
	agentUpdateDescription  string
	agentUpdateSystemPrompt string
	agentUpdateTools        string
	agentUpdateModel        string
	agentUpdateProjectID    string
	agentUpdateNodeID       string
	agentUpdateForce        bool

	agentDeleteYes bool
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage Mesh agents",
}

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List agents visible to the signed-in user",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}
		client := NewClient(sess.BaseURL, sess.AccessToken)

		path := "/agent/list"
		if p := strings.TrimSpace(agentListProjectID); p != "" {
			path = path + "?projectId=" + p
		}

		var resp struct {
			OK     bool `json:"ok"`
			Agents []struct {
				ID           string `json:"id"`
				Name         string `json:"name"`
				Model        string `json:"model"`
				ProjectID    *string `json:"project_id"`
				PinnedNodeID string `json:"pinned_node_id"`
			} `json:"agents"`
		}
		if err := client.doRequest("GET", path, nil, &resp); err != nil {
			return err
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tPROJECT\tPINNED_NODE\tMODEL")
		for _, a := range resp.Agents {
			project := "-"
			if a.ProjectID != nil {
				project = *a.ProjectID
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Name, project, a.PinnedNodeID, a.Model)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if len(resp.Agents) == 0 {
			fmt.Println("No agents found.")
		}
		return nil
	},
}

var agentCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a Mesh agent (interactive or flag-driven)",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		reader := bufio.NewReader(os.Stdin)

		name := strings.TrimSpace(agentCreateName)
		if name == "" {
			name = promptLine(reader, "Agent name: ")
		}
		if name == "" {
			return fmt.Errorf("name is required")
		}

		nodeID := strings.TrimSpace(agentCreateNodeID)
		if nodeID == "" {
			nodeID = promptLine(reader, "Pinned node ID: ")
		}
		if nodeID == "" {
			return fmt.Errorf("--pinned-node-id is required")
		}

		model := strings.TrimSpace(agentCreateModel)
		if model == "" {
			model = promptLine(reader, "Model (e.g. claude-opus-4-7): ")
		}
		if model == "" {
			return fmt.Errorf("--model is required")
		}

		description := agentCreateDescription
		systemPrompt := agentCreateSystemPrompt
		projectID := strings.TrimSpace(agentCreateProjectID)

		tools, err := parseTools(agentCreateTools, agentCreateForce)
		if err != nil {
			return err
		}

		payload := map[string]interface{}{
			"name":         name,
			"model":        model,
			"pinnedNodeId": nodeID,
			"tools":        tools,
		}
		if description != "" {
			payload["description"] = description
		}
		if systemPrompt != "" {
			payload["systemPrompt"] = systemPrompt
		}
		if projectID != "" {
			payload["projectId"] = projectID
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)
		var resp map[string]interface{}
		if err := client.doRequest("POST", "/agent/create", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var agentUpdateCmd = &cobra.Command{
	Use:   "update <agent-id>",
	Short: "Update fields on an existing Mesh agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		payload := map[string]interface{}{"id": args[0]}
		if cmd.Flags().Changed("name") {
			payload["name"] = agentUpdateName
		}
		if cmd.Flags().Changed("description") {
			payload["description"] = agentUpdateDescription
		}
		if cmd.Flags().Changed("system-prompt") {
			payload["systemPrompt"] = agentUpdateSystemPrompt
		}
		if cmd.Flags().Changed("model") {
			payload["model"] = agentUpdateModel
		}
		if cmd.Flags().Changed("project-id") {
			v := strings.TrimSpace(agentUpdateProjectID)
			if v == "" {
				payload["projectId"] = nil
			} else {
				payload["projectId"] = v
			}
		}
		if cmd.Flags().Changed("pinned-node-id") {
			payload["pinnedNodeId"] = agentUpdateNodeID
		}
		if cmd.Flags().Changed("tools") {
			tools, err := parseTools(agentUpdateTools, agentUpdateForce)
			if err != nil {
				return err
			}
			payload["tools"] = tools
		}
		if len(payload) == 1 {
			return fmt.Errorf("no update fields provided")
		}

		client := NewClient(sess.BaseURL, sess.AccessToken)
		var resp map[string]interface{}
		if err := client.doRequest("POST", "/agent/update", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var agentDeleteCmd = &cobra.Command{
	Use:   "delete <agent-id>",
	Short: "Delete a Mesh agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}
		if !agentDeleteYes {
			reader := bufio.NewReader(os.Stdin)
			answer := promptLine(reader, fmt.Sprintf("Delete agent %s? [y/N]: ", args[0]))
			if strings.ToLower(strings.TrimSpace(answer)) != "y" {
				fmt.Println("Cancelled.")
				return nil
			}
		}
		client := NewClient(sess.BaseURL, sess.AccessToken)
		var resp map[string]interface{}
		if err := client.doRequest("POST", "/agent/delete", map[string]string{"id": args[0]}, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

func init() {
	agentListCmd.Flags().StringVar(&agentListProjectID, "project-id", "", "Filter to a specific project")

	agentCreateCmd.Flags().StringVar(&agentCreateName, "name", "", "Agent name")
	agentCreateCmd.Flags().StringVar(&agentCreateDescription, "description", "", "Short description")
	agentCreateCmd.Flags().StringVar(&agentCreateSystemPrompt, "system-prompt", "", "System prompt body")
	agentCreateCmd.Flags().StringVar(&agentCreateTools, "tools", "", "Comma-separated tool names")
	agentCreateCmd.Flags().StringVar(&agentCreateModel, "model", "", "Model (claude-opus-4-7, claude-sonnet-4-6, claude-haiku-4-5)")
	agentCreateCmd.Flags().StringVar(&agentCreateProjectID, "project-id", "", "Project ID (omit for personal agent)")
	agentCreateCmd.Flags().StringVar(&agentCreateNodeID, "pinned-node-id", "", "Node ID this agent pins to")
	agentCreateCmd.Flags().BoolVar(&agentCreateForce, "force", false, "Skip client-side tool validation")

	agentUpdateCmd.Flags().StringVar(&agentUpdateName, "name", "", "Agent name")
	agentUpdateCmd.Flags().StringVar(&agentUpdateDescription, "description", "", "Short description")
	agentUpdateCmd.Flags().StringVar(&agentUpdateSystemPrompt, "system-prompt", "", "System prompt body")
	agentUpdateCmd.Flags().StringVar(&agentUpdateTools, "tools", "", "Comma-separated tool names")
	agentUpdateCmd.Flags().StringVar(&agentUpdateModel, "model", "", "Model")
	agentUpdateCmd.Flags().StringVar(&agentUpdateProjectID, "project-id", "", "Project ID (empty string clears)")
	agentUpdateCmd.Flags().StringVar(&agentUpdateNodeID, "pinned-node-id", "", "Node ID this agent pins to")
	agentUpdateCmd.Flags().BoolVar(&agentUpdateForce, "force", false, "Skip client-side tool validation")

	agentDeleteCmd.Flags().BoolVarP(&agentDeleteYes, "yes", "y", false, "Skip confirmation")

	agentCmd.AddCommand(agentListCmd, agentCreateCmd, agentUpdateCmd, agentDeleteCmd)
}

func promptLine(r *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	line, err := r.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(line)
}

func parseTools(csv string, force bool) ([]string, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return []string{}, nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	var unknown []string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		if !force {
			if !cliToolAllowlist[t] {
				unknown = append(unknown, t)
			}
		}
		out = append(out, t)
	}
	if len(unknown) > 0 && !force {
		allowed := make([]string, 0, len(cliToolAllowlist))
		for k := range cliToolAllowlist {
			allowed = append(allowed, k)
		}
		sort.Strings(allowed)
		return nil, fmt.Errorf("unknown tool(s): %s — allowed: %s (use --force to bypass)",
			strings.Join(unknown, ", "), strings.Join(allowed, ", "))
	}
	return out, nil
}
