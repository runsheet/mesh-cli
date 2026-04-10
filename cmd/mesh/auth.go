package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	baseURLFlag string
)

func init() {
	authCmd.AddCommand(authLoginCmd, authTokenLoginCmd, authWhoamiCmd, authLogoutCmd)
	authLoginCmd.Flags().StringVar(&baseURLFlag, "baseUrl", "https://api.projectmesh.io", "Base URL of the mesh API")
	authTokenLoginCmd.Flags().StringVar(&baseURLFlag, "baseUrl", "https://api.projectmesh.io", "Base URL of the mesh API")
}

var authLoginCmd = &cobra.Command{
	Use:   "login [baseUrl]",
	Short: "Login to mesh with browser-based flow (stubbed)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := "https://api.projectmesh.io"
		if len(args) == 1 {
			url = args[0]
		}
		fmt.Printf("Pretend to open browser to login at %s\n", url)
		// Stubbed: In real, open browser and handle callback
		// Here, we simulate a successful login by manual token entry
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Enter access token: ")
		token, _ := reader.ReadString('\n')
		token = strings.TrimSpace(token)

		sess := &Session{
			AccessToken: token,
			BaseURL:     url,
			CreatedAt:   time.Now(),
		}
		if err := SaveSession(sess); err != nil {
			return err
		}
		fmt.Println("Login successful")
		return nil
	},
}

var authTokenLoginCmd = &cobra.Command{
	Use:   "token-login [baseUrl] <accessToken>",
	Short: "Login using an access token",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := "https://api.projectmesh.io"
		if len(args) == 2 {
			url = args[0]
			token := args[1]
			sess := &Session{
				AccessToken: token,
				BaseURL:     url,
				CreatedAt:   time.Now(),
			}
			if err := SaveSession(sess); err != nil {
				return err
			}
			fmt.Println("Token login successful")
			return nil
		} else {
			token := args[0]
			sess := &Session{
				AccessToken: token,
				BaseURL:     url,
				CreatedAt:   time.Now(),
			}
			if err := SaveSession(sess); err != nil {
				return err
			}
			fmt.Println("Token login successful")
			return nil
		}
	},
}

var authWhoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Show current logged-in user",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := RequireSession()
		if err != nil {
			return err
		}
		fmt.Printf("Logged in with token: %s\n", sess.AccessToken)
		fmt.Printf("API Base URL: %s\n", sess.BaseURL)
		return nil
	},
}

var authLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Log out and clear session",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := ClearSession(); err != nil {
			return err
		}
		fmt.Println("Logged out, session cleared")
		return nil
	},
}
