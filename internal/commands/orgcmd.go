package commands

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var orgCmd = &cobra.Command{
	Use:   "org",
	Short: "Org management commands",
	Long:  "Create, list, update orgs and inspect org membership.",
}

var orgMembersCmd = &cobra.Command{
	Use:   "members",
	Short: "Org membership commands",
}

// ── org create ────────────────────────────────────────────────────────────────

var orgCreateSlug string

var orgCreateCmd = &cobra.Command{
	Use:     "create <name>",
	Short:   "Create a new org (caller becomes owner)",
	Example: "mesh org create \"Acme Inc\" --slug acme",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		payload := map[string]any{"name": args[0]}
		if cmd.Flags().Changed("slug") {
			payload["slug"] = orgCreateSlug
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/org/create", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── org list ──────────────────────────────────────────────────────────────────

var orgListCmd = &cobra.Command{
	Use:   "list",
	Short: "List orgs you are a member of",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		var resp struct {
			OK   bool `json:"ok"`
			Orgs []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Slug string `json:"slug"`
				Role string `json:"role"`
			} `json:"orgs"`
		}
		if err := client.doRequest("GET", "/org/list", nil, &resp); err != nil {
			return err
		}
		if len(resp.Orgs) == 0 {
			fmt.Println("no orgs found")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSLUG\tROLE")
		for _, o := range resp.Orgs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", o.ID, o.Name, o.Slug, o.Role)
		}
		return w.Flush()
	},
}

// ── org update ────────────────────────────────────────────────────────────────

var (
	orgUpdateName string
	orgUpdateSlug string
)

var orgUpdateCmd = &cobra.Command{
	Use:     "update <id>",
	Short:   "Update an org's name or slug (owner/admin only)",
	Example: "mesh org update <id> --name \"Acme Corp\" --slug acme-corp",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("name") && !cmd.Flags().Changed("slug") {
			return fmt.Errorf("at least one of --name or --slug is required")
		}
		payload := map[string]any{"id": args[0]}
		if cmd.Flags().Changed("name") {
			payload["name"] = orgUpdateName
		}
		if cmd.Flags().Changed("slug") {
			payload["slug"] = orgUpdateSlug
		}
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)
		var resp map[string]any
		if err := client.doRequest("POST", "/org/update", payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── org members list ──────────────────────────────────────────────────────────

var orgMembersListCmd = &cobra.Command{
	Use:   "list <org-id>",
	Short: "List members of an org",
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
		if err := client.doRequest("GET", "/org/members/list?orgId="+args[0], nil, &resp); err != nil {
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

func init() {
	orgCreateCmd.Flags().StringVar(&orgCreateSlug, "slug", "", "Custom slug (defaults to a slugified name)")
	orgUpdateCmd.Flags().StringVar(&orgUpdateName, "name", "", "New org name")
	orgUpdateCmd.Flags().StringVar(&orgUpdateSlug, "slug", "", "New org slug")

	orgMembersCmd.AddCommand(orgMembersListCmd)
	orgCmd.AddCommand(orgCreateCmd, orgListCmd, orgUpdateCmd, orgMembersCmd)
}
