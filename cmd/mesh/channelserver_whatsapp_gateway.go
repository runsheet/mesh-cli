package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // SQLite driver for whatsmeow

	"github.com/spf13/cobra"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var whatsappGatewayAccountFlag string

var channelServerWhatsappGatewayCmd = &cobra.Command{
	Use:   "whatsapp-gateway",
	Short: "WhatsApp gateway (daemon-supervised; one per account)",
	Long: "whatsapp-gateway holds the whatsmeow client for a paired WhatsApp account " +
		"and serves router connections over gateway.sock. " +
		"Normally spawned by the mesh daemon's ChannelManager.",
	RunE: func(cmd *cobra.Command, args []string) error {
		account := strings.TrimSpace(whatsappGatewayAccountFlag)
		if account == "" {
			return fmt.Errorf("--account is required")
		}
		return runWhatsappGateway(account)
	},
}

func init() {
	channelServerWhatsappGatewayCmd.Flags().StringVar(&whatsappGatewayAccountFlag, "account", "", "WhatsApp account name (required)")
	channelServerCmd.AddCommand(channelServerWhatsappGatewayCmd)
}

// whatsappShimConn represents one router connection (one subscribed JID).
type whatsappShimConn struct {
	conn  net.Conn
	wr    *ipcWriter
	jid   string // subscribed chatJid
	agent string // subscribing agent name (for conflict error messages)
}

// whatsappGateway holds runtime state for one WhatsApp gateway process.
type whatsappGateway struct {
	account string

	mu      sync.Mutex
	client  *whatsmeow.Client
	status  string
	msgIn   int
	msgOut  int
	// jidIndex maps chatJid → shimConn (one router per JID)
	jidIndex map[string]*whatsappShimConn
}

func newWhatsappGateway(account string) *whatsappGateway {
	return &whatsappGateway{
		account:  account,
		status:   "connecting",
		jidIndex: map[string]*whatsappShimConn{},
	}
}

func runWhatsappGateway(account string) error {
	logf := whatsappGatewayLogf(account)
	logf("whatsapp gateway start account=%s pid=%d", account, os.Getpid())

	storePath, err := WhatsappStorePath(account)
	if err != nil {
		return fmt.Errorf("store path: %w", err)
	}

	dbLog := waLog.Noop
	container, err := sqlstore.New(context.Background(), "sqlite", "file:"+storePath+"?_foreign_keys=on", dbLog)
	if err != nil {
		return fmt.Errorf("open whatsapp store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return fmt.Errorf("get device: %w", err)
	}

	gw := newWhatsappGateway(account)

	clientLog := waLog.Noop
	client := whatsmeow.NewClient(deviceStore, clientLog)
	gw.client = client

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("signal %s received, shutting down", sig)
		cancel()
	}()

	// Register event handler for inbound messages.
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			gw.handleInbound(v, logf)
		case *events.Connected:
			gw.mu.Lock()
			gw.status = "connected"
			gw.mu.Unlock()
			logf("whatsapp connected")
			meta, _ := LoadWhatsappAccountMeta(account)
			if client.Store.ID != nil {
				phone := client.Store.ID.User
				if meta.PhoneNumber == "" {
					meta.PhoneNumber = phone
					_ = UpdateWhatsappAccountMeta(account, meta)
				}
			}
			gw.publishStatus(account)
		case *events.Disconnected:
			gw.mu.Lock()
			gw.status = "disconnected"
			gw.mu.Unlock()
			logf("whatsapp disconnected")
			gw.publishStatus(account)
		case *events.LoggedOut:
			gw.mu.Lock()
			gw.status = "logged-out"
			gw.mu.Unlock()
			logf("whatsapp logged-out (device revoked)")
			gw.publishStatus(account)
		}
	})

	if err := client.Connect(); err != nil {
		gw.mu.Lock()
		gw.status = "error: " + err.Error()
		gw.mu.Unlock()
		gw.publishStatus(account)
		return fmt.Errorf("whatsapp connect: %w", err)
	}
	defer client.Disconnect()

	// Listen on gateway.sock for router connections.
	sockPath, err := WhatsappGatewaySockPath(account)
	if err != nil {
		return fmt.Errorf("gateway sock path: %w", err)
	}
	_ = os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	defer func() {
		listener.Close()
		_ = os.Remove(sockPath)
	}()
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return fmt.Errorf("chmod gateway.sock: %w", err)
	}
	logf("gateway.sock listening at %s", sockPath)

	// Status publisher goroutine.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		gw.publishStatus(account)
		for {
			select {
			case <-ctx.Done():
				gw.publishStatus(account)
				return
			case <-ticker.C:
				gw.publishStatus(account)
			}
		}
	}()

	// Accept router connections.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logf("accept error: %v", err)
				continue
			}
			go gw.handleRouter(ctx, conn, account, logf)
		}
	}()

	<-ctx.Done()
	logf("whatsapp gateway shutting down")
	return nil
}

// handleRouter serves one router connection until it closes or ctx is done.
func (gw *whatsappGateway) handleRouter(ctx context.Context, conn net.Conn, account string, logf func(string, ...any)) {
	defer conn.Close()
	rd := newIPCReader(conn)
	wr := newIPCWriter(conn)
	var shim *whatsappShimConn

	defer func() {
		if shim == nil {
			return
		}
		gw.mu.Lock()
		if gw.jidIndex[shim.jid] == shim {
			delete(gw.jidIndex, shim.jid)
		}
		gw.mu.Unlock()
		logf("router disconnected jid=%s", shim.jid)
		gw.publishStatus(account)
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		env, err := rd.Read()
		if err != nil {
			return
		}

		switch env.Type {
		case "subscribe":
			// RoutingKey is the chatJid for WhatsApp.
			jid := env.RoutingKey
			if jid == "" {
				_ = wr.Write(ipcErrorEnv("bad-subscribe", "routingKey (chatJid) is required"))
				return
			}

			gw.mu.Lock()
			if existing, taken := gw.jidIndex[jid]; taken {
				gw.mu.Unlock()
				_ = wr.Write(ipcErrorEnv("routing-key-conflict", "JID "+jid+" already owned by agent "+existing.agent))
				return
			}
			shim = &whatsappShimConn{conn: conn, wr: wr, jid: jid, agent: env.Agent}
			gw.jidIndex[jid] = shim
			gw.mu.Unlock()

			logf("subscribe jid=%s agent=%s", jid, env.Agent)
			_ = wr.Write(ipcAckEnv(jid))
			gw.publishStatus(account)

		case "unsubscribe":
			if shim != nil && shim.jid == env.RoutingKey {
				gw.mu.Lock()
				if gw.jidIndex[shim.jid] == shim {
					delete(gw.jidIndex, shim.jid)
				}
				gw.mu.Unlock()
				logf("unsubscribe jid=%s", env.RoutingKey)
				gw.publishStatus(account)
				shim = nil
			}

		case "send":
			// ChannelID is the destination JID.
			targetJID := env.ChannelID
			if targetJID == "" {
				targetJID = env.RoutingKey
			}
			if targetJID == "" {
				_ = wr.Write(ipcErrorEnv("bad-send", "channelId (target JID) is required"))
				continue
			}
			if err := gw.sendMessage(targetJID, env.Content, logf); err != nil {
				logf("send err jid=%s: %v", targetJID, err)
				_ = wr.Write(ipcErrorEnv("send-failed", err.Error()))
				continue
			}
			gw.mu.Lock()
			gw.msgOut++
			gw.mu.Unlock()
			_ = wr.Write(ipcEnvelope{V: ipcVersion, Type: "send-ack", ChannelID: targetJID})

		default:
			_ = wr.Write(ipcErrorEnv("unknown-type", env.Type))
		}
	}
}

// sendMessage sends a text message to the given JID via whatsmeow.
func (gw *whatsappGateway) sendMessage(jidStr, content string, logf func(string, ...any)) error {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return fmt.Errorf("parse JID %q: %w", jidStr, err)
	}

	msg := &waProto.Message{
		Conversation: proto.String(content),
	}

	gw.mu.Lock()
	client := gw.client
	gw.mu.Unlock()
	if client == nil {
		return fmt.Errorf("client not connected")
	}

	_, err = client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	logf("sent jid=%s len=%d", jidStr, len(content))
	return nil
}

// handleInbound processes an incoming whatsmeow message event and routes it to
// the subscribed router connection, if any.
func (gw *whatsappGateway) handleInbound(evt *events.Message, logf func(string, ...any)) {
	// Self-filter: drop messages sent by the paired device.
	gw.mu.Lock()
	client := gw.client
	gw.mu.Unlock()
	if client != nil && client.Store.ID != nil {
		// Messages from self have IsFromMe set.
		if evt.Info.IsFromMe {
			return
		}
	}

	// Determine the chat JID. For groups, use the group JID; for DMs, use the
	// sender JID in the s.whatsapp.net domain.
	chatJID := evt.Info.Chat.String()

	gw.mu.Lock()
	shim := gw.jidIndex[chatJID]
	gw.mu.Unlock()

	if shim == nil {
		return
	}

	// Extract text content.
	content := ""
	if evt.Message.GetConversation() != "" {
		content = evt.Message.GetConversation()
	} else if ext := evt.Message.GetExtendedTextMessage(); ext != nil {
		content = ext.GetText()
	}
	if content == "" {
		return // non-text message; skip in slice 2
	}

	sender := evt.Info.Sender.String()
	pushName := evt.Info.PushName
	if pushName != "" {
		sender = pushName
	}
	msgID := evt.Info.ID
	ts := evt.Info.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")

	gw.mu.Lock()
	gw.msgIn++
	gw.mu.Unlock()

	logf("inbound chatJid=%s sender=%s msgId=%s", chatJID, sender, msgID)

	env := ipcInboundEnvFull(
		chatJID,
		chatJID,
		sender,
		msgID,
		evt.Info.Sender.User,
		ts,
		content,
	)
	if err := shim.wr.Write(env); err != nil {
		logf("shim write err jid=%s: %v", chatJID, err)
	}
}

// publishStatus writes the gateway's status.json atomically.
func (gw *whatsappGateway) publishStatus(account string) {
	gw.mu.Lock()
	subs := make([]GatewaySubStatus, 0, len(gw.jidIndex))
	for jid := range gw.jidIndex {
		subs = append(subs, GatewaySubStatus{
			RoutingKey:       jid,
			SubscriptionMode: "channel",
		})
	}
	st := GatewayStatus{
		Status:        gw.status,
		MessagesIn:    gw.msgIn,
		MessagesOut:   gw.msgOut,
		Subscriptions: subs,
	}
	gw.mu.Unlock()
	_ = WriteGatewayStatus("whatsapp", account, st)
}

func whatsappGatewayLogf(account string) func(string, ...any) {
	home, _ := os.UserHomeDir()
	dir := fmt.Sprintf("%s/.mesh/whatsapp-accounts/%s", home, account)
	_ = os.MkdirAll(dir, 0o700)
	path := fmt.Sprintf("%s/gateway-%d.log", dir, os.Getpid())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "[whatsapp-gateway] "+format+"\n", args...)
		}
	}
	return func(format string, args ...any) {
		fmt.Fprintf(f, "["+time.Now().UTC().Format(time.RFC3339Nano)+"] "+format+"\n", args...)
	}
}
