package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var psAllFlag bool

var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "List local agents known to the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := NewLocalClient()
		if err != nil {
			return err
		}
		sessions, err := client.ListSessions(context.Background(), psAllFlag)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTEMPLATE\tSTATUS\tUPTIME\tSESSION")
		for _, s := range sessions {
			uptime := "-"
			if s.StartedAt != "" {
				if t, err := time.Parse(time.RFC3339, s.StartedAt); err == nil {
					uptime = shortDuration(time.Since(t))
				}
			}
			sid := s.SessionID
			if sid == "" {
				sid = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.TemplateSource, s.Status, uptime, sid)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if len(sessions) == 0 {
			fmt.Println("No agents defined. Use `mesh run --template <path> --name <name>` to create one.")
		}
		return nil
	},
}

func init() {
	psCmd.Flags().BoolVarP(&psAllFlag, "all", "a", false, "Include stopped/archived agents")
}

func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}
