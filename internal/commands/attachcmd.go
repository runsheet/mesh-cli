package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	iclient "projectmesh.io/mesh-cli/internal/client"
)

var attachCmd = &cobra.Command{
	Use:   "attach <name>",
	Short: "Attach to a running local session (Ctrl-P Ctrl-Q to detach)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		lc, err := newLocalClient()
		if err != nil {
			return err
		}

		iclient.ResetDetachState()

		stdinFd := int(os.Stdin.Fd())
		isTTY := term.IsTerminal(stdinFd)

		var onDetach func() bool
		if isTTY {
			oldState, err := term.MakeRaw(stdinFd)
			if err != nil {
				return fmt.Errorf("raw mode: %w", err)
			}
			defer func() { _ = term.Restore(stdinFd, oldState) }()
			fmt.Fprint(os.Stderr, "\r\n[attached — Ctrl-P Ctrl-Q to detach]\r\n")
			onDetach = func() bool {
				_ = term.Restore(stdinFd, oldState)
				fmt.Fprint(os.Stderr, "\r\n[detached]\r\n")
				return true
			}
		}

		ctx := context.Background()
		err = lc.attach(ctx, name, os.Stdin, os.Stdout, onDetach)
		if err != nil && !isTTY {
			return err
		}
		if err != nil && !iclient.DetachTripped() {
			return err
		}
		return nil
	},
}
