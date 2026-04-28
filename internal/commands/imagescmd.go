package commands

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	itpl "projectmesh.io/mesh-cli/internal/template"
)

var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "List cached templates (alias of `templates ls`)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImagesList()
	},
}

var templatesCmd = &cobra.Command{
	Use:     "templates",
	Aliases: []string{"template"},
	Short:   "Inspect the local template cache",
}

var templatesLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List cached templates",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImagesList()
	},
}

func runImagesList() error {
	lc, err := newLocalClient()
	if err != nil {
		return err
	}
	entries, err := lc.listTemplates(context.Background())
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SOURCE\tREF\tSHA\tLAST USED\tAGENTS")
	for _, e := range entries {
		src := e.Source
		if src == "" {
			src = "-"
		}
		ref := e.Ref
		if ref == "" {
			ref = "-"
		}
		last := "-"
		if !e.LastUsed.IsZero() {
			last = shortDuration(time.Since(e.LastUsed)) + " ago"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", src, ref, itpl.ShortSHA(e.SHA), last, e.Agents)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("No templates cached. Use `mesh pull <ref>` to fetch one.")
	}
	return nil
}

func init() {
	templatesCmd.AddCommand(templatesLsCmd)
}
