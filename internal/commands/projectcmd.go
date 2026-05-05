package commands

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var allowedProjectRoles = map[string]bool{
	"owner":  true,
	"admin":  true,
	"member": true,
	"viewer": true,
}

var projectCmd = &cobra.Command{
	Use:     "project",
	Aliases: []string{"proj"},
	Short:   "Project management commands",
	Long:    "Create, inspect, update, and delete projects. Manage project membership.",
}

var membersCmd = &cobra.Command{
	Use:   "members",
	Short: "Project membership commands",
}

// ── project create ────────────────────────────────────────────────────────────

var projectCreateDescription string

var projectCreateCmd = &cobra.Command{
	Use:     "create <name>",
	Short:   "Create a new project",
	Example: "mesh project create \"My Project\" --description \"Smoke test\"",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		payload := map[string]any{"name": args[0]}
		if v := strings.TrimSpace(projectCreateDescription); v != "" {
			payload["description"] = v
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/project/create", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── project list ──────────────────────────────────────────────────────────────

var projectListOrg string

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List projects you are a member of",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		if cmd.Flags().Changed("org") {
			client.OrgSlug = projectListOrg
		}
		var resp struct {
			OK       bool `json:"ok"`
			Projects []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Slug      string `json:"slug"`
				UpdatedAt string `json:"updated_at"`
			} `json:"projects"`
		}
		if err := client.doRequest("GET", "/project/list", nil, &resp); err != nil {
			return err
		}
		if len(resp.Projects) == 0 {
			fmt.Println("no projects found")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSLUG\tUPDATED")
		for _, p := range resp.Projects {
			updated := p.UpdatedAt
			if len(updated) > 16 {
				updated = updated[:16]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.ID, p.Name, p.Slug, updated)
		}
		return w.Flush()
	},
}

// ── project show ──────────────────────────────────────────────────────────────

var projectShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show a project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("GET", "/project/"+args[0], nil, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── project update ────────────────────────────────────────────────────────────

var (
	projectUpdateName        string
	projectUpdateDescription string
)

var projectUpdateCmd = &cobra.Command{
	Use:     "update <id>",
	Short:   "Update a project's name or description",
	Example: "mesh project update <id> --name \"Renamed\" --description \"New desc\"",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("name") && !cmd.Flags().Changed("description") {
			return fmt.Errorf("at least one of --name or --description is required")
		}
		payload := map[string]any{"id": args[0]}
		if cmd.Flags().Changed("name") {
			payload["name"] = projectUpdateName
		}
		if cmd.Flags().Changed("description") {
			payload["description"] = projectUpdateDescription
		}
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/project/update", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── project delete ────────────────────────────────────────────────────────────

var projectDeleteYes bool

var projectDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a project (must be empty)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		if !projectDeleteYes {
			fmt.Fprintf(os.Stderr, "Delete project %s? This cannot be undone. [y/N]: ", id)
			scanner := bufio.NewScanner(os.Stdin)
			if !scanner.Scan() || strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
				fmt.Fprintln(os.Stderr, "aborted")
				return nil
			}
		}
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("DELETE", "/project/"+id, nil, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── project members add ───────────────────────────────────────────────────────

var membersAddRole string

var membersAddCmd = &cobra.Command{
	Use:     "add <project-id> <email>",
	Short:   "Add a member to a project by email",
	Example: "mesh project members add <project-id> colleague@example.com --role member",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		role := strings.TrimSpace(membersAddRole)
		if !allowedProjectRoles[role] {
			choices := []string{"owner", "admin", "member", "viewer"}
			return fmt.Errorf("invalid role %q, expected one of: %s", role, strings.Join(choices, ", "))
		}
		sess, err := requireSession()
		if err != nil {
			return err
		}
		payload := map[string]any{
			"projectId": args[0],
			"email":     args[1],
			"role":      role,
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/project/members/add", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── project members list ──────────────────────────────────────────────────────

var membersListCmd = &cobra.Command{
	Use:   "list <project-id>",
	Short: "List project members",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		var resp struct {
			OK      bool `json:"ok"`
			Members []struct {
				UserID   string `json:"user_id"`
				Role     string `json:"role"`
				JoinedAt string `json:"joined_at"`
			} `json:"members"`
		}
		if err := client.doRequest("GET", "/project/members/list?projectId="+args[0], nil, &resp); err != nil {
			return err
		}
		if len(resp.Members) == 0 {
			fmt.Println("no members found")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "USER_ID\tROLE\tJOINED")
		for _, m := range resp.Members {
			joined := m.JoinedAt
			if len(joined) > 16 {
				joined = joined[:16]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", m.UserID, m.Role, joined)
		}
		return w.Flush()
	},
}

// ── project members remove ────────────────────────────────────────────────────

var membersRemoveCmd = &cobra.Command{
	Use:   "remove <project-id> <user-id>",
	Short: "Remove a member from a project",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		payload := map[string]any{
			"projectId": args[0],
			"userId":    args[1],
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/project/members/remove", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── project roles ─────────────────────────────────────────────────────────────

var projectRolesCmd = &cobra.Command{
	Use:   "roles <project-id>",
	Short: "Show role-to-agent assignments for a project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)

		var resp struct {
			OK        bool   `json:"ok"`
			ProjectID string `json:"projectId"`
			Roles     map[string]*struct {
				AgentID   string `json:"agentId"`
				AgentName string `json:"agentName"`
				NodeID    string `json:"nodeId"`
			} `json:"roles"`
		}
		if err := client.doRequest("GET", "/project/"+args[0]+"/roles", nil, &resp); err != nil {
			return err
		}

		loopRoles := []string{"pm", "researcher", "devlead", "builder", "tester"}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ROLE\tAGENT NAME\tAGENT ID\tNODE ID")
		for _, role := range loopRoles {
			entry := resp.Roles[role]
			if entry == nil {
				fmt.Fprintf(w, "%s\t-\t(unassigned)\t\n", role)
			} else {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", role, entry.AgentName, entry.AgentID, entry.NodeID)
			}
		}
		return w.Flush()
	},
}

// ── project budget ────────────────────────────────────────────────────────────

var budgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Budget management for a project",
}

var (
	budgetSetProject    string
	budgetSetDailyUsd   float64
	budgetSetAlertAt    int
	budgetSetPauseLanes string
)

var budgetSetCmd = &cobra.Command{
	Use:     "set",
	Short:   "Set or update the daily USD budget for a project",
	Example: "mesh project budget set --project <id> --daily-usd 50 --alert-at 80 --pause-lanes ship",
	RunE: func(cmd *cobra.Command, args []string) error {
		if budgetSetProject == "" {
			return fmt.Errorf("--project is required")
		}
		sess, err := requireSession()
		if err != nil {
			return err
		}
		payload := map[string]any{
			"dailyUsd": budgetSetDailyUsd,
		}
		if cmd.Flags().Changed("alert-at") {
			payload["alertAtPct"] = budgetSetAlertAt
		}
		if cmd.Flags().Changed("pause-lanes") {
			var lanes []string
			for _, l := range strings.Split(budgetSetPauseLanes, ",") {
				l = strings.TrimSpace(l)
				if l != "" {
					lanes = append(lanes, l)
				}
			}
			payload["pauseLanes"] = lanes
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/budget/"+budgetSetProject, payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── project digest ────────────────────────────────────────────────────────────

var digestCmd = &cobra.Command{
	Use:   "digest",
	Short: "Digest subscription management for a project",
}

var (
	digestEnableProject string
	digestEnableChannel string
	digestEnableTime    string
	digestEnableTZ      string
)

var digestEnableCmd = &cobra.Command{
	Use:     "enable",
	Short:   "Create or update a digest subscription",
	Example: "mesh project digest enable --project <id> --channel discord:dm/<userid> --time 08:00 --timezone America/New_York",
	RunE: func(cmd *cobra.Command, args []string) error {
		if digestEnableProject == "" || digestEnableChannel == "" {
			return fmt.Errorf("--project and --channel are required")
		}
		channelType, channelTarget, parseErr := parseChannelFlag(digestEnableChannel)
		if parseErr != nil {
			return parseErr
		}
		sess, err := requireSession()
		if err != nil {
			return err
		}
		payload := map[string]any{
			"projectId":     digestEnableProject,
			"channelType":   channelType,
			"channelTarget": channelTarget,
			"sendAtLocal":   digestEnableTime,
			"timezone":      digestEnableTZ,
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/digest/subscriptions", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var digestListProject string

var digestListCmd = &cobra.Command{
	Use:   "list",
	Short: "List digest subscriptions for a project",
	RunE: func(cmd *cobra.Command, args []string) error {
		if digestListProject == "" {
			return fmt.Errorf("--project is required")
		}
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		var resp struct {
			OK            bool `json:"ok"`
			Subscriptions []struct {
				ID            string `json:"id"`
				ChannelType   string `json:"channel_type"`
				ChannelTarget string `json:"channel_target"`
				SendAtLocal   string `json:"send_at_local"`
				Timezone      string `json:"timezone"`
				Enabled       bool   `json:"enabled"`
			} `json:"subscriptions"`
		}
		if err := client.doRequest("GET", "/digest/subscriptions?projectId="+digestListProject, nil, &resp); err != nil {
			return err
		}
		if len(resp.Subscriptions) == 0 {
			fmt.Println("no subscriptions found")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tCHANNEL_TYPE\tTARGET\tSEND_AT\tTIMEZONE\tENABLED")
		for _, s := range resp.Subscriptions {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%v\n",
				s.ID, s.ChannelType, s.ChannelTarget, s.SendAtLocal, s.Timezone, s.Enabled)
		}
		return w.Flush()
	},
}

var digestDisableID string

var digestDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Delete a digest subscription by ID",
	RunE: func(cmd *cobra.Command, args []string) error {
		if digestDisableID == "" {
			return fmt.Errorf("--id is required")
		}
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("DELETE", "/digest/subscriptions/"+digestDisableID, nil, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// parseChannelFlag parses --channel values like:
//
//	discord:dm/<userid>   → channel_type=discord_dm, target=<userid>
//	whatsapp:<e164>       → channel_type=whatsapp, target=<e164>
//	email:<addr>          → channel_type=email, target=<addr>
func parseChannelFlag(ch string) (channelType, channelTarget string, err error) {
	if strings.HasPrefix(ch, "discord:dm/") {
		return "discord_dm", strings.TrimPrefix(ch, "discord:dm/"), nil
	}
	if strings.HasPrefix(ch, "whatsapp:") {
		return "whatsapp", strings.TrimPrefix(ch, "whatsapp:"), nil
	}
	if strings.HasPrefix(ch, "email:") {
		return "email", strings.TrimPrefix(ch, "email:"), nil
	}
	return "", "", fmt.Errorf("invalid --channel format %q; expected discord:dm/<userid>, whatsapp:<e164>, or email:<addr>", ch)
}

func init() {
	projectCreateCmd.Flags().StringVar(&projectCreateDescription, "description", "", "Project description")
	projectListCmd.Flags().StringVar(&projectListOrg, "org", "", "Org slug to scope the listing (overrides session default)")
	projectUpdateCmd.Flags().StringVar(&projectUpdateName, "name", "", "New project name")
	projectUpdateCmd.Flags().StringVar(&projectUpdateDescription, "description", "", "New project description")
	projectDeleteCmd.Flags().BoolVar(&projectDeleteYes, "yes", false, "Skip confirmation prompt")
	membersAddCmd.Flags().StringVar(&membersAddRole, "role", "member", "Role to assign: owner, admin, member, viewer")

	budgetSetCmd.Flags().StringVar(&budgetSetProject, "project", "", "Project ID")
	budgetSetCmd.Flags().Float64Var(&budgetSetDailyUsd, "daily-usd", 0, "Daily budget in USD")
	budgetSetCmd.Flags().IntVar(&budgetSetAlertAt, "alert-at", 80, "Alert threshold percentage (1–99)")
	budgetSetCmd.Flags().StringVar(&budgetSetPauseLanes, "pause-lanes", "ship", "Comma-separated lanes to pause at 100%: ship,research,builder")
	_ = budgetSetCmd.MarkFlagRequired("daily-usd")

	digestEnableCmd.Flags().StringVar(&digestEnableProject, "project", "", "Project ID")
	digestEnableCmd.Flags().StringVar(&digestEnableChannel, "channel", "", "Channel: discord:dm/<userid>, whatsapp:<e164>, email:<addr>")
	digestEnableCmd.Flags().StringVar(&digestEnableTime, "time", "08:00", "Local send time (HH:MM)")
	digestEnableCmd.Flags().StringVar(&digestEnableTZ, "timezone", "UTC", "IANA timezone, e.g. America/New_York")

	digestListCmd.Flags().StringVar(&digestListProject, "project", "", "Project ID")
	digestDisableCmd.Flags().StringVar(&digestDisableID, "id", "", "Subscription ID to delete")

	digestCmd.AddCommand(digestEnableCmd, digestListCmd, digestDisableCmd)
	budgetCmd.AddCommand(budgetSetCmd)

	membersCmd.AddCommand(membersAddCmd, membersListCmd, membersRemoveCmd)
	projectCmd.AddCommand(projectCreateCmd, projectListCmd, projectShowCmd, projectUpdateCmd, projectDeleteCmd, membersCmd, projectRolesCmd, budgetCmd, digestCmd)
}
