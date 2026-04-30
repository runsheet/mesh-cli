package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/localsock"
	"projectmesh.io/mesh-cli/internal/state"
)

var cliToolAllowlist = map[string]bool{
	"Read":         true,
	"Edit":         true,
	"Write":        true,
	"Bash":         true,
	"Grep":         true,
	"Glob":         true,
	"WebFetch":     true,
	"WebSearch":    true,
	"Task":         true,
	"TodoWrite":    true,
	"NotebookEdit": true,
	"BashOutput":   true,
	"KillShell":    true,
	"SlashCommand": true,
	"ExitPlanMode": true,
}

var validAgentRoles = map[string]bool{
	"pm": true, "researcher": true, "devlead": true, "builder": true, "tester": true,
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
	agentCreateRole         string
	agentCreateForce        bool

	agentUpdateName         string
	agentUpdateDescription  string
	agentUpdateSystemPrompt string
	agentUpdateTools        string
	agentUpdateModel        string
	agentUpdateProjectID    string
	agentUpdateNodeID       string
	agentUpdateRole         string
	agentUpdateForce        bool

	agentDeleteYes bool

	agentPushProjectID string
	agentPushAs        string
	agentPushAll       bool

	agentPullAs string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage Mesh agents",
}

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List agents visible to the signed-in user",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)

		path := "/agent/list"
		if p := strings.TrimSpace(agentListProjectID); p != "" {
			path = path + "?projectId=" + p
		}

		var resp struct {
			OK     bool `json:"ok"`
			Agents []struct {
				ID           string  `json:"id"`
				Name         string  `json:"name"`
				Model        string  `json:"model"`
				ProjectID    *string `json:"project_id"`
				PinnedNodeID string  `json:"pinned_node_id"`
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
		sess, err := requireSession()
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

		if role := strings.TrimSpace(agentCreateRole); role != "" && !validAgentRoles[role] {
			return fmt.Errorf("invalid --role %q: must be one of pm, researcher, devlead, builder, tester", role)
		}

		payload := map[string]any{
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
		if role := strings.TrimSpace(agentCreateRole); role != "" {
			payload["role"] = role
		}

		client := newMeshClientForSession(sess)
		var resp map[string]any
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
		sess, err := requireSession()
		if err != nil {
			return err
		}

		payload := map[string]any{"id": args[0]}
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
		if cmd.Flags().Changed("role") {
			role := strings.TrimSpace(agentUpdateRole)
			if role != "" && !validAgentRoles[role] {
				return fmt.Errorf("invalid --role %q: must be one of pm, researcher, devlead, builder, tester, or empty string to clear", role)
			}
			payload["role"] = role // "" sends null (clear) via JSON
			if agentUpdateForce {
				payload["force"] = true
			}
		}
		if len(payload) == 1 {
			return fmt.Errorf("no update fields provided")
		}

		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/agent/update", payload, &resp); err != nil {
			// Detect 409 role_conflict from API and surface a clear message.
			if strings.Contains(err.Error(), "role_conflict") {
				return fmt.Errorf("%w\nUse --force to reassign the role from the current holder", err)
			}
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
		sess, err := requireSession()
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
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/agent/delete", map[string]string{"id": args[0]}, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var agentPushCmd = &cobra.Command{
	Use:   "push [name]",
	Short: "Adopt a local agent into the cluster (Docker push analog)",
	Long:  "Push a local agent's definition to the cluster so it becomes a row in mesh-api pinned to this node. Use --all to adopt every local agent that hasn't been adopted yet.",
	RunE: func(cmd *cobra.Command, args []string) error {
		lc, err := newLocalClient()
		if err != nil {
			return err
		}

		if agentPushAll {
			if len(args) > 0 {
				return fmt.Errorf("--all cannot be combined with a positional name")
			}
			return pushAllAgents(cmd, lc)
		}

		if len(args) != 1 {
			return fmt.Errorf("exactly one agent name is required (or pass --all)")
		}
		return pushOneAgent(cmd, lc, args[0], agentPushAs)
	},
}

var agentPullCmd = &cobra.Command{
	Use:   "pull <cloud-id>",
	Short: "Materialize a cloud agent definition as a local agent",
	Long:  "Pull a cloud agent (by UUID) into the local daemon so it shows up in `mesh ps -a`. Does not start the session — run `mesh restart` to spawn.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		lc, err := newLocalClient()
		if err != nil {
			return err
		}
		return pullOneAgent(cmd, lc, args[0], agentPullAs)
	},
}

func pushOneAgent(cmd *cobra.Command, lc *localClient, name, asName string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	reader := bufio.NewReader(os.Stdin)

	for {
		req := localsock.PushSessionRequest{ProjectID: agentPushProjectID, As: asName}
		status, body, err := lc.pushAgent(ctx, name, req)
		if err != nil {
			return err
		}

		switch status {
		case http.StatusOK:
			var resp localsock.PushSessionResponse
			if jerr := json.Unmarshal(body, &resp); jerr != nil {
				return jerr
			}
			project := "personal"
			if resp.ProjectID != "" {
				project = resp.ProjectID
			}
			fmt.Printf("adopted %s as cloud agent %s in project %s\n", resp.Name, resp.CloudAgentID, project)
			return nil
		case http.StatusConflict:
			var parsed struct {
				Error           string `json:"error"`
				CloudAgentID    string `json:"cloudAgentId"`
				ExistingAgentID string `json:"existingAgentId"`
			}
			_ = json.Unmarshal(body, &parsed)
			if parsed.Error == "already_adopted" {
				return fmt.Errorf("agent %q is already adopted (cloudAgentId=%s)", name, parsed.CloudAgentID)
			}
			clashed := asName
			if clashed == "" {
				clashed = name
			}
			fmt.Printf("name %q already exists in the cluster. Rename? [new name or q]: ", clashed)
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(answer)
			if answer == "" || answer == "q" || answer == "Q" {
				return fmt.Errorf("push cancelled: name collision in cluster")
			}
			asName = answer
			continue
		case http.StatusPreconditionFailed:
			return fmt.Errorf("daemon is not linked to a cluster; run `mesh node register` first")
		default:
			return fmt.Errorf("push failed: %s: %s", http.StatusText(status), strings.TrimSpace(string(body)))
		}
	}
}

func pushAllAgents(cmd *cobra.Command, lc *localClient) error {
	agents, err := state.ListAgents()
	if err != nil {
		return err
	}
	adopted := 0
	skipped := 0
	for _, a := range agents {
		if a.CloudAgentID != nil && *a.CloudAgentID != "" {
			skipped++
			continue
		}
		if err := pushOneAgent(cmd, lc, a.Name, ""); err != nil {
			fmt.Printf("skipping %s: %v\n", a.Name, err)
			continue
		}
		adopted++
	}
	fmt.Printf("push --all: adopted=%d skipped=%d\n", adopted, skipped)
	return nil
}

func pullOneAgent(cmd *cobra.Command, lc *localClient, cloudID, asName string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	status, body, err := lc.pullAgent(ctx, localsock.PullSessionRequest{CloudAgentID: cloudID, As: asName})
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		var resp localsock.PullSessionResponse
		if jerr := json.Unmarshal(body, &resp); jerr != nil {
			return jerr
		}
		fmt.Printf("pulled cloud agent %s as local %q (not started; run `mesh restart %s` to spawn)\n", resp.CloudAgentID, resp.Name, resp.Name)
		return nil
	case http.StatusConflict:
		return fmt.Errorf("local name already in use; pass --as <different-name>")
	case http.StatusPreconditionFailed:
		return fmt.Errorf("daemon is not linked to a cluster; run `mesh node register` first")
	default:
		return fmt.Errorf("pull failed: %s: %s", http.StatusText(status), strings.TrimSpace(string(body)))
	}
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
	agentCreateCmd.Flags().StringVar(&agentCreateRole, "role", "", "Loop role: pm, researcher, devlead, builder, tester")
	agentCreateCmd.Flags().BoolVar(&agentCreateForce, "force", false, "Skip client-side tool validation")

	agentUpdateCmd.Flags().StringVar(&agentUpdateName, "name", "", "Agent name")
	agentUpdateCmd.Flags().StringVar(&agentUpdateDescription, "description", "", "Short description")
	agentUpdateCmd.Flags().StringVar(&agentUpdateSystemPrompt, "system-prompt", "", "System prompt body")
	agentUpdateCmd.Flags().StringVar(&agentUpdateTools, "tools", "", "Comma-separated tool names")
	agentUpdateCmd.Flags().StringVar(&agentUpdateModel, "model", "", "Model")
	agentUpdateCmd.Flags().StringVar(&agentUpdateProjectID, "project-id", "", "Project ID (empty string clears)")
	agentUpdateCmd.Flags().StringVar(&agentUpdateNodeID, "pinned-node-id", "", "Node ID this agent pins to")
	agentUpdateCmd.Flags().StringVar(&agentUpdateRole, "role", "", "Loop role: pm, researcher, devlead, builder, tester, or empty to clear")
	agentUpdateCmd.Flags().BoolVar(&agentUpdateForce, "force", false, "Force role reassignment from current holder; also skips tool validation")

	agentDeleteCmd.Flags().BoolVarP(&agentDeleteYes, "yes", "y", false, "Skip confirmation")

	agentPushCmd.Flags().StringVar(&agentPushProjectID, "project", "", "Project ID to scope the cloud agent to (omit for personal)")
	agentPushCmd.Flags().StringVar(&agentPushAs, "as", "", "Rename the agent in the cluster namespace")
	agentPushCmd.Flags().BoolVar(&agentPushAll, "all", false, "Push every local agent without a cloudAgentId")

	agentPullCmd.Flags().StringVar(&agentPullAs, "as", "", "Local name to pull the cloud agent under")

	agentCmd.AddCommand(agentListCmd, agentCreateCmd, agentUpdateCmd, agentDeleteCmd, agentPushCmd, agentPullCmd)
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
