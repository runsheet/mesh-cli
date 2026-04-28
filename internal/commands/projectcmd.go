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

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List projects you are a member of",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		var resp struct {
			OK       bool `json:"ok"`
			Projects []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Slug        string `json:"slug"`
				UpdatedAt   string `json:"updated_at"`
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
				UserID    string `json:"user_id"`
				Role      string `json:"role"`
				JoinedAt  string `json:"joined_at"`
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

func init() {
	projectCreateCmd.Flags().StringVar(&projectCreateDescription, "description", "", "Project description")
	projectUpdateCmd.Flags().StringVar(&projectUpdateName, "name", "", "New project name")
	projectUpdateCmd.Flags().StringVar(&projectUpdateDescription, "description", "", "New project description")
	projectDeleteCmd.Flags().BoolVar(&projectDeleteYes, "yes", false, "Skip confirmation prompt")
	membersAddCmd.Flags().StringVar(&membersAddRole, "role", "member", "Role to assign: owner, admin, member, viewer")

	membersCmd.AddCommand(membersAddCmd, membersListCmd, membersRemoveCmd)
	projectCmd.AddCommand(projectCreateCmd, projectListCmd, projectShowCmd, projectUpdateCmd, projectDeleteCmd, membersCmd)
}
