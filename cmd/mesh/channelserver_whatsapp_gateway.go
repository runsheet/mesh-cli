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

	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // SQLite driver for whatsmeow
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

// whatsappShimConn represents one router connection subscribed to a single JID.
type whatsappShimConn struct {
	conn  net.Conn
	wr    *ipcWriter
	jid   string // subscribed chatJid
	agent string // subscribing agent name (for conflict error messages)
}

// whatsappGateway holds runtime state for one WhatsApp gateway process.
type whatsappGateway struct {
	account string

	mu       sync.Mutex
	client   *whatsmeow.Client
	status   string
	msgIn    int
	msgOut   int
	jidIndex map[string]*whatsappShimConn // chatJid → shimConn
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

	container, err := sqlstore.New(context.Background(), "sqlite", "file:"+storePath+"?_pragma=foreign_keys(1)", waLog.Noop)
	if err != nil {
		return fmt.Errorf("open whatsapp store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return fmt.Errorf("get device: %w", err)
	}

	gw := newWhatsappGateway(account)
	client := whatsmeow.NewClient(deviceStore, waLog.Noop)
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

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			gw.handleInbound(v, logf)
		case *events.Connected:
			gw.mu.Lock()
			gw.status = "connected"
			gw.mu.Unlock()
			logf("whatsapp connected")
			if client.Store.ID != nil {
				meta, _ := LoadWhatsappAccountMeta(account)
				if meta.PhoneNumber == "" {
					meta.PhoneNumber = client.Store.ID.User
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
			logf("whatsapp logged-out (device revoked on phone)")
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

// handleRouter serves one router connection until it closes or ctx is cancelled.
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
			targetJID := env.ChannelID
			if targetJID == "" {
				targetJID = env.RoutingKey
			}
			if targetJID == "" {
				_ = wr.Write(ipcErrorEnv("bad-send", "channelId (target JID) is required"))
				continue
			}
			if err := gw.sendMessage(ctx, targetJID, env.Content, logf); err != nil {
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
func (gw *whatsappGateway) sendMessage(ctx context.Context, jidStr, content string, logf func(string, ...any)) error {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return fmt.Errorf("parse JID %q: %w", jidStr, err)
	}

	msg := &waE2E.Message{
		Conversation: proto.String(content),
	}

	gw.mu.Lock()
	client := gw.client
	gw.mu.Unlock()
	if client == nil {
		return fmt.Errorf("client not connected")
	}

	_, err = client.SendMessage(ctx, jid, msg)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	logf("sent jid=%s len=%d", jidStr, len(content))
	return nil
}

// handleInbound processes an incoming whatsmeow message event and routes it to
// the subscribed router connection for that JID.
func (gw *whatsappGateway) handleInbound(evt *events.Message, logf func(string, ...any)) {
	// Self-filter: skip messages sent from the paired device.
	if evt.Info.IsFromMe {
		return
	}

	chatJID := evt.Info.Chat.String()

	gw.mu.Lock()
	shim := gw.jidIndex[chatJID]
	gw.mu.Unlock()

	if shim == nil {
		return
	}

	// Extract text content; skip non-text in slice 2.
	content := evt.Message.GetConversation()
	if content == "" {
		if ext := evt.Message.GetExtendedTextMessage(); ext != nil {
			content = ext.GetText()
		}
	}
	if content == "" {
		return
	}

	sender := evt.Info.Sender.String()
	if evt.Info.PushName != "" {
		sender = evt.Info.PushName
	}
	ts := evt.Info.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")

	gw.mu.Lock()
	gw.msgIn++
	gw.mu.Unlock()

	logf("inbound chatJid=%s sender=%s msgId=%s", chatJID, sender, evt.Info.ID)

	env := ipcInboundEnvFull(
		chatJID,
		chatJID,
		sender,
		evt.Info.ID,
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
	for jid, shim := range gw.jidIndex {
		subs = append(subs, GatewaySubStatus{
			Agent:            shim.agent,
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
