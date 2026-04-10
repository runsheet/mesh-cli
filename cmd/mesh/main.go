package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "mesh",
		Short: "Mesh CLI - Go Implementation",
	}

	rootCmd.AddCommand(authCmd, workItemCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authentication commands",
}

func init() {
	// Initialize auth commands
}
