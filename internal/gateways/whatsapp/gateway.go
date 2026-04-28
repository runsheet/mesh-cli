package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // SQLite driver for whatsmeow

	"projectmesh.io/mesh-cli/internal/accounts"
	"projectmesh.io/mesh-cli/internal/ipc"
	"projectmesh.io/mesh-cli/internal/paths"
)

// shimConn represents one router connection subscribed to a single JID.
type shimConn struct {
	conn    net.Conn
	writeMu sync.Mutex // guards wr; multiple goroutines may write concurrently
	wr      *ipc.Writer
	jid     string // subscribed chatJid
	agent   string // subscribing agent name (for conflict error messages)
}

// GroupEntry is the wire shape for a single group in a list-groups-resp.
type GroupEntry struct {
	JID     string `json:"jid"`
	Subject string `json:"subject"`
}

// Gateway holds runtime state for one WhatsApp gateway process.
type Gateway struct {
	account string

	mu         sync.Mutex
	client     *whatsmeow.Client
	status     string
	msgIn      int
	msgOut     int
	jidIndex   map[string]*shimConn // chatJid → shimConn
	downloadFn func(ctx context.Context, client *whatsmeow.Client, msg whatsmeow.DownloadableMessage) ([]byte, error)
}

func newGateway(account string) *Gateway {
	return &Gateway{
		account:  account,
		status:   "connecting",
		jidIndex: map[string]*shimConn{},
		downloadFn: func(ctx context.Context, client *whatsmeow.Client, msg whatsmeow.DownloadableMessage) ([]byte, error) {
			return client.Download(ctx, msg)
		},
	}
}

// Run is the entry point for the WhatsApp gateway process.
func Run(account string) error {
	logf := gatewayLogf(account)
	logf("whatsapp gateway start account=%s pid=%d", account, os.Getpid())

	storePath, err := accounts.WhatsappStorePath(account)
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

	gw := newGateway(account)
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

	client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.Message:
			gw.handleInbound(v, logf)
		case *events.Connected:
			gw.mu.Lock()
			gw.status = "connected"
			gw.mu.Unlock()
			logf("whatsapp connected")
			if client.Store.PushName == "" {
				client.Store.PushName = account
				if err := client.Store.Save(context.Background()); err != nil {
					logf("save pushname err: %v", err)
				}
			}
			go func() {
				if err := client.SendPresence(context.Background(), types.PresenceAvailable); err != nil {
					logf("send-presence available err: %v", err)
				}
			}()
			if client.Store.ID != nil {
				meta, _ := accounts.LoadWhatsappAccountMeta(account)
				if meta.PhoneNumber == "" {
					meta.PhoneNumber = client.Store.ID.User
					_ = accounts.UpdateWhatsappAccountMeta(account, meta)
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

	sockPath, err := paths.WhatsappGatewaySockPath(account)
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

	PruneMedia(account)

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
func (gw *Gateway) handleRouter(ctx context.Context, conn net.Conn, account string, logf func(string, ...any)) {
	defer conn.Close()
	rd := ipc.NewReader(conn)
	wr := ipc.NewWriter(conn)
	var shim *shimConn

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
				_ = wr.Write(ipc.ErrorEnv("bad-subscribe", "routingKey (chatJid) is required"))
				return
			}

			gw.mu.Lock()
			if existing, taken := gw.jidIndex[jid]; taken {
				gw.mu.Unlock()
				_ = wr.Write(ipc.ErrorEnv("routing-key-conflict", "JID "+jid+" already owned by agent "+existing.agent))
				return
			}
			shim = &shimConn{conn: conn, wr: wr, jid: jid, agent: env.Agent}
			gw.jidIndex[jid] = shim
			gw.mu.Unlock()

			logf("subscribe jid=%s agent=%s", jid, env.Agent)
			_ = wr.Write(ipc.AckEnv(jid))
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
				_ = wr.Write(ipc.ErrorEnv("bad-send", "channelId (target JID) is required"))
				continue
			}
			gw.mu.Lock()
			client := gw.client
			gw.mu.Unlock()
			var sendErr error
			if len(env.Attachments) > 0 {
				sendErr = gw.sendMessageWithImage(ctx, targetJID, env.Content, env.Attachments[0], client, logf)
			} else {
				sendErr = gw.sendMessage(ctx, targetJID, env.Content, logf)
			}
			if sendErr != nil {
				logf("send err jid=%s: %v", targetJID, sendErr)
				_ = wr.Write(ipc.ErrorEnv("send-failed", sendErr.Error()))
				continue
			}
			gw.mu.Lock()
			gw.msgOut++
			gw.mu.Unlock()
			_ = wr.Write(ipc.Envelope{V: ipc.Version, Type: "send-ack", ChannelID: targetJID})

		case "list-groups":
			gw.mu.Lock()
			client := gw.client
			gw.mu.Unlock()
			if client == nil {
				_ = wr.Write(ipc.ErrorEnv("not-connected", "whatsapp client not connected"))
				continue
			}
			listCtx, listCancel := context.WithTimeout(ctx, 10*time.Second)
			groups, err := client.GetJoinedGroups(listCtx)
			listCancel()
			if err != nil {
				_ = wr.Write(ipc.ErrorEnv("list-groups-failed", err.Error()+
					"; websocket may be down — check: mesh whatsapp status --account "+account))
				continue
			}
			entries := make([]GroupEntry, 0, len(groups))
			for _, g := range groups {
				entries = append(entries, GroupEntry{
					JID:     g.JID.String(),
					Subject: g.GroupName.Name,
				})
			}
			data, _ := json.Marshal(entries)
			_ = wr.Write(ipc.ListGroupsRespEnv(string(data)))

		case "join-group":
			code := env.RoutingKey
			if code == "" {
				_ = wr.Write(ipc.ErrorEnv("bad-join-group", "routingKey (invite code) is required"))
				continue
			}
			gw.mu.Lock()
			client := gw.client
			gw.mu.Unlock()
			if client == nil {
				_ = wr.Write(ipc.ErrorEnv("not-connected", "whatsapp client not connected"))
				continue
			}
			joinCtx, joinCancel := context.WithTimeout(ctx, 30*time.Second)
			groupJID, err := client.JoinGroupWithLink(joinCtx, code)
			joinCancel()
			if err != nil {
				_ = wr.Write(ipc.ErrorEnv("join-group-failed", err.Error()))
				continue
			}
			subject := ""
			infoCtx, infoCancel := context.WithTimeout(ctx, 10*time.Second)
			info, infoErr := client.GetGroupInfo(infoCtx, groupJID)
			infoCancel()
			if infoErr == nil {
				subject = info.GroupName.Name
			}
			logf("join-group jid=%s subject=%q", groupJID.String(), subject)
			_ = wr.Write(ipc.JoinGroupRespEnv(groupJID.String(), subject))

		default:
			_ = wr.Write(ipc.ErrorEnv("unknown-type", env.Type))
		}
	}
}

// sendMessage sends a text message to the given JID via whatsmeow.
func (gw *Gateway) sendMessage(ctx context.Context, jidStr, content string, logf func(string, ...any)) error {
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

	go func() {
		if err := client.SendChatPresence(
			context.Background(),
			jid,
			types.ChatPresencePaused,
			types.ChatPresenceMediaText,
		); err != nil {
			logf("chat-presence paused err jid=%s: %v", jid, err)
		}
	}()

	_, err = client.SendMessage(ctx, jid, msg)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	logf("sent jid=%s len=%d", jidStr, len(content))
	return nil
}

// sendMessageWithImage uploads att to WhatsApp CDN and sends it as a captioned
// ImageMessage to jidStr. content becomes the caption (may be empty).
func (gw *Gateway) sendMessageWithImage(ctx context.Context, jidStr, content string, att ipc.Attachment, client *whatsmeow.Client, logf func(string, ...any)) error {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return fmt.Errorf("parse JID %q: %w", jidStr, err)
	}
	if client == nil {
		return fmt.Errorf("client not connected")
	}

	fileBytes, err := os.ReadFile(att.Path)
	if err != nil {
		return fmt.Errorf("read attachment: %w", err)
	}

	uploadCtx, uploadCancel := context.WithTimeout(ctx, 60*time.Second)
	defer uploadCancel()
	resp, err := client.Upload(uploadCtx, fileBytes, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("whatsapp upload: %w", err)
	}

	msg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			URL:           proto.String(resp.URL),
			DirectPath:    proto.String(resp.DirectPath),
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(fileBytes))),
			Mimetype:      proto.String(att.MIMEType),
			Caption:       proto.String(content),
		},
	}
	_, err = client.SendMessage(ctx, jid, msg)
	if err != nil {
		return fmt.Errorf("whatsapp send with image: %w", err)
	}
	logf("sent image jid=%s mime=%s len=%d caption=%d", jidStr, att.MIMEType, len(fileBytes), len(content))
	return nil
}

// handleInbound processes an incoming whatsmeow message event and routes it to
// the subscribed router connection for that JID.
func (gw *Gateway) handleInbound(evt *events.Message, logf func(string, ...any)) {
	if evt.Info.IsFromMe {
		return
	}

	chatJID := evt.Info.Chat.String()

	gw.mu.Lock()
	shim := gw.jidIndex[chatJID]
	client := gw.client
	gw.mu.Unlock()

	if shim == nil {
		return
	}

	caption := evt.Message.GetConversation()
	if caption == "" {
		if ext := evt.Message.GetExtendedTextMessage(); ext != nil {
			caption = ext.GetText()
		}
	}
	if caption == "" {
		if img := evt.Message.GetImageMessage(); img != nil {
			caption = img.GetCaption()
		}
	}

	var attachments []ipc.Attachment
	if img := evt.Message.GetImageMessage(); img != nil {
		att := gw.downloadMedia(evt, img, "image", img.GetMimetype(), client, logf)
		attachments = []ipc.Attachment{att}
	} else if aud := evt.Message.GetAudioMessage(); aud != nil {
		att := gw.downloadAudio(evt, aud, client, logf)
		attachments = []ipc.Attachment{att}
	}

	if caption == "" && len(attachments) == 0 {
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

	logf("inbound chatJid=%s sender=%s msgId=%s attachments=%d", chatJID, sender, evt.Info.ID, len(attachments))

	env := ipc.InboundEnvFull(
		chatJID,
		chatJID,
		sender,
		evt.Info.ID,
		evt.Info.Sender.User,
		ts,
		caption,
		attachments,
	)

	shim.writeMu.Lock()
	err := shim.wr.Write(env)
	shim.writeMu.Unlock()
	if err != nil {
		logf("shim write err jid=%s: %v", chatJID, err)
		return
	}

	if client == nil {
		return
	}

	go func() {
		if err := client.MarkRead(
			context.Background(),
			[]types.MessageID{evt.Info.ID},
			time.Now(),
			evt.Info.Chat,
			evt.Info.Sender,
		); err != nil {
			logf("mark-read err jid=%s msgId=%s: %v", chatJID, evt.Info.ID, err)
		}
	}()

	go func() {
		if err := client.SendChatPresence(
			context.Background(),
			evt.Info.Chat,
			types.ChatPresenceComposing,
			types.ChatPresenceMediaText,
		); err != nil {
			logf("chat-presence composing err jid=%s: %v", chatJID, err)
		}
	}()
}

// downloadMedia is the shared download path for image and audio attachments.
// mime must already be normalised (codec parameters stripped).
// attType is "image" or "audio" and becomes ipc.Attachment.Type.
func (gw *Gateway) downloadMedia(
	evt *events.Message,
	msg whatsmeow.DownloadableMessage,
	attType string,
	mime string,
	client *whatsmeow.Client,
	logf func(string, ...any),
) ipc.Attachment {
	path, err := MediaPath(gw.account, evt.Info.ID, mime)
	if err != nil {
		reason := fmt.Sprintf("unsupported media type %s", mime)
		logf("%s skip msgId=%s: %s", attType, evt.Info.ID, reason)
		return ipc.Attachment{Type: attType, MIMEType: mime, FailReason: reason}
	}

	if client == nil {
		return ipc.Attachment{Type: attType, MIMEType: mime, FailReason: "client not connected"}
	}

	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		dlCtx, dlCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dlCancel()
		data, err := gw.downloadFn(dlCtx, client, msg)
		ch <- result{data, err}
	}()

	res := <-ch
	if res.err != nil {
		reason := res.err.Error()
		if res.err == context.DeadlineExceeded {
			reason = "download timeout after 30s"
		}
		logf("%s download err msgId=%s mime=%s: %s", attType, evt.Info.ID, mime, reason)
		return ipc.Attachment{Type: attType, MIMEType: mime, FailReason: reason}
	}

	dir, _ := MediaDir(gw.account)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		reason := "write failed: " + err.Error()
		logf("%s mkdir err: %s", attType, reason)
		return ipc.Attachment{Type: attType, MIMEType: mime, FailReason: reason}
	}
	if err := os.WriteFile(path, res.data, 0o600); err != nil {
		reason := "write failed: " + err.Error()
		logf("%s write err msgId=%s: %s", attType, evt.Info.ID, reason)
		return ipc.Attachment{Type: attType, MIMEType: mime, FailReason: reason}
	}

	logf("%s download ok path=%s", attType, path)
	PruneMedia(gw.account)
	return ipc.Attachment{Type: attType, Path: path, MIMEType: mime}
}

// downloadAudio downloads an audio attachment (voice note or uploaded audio).
// Voice notes arrive with mimetype "audio/ogg; codecs=opus"; the codecs
// parameter is stripped before the mimeToExt lookup. On failure, rawMime
// is preserved in att.MIMEType for diagnostics.
func (gw *Gateway) downloadAudio(evt *events.Message, aud *waE2E.AudioMessage, client *whatsmeow.Client, logf func(string, ...any)) ipc.Attachment {
	rawMime := aud.GetMimetype()
	mime := rawMime
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	att := gw.downloadMedia(evt, aud, "audio", mime, client, logf)
	if att.FailReason != "" {
		att.MIMEType = rawMime
	}
	return att
}

// publishStatus writes the gateway's status.json atomically.
func (gw *Gateway) publishStatus(account string) {
	gw.mu.Lock()
	subs := make([]accounts.GatewaySubStatus, 0, len(gw.jidIndex))
	for jid, shim := range gw.jidIndex {
		subs = append(subs, accounts.GatewaySubStatus{
			Agent:            shim.agent,
			RoutingKey:       jid,
			SubscriptionMode: "channel",
		})
	}
	st := accounts.GatewayStatus{
		Status:        gw.status,
		MessagesIn:    gw.msgIn,
		MessagesOut:   gw.msgOut,
		Subscriptions: subs,
	}
	gw.mu.Unlock()
	_ = accounts.WriteGatewayStatus("whatsapp", account, st)
}

// gatewayLogf returns a logger that writes to the gateway's log file.
func gatewayLogf(account string) func(string, ...any) {
	dir, _ := paths.PlatformAccountDir("whatsapp", account)
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
