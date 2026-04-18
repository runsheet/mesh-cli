package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "List cached templates (alias of `templates ls`)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImagesList()
	},
}

// templatesCmd is the noun form; `mesh templates ls` is the canonical long
// spelling, `mesh images` is the Docker-style short alias. Both share output.
var templatesCmd = &cobra.Command{
	Use:   "templates",
	Short: "Inspect the local template cache",
}

var templatesLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List cached templates",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImagesList()
	},
}

func runImagesList() error {
	client, err := NewLocalClient()
	if err != nil {
		return err
	}
	entries, err := client.ListTemplates(context.Background())
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", src, ref, ShortSHA(e.SHA), last, e.Agents)
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
