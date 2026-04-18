package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var attachCmd = &cobra.Command{
	Use:   "attach <name>",
	Short: "Attach to a running local session (Ctrl-P Ctrl-Q to detach)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		client, err := NewLocalClient()
		if err != nil {
			return err
		}

		resetDetachState()

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
				// Restore terminal immediately so the post-detach prompt
				// isn't smashed.
				_ = term.Restore(stdinFd, oldState)
				fmt.Fprint(os.Stderr, "\r\n[detached]\r\n")
				return true
			}
		}

		ctx := context.Background()
		err = client.Attach(ctx, name, os.Stdin, os.Stdout, onDetach)
		if err != nil && !isTTY {
			return err
		}
		// On TTY, detach/EOF are both "clean exit" from the user's POV.
		if err != nil && !detachTrippedF {
			return err
		}
		return nil
	},
}
