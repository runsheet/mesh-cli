package commands

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/channel"
	"projectmesh.io/mesh-cli/internal/node"
)

var channelSockFlag string

var channelCmd = &cobra.Command{
	Use:    "channel",
	Short:  "The mesh channel server (spawned by claude, not for hand use)",
	Hidden: true,
}

var channelServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the mesh channel on stdio and bridge to the node's hub socket",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := node.LoadConfig()
		if err != nil {
			return err
		}
		gw, err := platformClient()
		if err != nil {
			return err
		}
		sock := channelSockFlag
		if sock == "" {
			if sock, err = channel.SockPath(); err != nil {
				return err
			}
		}
		logger := log.New(os.Stderr, "mesh channel: ", log.LstdFlags)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		scopeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		scopes, err := node.ResolveScopes(scopeCtx, gw, cfg)
		cancel()
		if err != nil {
			return err
		}
		h := &channel.Handler{GW: gw, Cfg: cfg, Scopes: scopes, Log: logger}
		srv := &channel.Server{Instructions: channel.Instructions, Tools: h.Tools(), Channel: true}
		var conn *channel.Conn
		h.OnDone = func(itemID, outcome string) {
			if conn != nil {
				if err := conn.Done(itemID, outcome); err != nil {
					logger.Printf("hub: %v", err)
				}
			}
		}
		go func() {
			for ctx.Err() == nil {
				c, err := channel.Dial(ctx, sock, func(content string, meta map[string]string) {
					if id := meta["session_id"]; id != "" {
						h.SetSessionID(id)
					}
					if err := srv.Push(content, meta); err != nil {
						logger.Printf("push: %v", err)
					}
				})
				if err != nil {
					return
				}
				conn = c
				logger.Printf("bridged to %s", sock)
				<-ctx.Done()
			}
		}()
		if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
			return fmt.Errorf("channel: %w", err)
		}
		return nil
	},
}

func init() {
	channelServeCmd.Flags().StringVar(&channelSockFlag, "sock", "", "the node hub's socket (default ~/.mesh/node.sock)")
	channelCmd.AddCommand(channelServeCmd)
}
