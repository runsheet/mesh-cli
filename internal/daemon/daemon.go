// Package daemon implements the mesh node daemon: local Unix-socket HTTP
// surface plus optional outbound gRPC stream to mesh-api.
package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"projectmesh.io/mesh-cli/internal/agentsync"
	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/channels"
	"projectmesh.io/mesh-cli/internal/claudeconf"
	"projectmesh.io/mesh-cli/internal/ipc"
	"projectmesh.io/mesh-cli/internal/localsock"
	"projectmesh.io/mesh-cli/internal/paths"
	"projectmesh.io/mesh-cli/internal/peers"
	"projectmesh.io/mesh-cli/internal/sessions"
	isnap "projectmesh.io/mesh-cli/internal/snapshot"
	"projectmesh.io/mesh-cli/internal/state"
	itpl "projectmesh.io/mesh-cli/internal/template"
	meshv1 "projectmesh.io/mesh-cli/proto/meshv1"
)

const (
	daemonVersion = "dev"

	heartbeatInterval        = 30 * time.Second
	reconnectInitialBackoff  = 1 * time.Second
	reconnectMaxBackoff      = 30 * time.Second
	nodeStreamKeepaliveTimer = 60 * time.Second
)

// RunDaemon starts the node daemon. Two surfaces are served:
//
//   - The local Unix socket (~/.mesh/daemon.sock) — always on, drives
//     `mesh run`/`ps`/`attach`/...  This is the "standalone" path; no cluster
//     account required.
//   - The outbound gRPC stream to mesh-api — only when ~/.mesh/node-token
//     exists. Adds cluster features (AgentSync, TaskPoke, portal session
//     streaming) alongside the local surface.
//
// Both paths share a single SessionRegistry so locally-run and cluster-pinned
// agents are observable through either surface.
func RunDaemon(baseURL string, grpcAddr string, probeNodeID func(baseURL, token string) (string, error)) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registry := sessions.NewSessionRegistry()

	itpl.PruneTemplates()
	migrateLegacyAgentPromptFiles()
	sessions.MigrateAwayFromPluginScaffold()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve mesh binary: %w", err)
	}

	claudeconf.MigrateMCPSettingsToMesh(exe)

	broker := peers.New()

	// Load the node token to decide whether cluster features are enabled.
	// The Phase 2 dispatcher rebuild (DSP-01..07) is the new wiring point for
	// per-agent task delivery; the prior channel-routing supervisor was removed
	// in Plan 00-05 (INV-02 architectural cut).
	token, tokenErr := auth.LoadNodeToken()
	clustered := tokenErr == nil && strings.TrimSpace(token) != ""
	if tokenErr != nil && !errors.Is(tokenErr, os.ErrNotExist) {
		return fmt.Errorf("read node token: %w", tokenErr)
	}

	listener, err := localsock.StartLocalSocket(ctx, registry, broker, localsock.Deps{
		WritePromptFile: writeAgentPromptFile,
		ProbeNodeID:     probeNodeID,
		AutoPushAgent: func(agentName string) {
			tok, err := auth.LoadNodeToken()
			if err != nil || strings.TrimSpace(tok) == "" {
				return
			}
			if err := agentsync.AdoptOne(baseURL, tok, agentName); err != nil {
				fmt.Printf("mesh auto-push new agent %s: %v\n", agentName, err)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("start local socket: %w", err)
	}
	defer listener.Close()

	autoStartAgents(registry)

	if !clustered {
		fmt.Println("mesh node daemon running in standalone mode (no node token present; cluster features disabled)")
		<-ctx.Done()
		return ctx.Err()
	}

	return runClusteredDaemon(ctx, baseURL, grpcAddr, token, registry)
}

func autoStartAgents(registry *sessions.SessionRegistry) {
	agents, err := state.ListAgents()
	if err != nil {
		fmt.Printf("mesh autostart: load state: %v\n", err)
		return
	}
	if len(agents) == 0 {
		return
	}
	started, skipped := 0, 0
	for _, a := range agents {
		if err := autoStartOne(registry, a); err != nil {
			fmt.Printf("mesh autostart: skip %s: %v\n", a.Name, err)
			skipped++
			continue
		}
		fmt.Printf("mesh autostart: started %s\n", a.Name)
		started++
	}
	fmt.Printf("mesh autostart: %d started, %d skipped\n", started, skipped)
}

func autoStartOne(registry *sessions.SessionRegistry, a *state.AgentDef) error {
	if !isnap.Exists(a.Name) {
		if migErr := migrateAgentSnapshot(a); migErr != nil {
			return fmt.Errorf("snapshot missing and migration failed (%w) — run `mesh update %s` to rebuild or `mesh rm %s` to remove", migErr, a.Name, a.Name)
		}
	}

	snapshotAY, err := isnap.LoadAgentYAML(a.Name)
	if err != nil {
		return fmt.Errorf("read snapshot agent.yaml: %w", err)
	}
	snapshotPrompt, err := isnap.LoadSystemPrompt(a.Name)
	if err != nil {
		return fmt.Errorf("read snapshot system_prompt.md: %w", err)
	}

	envMap := map[string]string{}
	var missing []string
	required := a.RequiredEnv
	if len(snapshotAY.Spec.RequiredEnv) > 0 {
		required = snapshotAY.Spec.RequiredEnv
	}
	for _, name := range required {
		v := os.Getenv(name)
		if v == "" {
			missing = append(missing, name)
			continue
		}
		envMap[name] = v
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}

	model := a.Model
	if model == "" {
		model = snapshotAY.Spec.Model
	}

	resumeID := a.ClaudeSessionID
	if resumeID == "" {
		resumeID = uuid.NewString()
		_ = state.UpdateAgent(a.Name, func(d *state.AgentDef) { d.ClaudeSessionID = resumeID })
	}

	if err := writeAgentPromptFile(a.Name, snapshotPrompt); err != nil {
		fmt.Printf("mesh autostart: prompt-file err %s: %v\n", a.Name, err)
	}

	_, _, err = registry.StartWithOptions(sessions.StartOptions{
		AgentID:                    a.Name,
		Model:                      model,
		ResumeID:                   resumeID,
		Env:                        envMap,
		DangerouslySkipPermissions: a.DangerouslySkipPermissions,
	})
	return err
}

func migrateAgentSnapshot(a *state.AgentDef) error {
	if a.TemplateSource == "" || strings.HasPrefix(a.TemplateSource, "cloud:") {
		return fmt.Errorf("no local template source")
	}
	src, err := itpl.ResolveTemplateSource(a.TemplateSource)
	if err != nil {
		return err
	}
	if src.Kind == "git" && a.TemplateSha != "" {
		src.Ref = a.TemplateSha
	}
	tplDir, _, _, err := itpl.MaterializeTemplateDir(src)
	if err != nil {
		return err
	}
	base, err := itpl.LoadValues(tplDir, nil, nil, nil)
	if err != nil {
		return err
	}
	mergedValues := itpl.DeepMerge(base, a.Values)
	agentBytes, promptBytes, err := itpl.RenderTemplate(tplDir, a.Name, mergedValues)
	if err != nil {
		return err
	}
	if err := isnap.Agent(a.Name, agentBytes, promptBytes); err != nil {
		return err
	}
	fmt.Printf("mesh autostart: migrated snapshot for %s\n", a.Name)
	return nil
}

func runClusteredDaemon(ctx context.Context, baseURL, grpcAddr, token string, registry *sessions.SessionRegistry) error {
	conn, err := dialGrpc(grpcAddr)
	if err != nil {
		return fmt.Errorf("grpc dial %s: %w", grpcAddr, err)
	}
	defer conn.Close()

	client := meshv1.NewNodeServiceClient(conn)

	nodeID, err := registerWithSkewLog(ctx, client, token)
	if err != nil {
		return err
	}
	fmt.Printf("mesh node daemon registered nodeId=%s baseUrl=%s\n", nodeID, baseURL)

	if baseURL != "" {
		if err := warmLoopTemplate(baseURL, token); err != nil {
			fmt.Printf("mesh node loop-template warm err: %v (will retry on next agent_sync)\n", err)
		}
	}

	// Auto-push un-adopted local agents after connecting to the cluster.
	// Best-effort goroutine — daemon must not block on this.
	if baseURL != "" {
		go func() {
			result := agentsync.SyncAll(baseURL, token)
			fmt.Println(result.String())
		}()
	}

	// The per-agent channel router serves the mesh-shim sockets and delivers
	// inbound channel messages. Bound to the daemon ctx (not a stream ctx) so a
	// NodeStream reconnect never tears down a live agent's socket/shim.
	router := channels.New(ctx)

	errCh := make(chan error, 2)
	go func() { errCh <- heartbeatLoop(ctx, client, token, nodeID, registry) }()
	go func() { errCh <- nodeStreamLoop(ctx, client, token, registry, router, baseURL) }()
	go pollAgents(ctx, baseURL, token)

	return <-errCh
}

// DialGrpc dials a gRPC address with TLS credentials (system root CAs) for
// non-local hosts and plaintext for localhost/127.0.0.1, plus keepalive.
// Exported for use by cmd/mesh callers that probe node id.
func DialGrpc(addr string) (*grpc.ClientConn, error) {
	return dialGrpc(addr)
}

func dialGrpc(addr string) (*grpc.ClientConn, error) {
	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = addr
	}
	var creds credentials.TransportCredentials
	if isLocalHost(host) {
		creds = insecure.NewCredentials()
	} else {
		// Fly's edge terminates TLS for tcp services with handlers = ["tls"].
		// System roots are sufficient for the public hostname's cert chain.
		creds = credentials.NewTLS(&tls.Config{ServerName: host})
	}
	return grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "":
		return true
	}
	return false
}

func authContext(ctx context.Context, token string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)
	return metadata.NewOutgoingContext(ctx, md)
}

func registerWithSkewLog(ctx context.Context, client meshv1.NodeServiceClient, token string) (string, error) {
	ctx, cancel := context.WithTimeout(authContext(ctx, token), 15*time.Second)
	defer cancel()

	before := time.Now()
	resp, err := client.Register(ctx, &meshv1.RegisterRequest{
		NodeSlug: "",
		Version:  daemonVersion,
	})
	if err != nil {
		return "", fmt.Errorf("register: %w", err)
	}

	if resp.GetRegisteredAt() != nil {
		serverEpoch := resp.RegisteredAt.AsTime()
		skew := serverEpoch.Sub(before)
		fmt.Printf("mesh node register server_epoch=%d ms skew=%s\n", serverEpoch.UnixMilli(), skew)
	}

	return resp.GetNodeId(), nil
}

func heartbeatLoop(ctx context.Context, client meshv1.NodeServiceClient, token, nodeID string, registry *sessions.SessionRegistry) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	backoff := reconnectInitialBackoff
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			rpcCtx, cancel := context.WithTimeout(authContext(ctx, token), 10*time.Second)
			_, err := client.Heartbeat(rpcCtx, &meshv1.HeartbeatRequest{
				NodeId:   nodeID,
				Status:   meshv1.NodeStatus_NODE_STATUS_ONLINE,
				Sessions: registrySummariesProto(registry),
			})
			cancel()
			if err != nil {
				fmt.Printf("mesh node heartbeat error: %v (retrying in %s)\n", err, backoff)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(backoff):
				}
				backoff = nextBackoff(backoff)
				continue
			}
			backoff = reconnectInitialBackoff
		}
	}
}

func registrySummariesProto(registry *sessions.SessionRegistry) []*meshv1.SessionSummary {
	if registry == nil {
		return nil
	}
	list := registry.List()
	out := make([]*meshv1.SessionSummary, 0, len(list))
	for _, s := range list {
		out = append(out, s.Proto())
	}
	return out
}

func nodeStreamLoop(ctx context.Context, client meshv1.NodeServiceClient, token string, registry *sessions.SessionRegistry, router *channels.Router, baseURL string) error {
	backoff := reconnectInitialBackoff
	for {
		if err := runNodeStream(ctx, client, token, registry, router, baseURL); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			fmt.Printf("mesh node stream disconnected: %v (reconnecting in %s)\n", err, backoff)
		} else {
			fmt.Printf("mesh node stream closed cleanly; reconnecting in %s\n", backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff)
	}
}

type streamSender struct {
	mu     sync.Mutex
	stream meshv1.NodeService_NodeStreamClient
}

func (s *streamSender) Send(frame *meshv1.NodeToApi) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(frame)
}

func runNodeStream(ctx context.Context, client meshv1.NodeServiceClient, token string, registry *sessions.SessionRegistry, router *channels.Router, baseURL string) error {
	streamCtx, cancel := context.WithCancel(authContext(ctx, token))
	defer cancel()

	stream, err := client.NodeStream(streamCtx)
	if err != nil {
		return fmt.Errorf("open node stream: %w", err)
	}
	fmt.Println("mesh node stream opened")

	sender := &streamSender{stream: stream}

	registry.SetExitHook(func(agentID string, exitErr error) {
		st := meshv1.SessionState_SESSION_STATE_EXITED
		detail := ""
		exitCode := int32(0)
		if exitErr != nil {
			st = meshv1.SessionState_SESSION_STATE_CRASHED
			detail = exitErr.Error()
			if exitCode == 0 {
				exitCode = 1
			}
		}
		_ = sender.Send(&meshv1.NodeToApi{
			Payload: &meshv1.NodeToApi_SessionEvent{
				SessionEvent: &meshv1.SessionEvent{
					AgentId:    agentID,
					State:      st,
					ExitCode:   exitCode,
					Detail:     detail,
					OccurredAt: timestamppb.Now(),
				},
			},
		})
	})

	kaTicker := time.NewTicker(nodeStreamKeepaliveTimer)
	defer kaTicker.Stop()

	recvErr := make(chan error, 1)
	go func() {
		for {
			frame, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			handleApiFrame(streamCtx, sender, registry, router, frame, baseURL, token)
		}
	}()

	for {
		select {
		case <-streamCtx.Done():
			return streamCtx.Err()
		case err := <-recvErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-kaTicker.C:
			if err := sender.Send(&meshv1.NodeToApi{
				Payload: &meshv1.NodeToApi_HelloAck{
					HelloAck: &meshv1.StreamHelloAck{},
				},
			}); err != nil {
				return fmt.Errorf("keepalive send: %w", err)
			}
		}
	}
}

func handleApiFrame(ctx context.Context, sender *streamSender, registry *sessions.SessionRegistry, router *channels.Router, frame *meshv1.ApiToNode, baseURL, token string) {
	if frame == nil {
		return
	}
	reqID := frame.GetRequestId()
	fmt.Printf("mesh node stream frame reqId=%q kind=%s\n", reqID, apiFrameKind(frame))

	switch p := frame.Payload.(type) {
	case *meshv1.ApiToNode_SessionList:
		_ = p
		list := registry.List()
		protos := make([]*meshv1.SessionSummary, 0, len(list))
		for _, s := range list {
			protos = append(protos, s.Proto())
		}
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_SessionListResponse{
				SessionListResponse: &meshv1.SessionListResponse{Sessions: protos},
			},
		})

	case *meshv1.ApiToNode_SessionStart:
		agentID := p.SessionStart.GetAgentId()
		model := p.SessionStart.GetOptions().GetModel()
		sess, created, err := registry.Start(agentID, model, "")
		if err != nil {
			_ = sender.Send(&meshv1.NodeToApi{
				RequestId: reqID,
				Payload: &meshv1.NodeToApi_Error{
					Error: &meshv1.StreamError{Code: "session_start_failed", Message: err.Error()},
				},
			})
			return
		}
		router.Start(agentID)
		summary, _ := registry.Get(sess.AgentID)
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_SessionStartResponse{
				SessionStartResponse: &meshv1.SessionStartResponse{
					Session: summary.Proto(),
					Created: created,
				},
			},
		})

	case *meshv1.ApiToNode_SessionStop:
		stopped, err := registry.Stop(p.SessionStop.GetAgentId(), p.SessionStop.GetForce())
		if err != nil {
			_ = sender.Send(&meshv1.NodeToApi{
				RequestId: reqID,
				Payload: &meshv1.NodeToApi_Error{
					Error: &meshv1.StreamError{Code: "session_stop_failed", Message: err.Error()},
				},
			})
			return
		}
		router.Stop(p.SessionStop.GetAgentId())
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_SessionStopResponse{
				SessionStopResponse: &meshv1.SessionStopResponse{Stopped: stopped},
			},
		})

	case *meshv1.ApiToNode_SessionNewUp:
		agentID := p.SessionNewUp.GetAgentId()
		model := p.SessionNewUp.GetOptions().GetModel()
		sess, err := registry.NewUp(agentID, model)
		if err != nil {
			_ = sender.Send(&meshv1.NodeToApi{
				RequestId: reqID,
				Payload: &meshv1.NodeToApi_Error{
					Error: &meshv1.StreamError{Code: "session_new_up_failed", Message: err.Error()},
				},
			})
			return
		}
		router.Start(agentID)
		summary, _ := registry.Get(sess.AgentID)
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_SessionNewUpResponse{
				SessionNewUpResponse: &meshv1.SessionNewUpResponse{Session: summary.Proto()},
			},
		})

	case *meshv1.ApiToNode_SessionAttachOpen:
		attachRegistrySet(p.SessionAttachOpen.GetAttachId(), p.SessionAttachOpen.GetAgentId())
		go runAttach(ctx, sender, registry, p.SessionAttachOpen)

	case *meshv1.ApiToNode_SessionAttachInput:
		attachID := p.SessionAttachInput.GetAttachId()
		agentID, ok := attachRegistryLookup(attachID)
		if !ok {
			fmt.Printf("mesh node attach input drop: unknown attachId=%q\n", attachID)
			return
		}
		if _, err := registry.WriteInput(agentID, p.SessionAttachInput.GetData()); err != nil {
			fmt.Printf("mesh node attach input err agent=%s: %v\n", agentID, err)
		}

	case *meshv1.ApiToNode_SessionAttachClose:
		attachID := p.SessionAttachClose.GetAttachId()
		agentID, ok := attachRegistryRemove(attachID)
		if !ok {
			fmt.Printf("mesh node attach close for unknown attachId=%q\n", attachID)
			return
		}
		registry.DetachAttacher(agentID, attachID)

	case *meshv1.ApiToNode_AgentSync:
		ApplyAgentSync(registry, p.AgentSync, baseURL, token)
		applied := make([]string, 0, len(p.AgentSync.GetAgents()))
		for _, a := range p.AgentSync.GetAgents() {
			applied = append(applied, a.GetAgentId())
		}
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_AgentSyncAck{
				AgentSyncAck: &meshv1.AgentSyncAck{
					Revision:        p.AgentSync.GetRevision(),
					AppliedAgentIds: applied,
					Failures:        nil,
				},
			},
		})

	case *meshv1.ApiToNode_TaskPoke:
		delivered := deliverTaskPoke(p.TaskPoke)
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_TaskPokeAck{
				TaskPokeAck: &meshv1.TaskPokeAck{
					TaskId:    p.TaskPoke.GetTaskId(),
					Delivered: delivered,
				},
			},
		})

	case *meshv1.ApiToNode_ChannelInbound:
		ci := p.ChannelInbound
		agentID := ci.GetAgentId()
		// Ensure the per-agent router socket is listening for the shim.
		router.Start(agentID)

		// Resume-on-inbound: if no shim is connected (the conversation's PTY is
		// hibernated or never started), spawn/resume the claude session in the
		// background so the SERIAL dispatch loop isn't blocked. registry.Start is
		// idempotent — a no-op when the session is already running. The message is
		// still enqueued below and the router's queue flushes once the shim subscribes.
		detail := "delivered"
		if !router.IsConnected(agentID) {
			detail = "resuming"
			go func() {
				if _, _, err := registry.Start(agentID, "", ci.GetClaudeSessionId()); err != nil {
					fmt.Printf("mesh node channel_inbound resume agent=%s err=%v\n", agentID, err)
				}
			}()
		}

		// Build the inbound envelope the shim surfaces as notifications/claude/channel.
		// Source is the platform so meta.source identifies it (whatsapp/discord/web).
		var sentAt string
		if ts := ci.GetSentAt(); ts != nil {
			sentAt = ts.AsTime().Format(time.RFC3339)
		}
		env := ipc.InboundEnvFull(
			"agent:"+agentID,
			ci.GetExternalId(),
			ci.GetSender(),
			ci.GetMessageId(),
			ci.GetSenderId(),
			sentAt,
			ci.GetContent(),
			nil,
		)
		env.Source = ci.GetPlatform()
		accepted := router.DeliverInbound(agentID, env)
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_ChannelInboundAck{
				ChannelInboundAck: &meshv1.ChannelInboundAck{Accepted: accepted, Detail: detail},
			},
		})

	case *meshv1.ApiToNode_Hello:
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload:   &meshv1.NodeToApi_HelloAck{HelloAck: &meshv1.StreamHelloAck{AcceptedAt: timestamppb.Now()}},
		})

	default:
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_Error{
				Error: &meshv1.StreamError{Code: "unknown_payload", Message: "node does not recognize envelope variant"},
			},
		})
	}
}

func runAttach(ctx context.Context, sender *streamSender, registry *sessions.SessionRegistry, open *meshv1.SessionAttachOpen) {
	attachID := open.GetAttachId()
	agentID := open.GetAgentId()

	_, frames, dropped, cleanup, err := registry.AttachWithID(agentID, attachID)
	if err != nil {
		attachRegistryRemove(attachID)
		_ = sender.Send(&meshv1.NodeToApi{
			Payload: &meshv1.NodeToApi_SessionAttachClosed{
				SessionAttachClosed: &meshv1.SessionAttachClosed{AttachId: attachID, Reason: err.Error()},
			},
		})
		return
	}
	defer func() {
		cleanup()
		attachRegistryRemove(attachID)
	}()

	firstFrame := true
	for {
		select {
		case <-ctx.Done():
			_ = sender.Send(&meshv1.NodeToApi{
				Payload: &meshv1.NodeToApi_SessionAttachClosed{
					SessionAttachClosed: &meshv1.SessionAttachClosed{AttachId: attachID, Reason: "stream closed"},
				},
			})
			return
		case chunk, ok := <-frames:
			if !ok {
				_ = sender.Send(&meshv1.NodeToApi{
					Payload: &meshv1.NodeToApi_SessionAttachClosed{
						SessionAttachClosed: &meshv1.SessionAttachClosed{AttachId: attachID, Reason: "session exited"},
					},
				})
				return
			}
			for start := 0; start < len(chunk); start += sessions.AttachFrameCap {
				end := min(start+sessions.AttachFrameCap, len(chunk))
				data := &meshv1.SessionAttachData{
					AttachId:   attachID,
					Data:       chunk[start:end],
					ObservedAt: timestamppb.Now(),
				}
				if firstFrame {
					data.DroppedBytes = dropped
					firstFrame = false
				}
				if err := sender.Send(&meshv1.NodeToApi{
					Payload: &meshv1.NodeToApi_SessionAttachData{SessionAttachData: data},
				}); err != nil {
					return
				}
			}
		}
	}
}

var (
	attachRegistryMu sync.Mutex
	attachRegistry   = map[string]string{}
)

func attachRegistrySet(attachID, agentID string) {
	if attachID == "" {
		return
	}
	attachRegistryMu.Lock()
	defer attachRegistryMu.Unlock()
	attachRegistry[attachID] = agentID
}

func attachRegistryLookup(attachID string) (string, bool) {
	attachRegistryMu.Lock()
	defer attachRegistryMu.Unlock()
	a, ok := attachRegistry[attachID]
	return a, ok
}

func attachRegistryRemove(attachID string) (string, bool) {
	attachRegistryMu.Lock()
	defer attachRegistryMu.Unlock()
	a, ok := attachRegistry[attachID]
	if ok {
		delete(attachRegistry, attachID)
	}
	return a, ok
}

// ApplyAgentSync reconciles the node's session registry against the
// authoritative agent set pushed by mesh-api.
var agentSyncMu sync.Mutex

var (
	lastAppliedMu sync.Mutex
	lastApplied   = map[string]appliedAgent{}
)

type appliedAgent struct {
	systemPrompt string
	tools        []string
	model        string
}

func toolsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ApplyAgentSync(registry *sessions.SessionRegistry, sync *meshv1.AgentSync, baseURL, token string) {
	if sync == nil || registry == nil {
		return
	}
	agentSyncMu.Lock()
	defer agentSyncMu.Unlock()

	if baseURL != "" {
		if err := ensureLoopTemplate(baseURL, token); err != nil {
			fmt.Printf("mesh node loop-template fetch err: %v\n", err)
		}
	}

	fmt.Printf("mesh node agent_sync revision=%q mode=%s agents=%d\n",
		sync.GetRevision(), sync.GetMode().String(), len(sync.GetAgents()))

	desired := map[string]*meshv1.AgentDefinition{}
	for _, a := range sync.GetAgents() {
		if a == nil || a.GetAgentId() == "" {
			continue
		}
		desired[a.GetAgentId()] = a
	}

	adoptedByCloudID := map[string]string{}
	if locals, err := state.ListAgents(); err == nil {
		for _, a := range locals {
			if a.CloudAgentID != nil && *a.CloudAgentID != "" {
				adoptedByCloudID[*a.CloudAgentID] = a.Name
			}
		}
	}

	live := map[string]struct{}{}
	for _, s := range registry.List() {
		live[s.AgentID] = struct{}{}
	}

	keepKeys := map[string]struct{}{}
	for cloudID := range desired {
		if local, ok := adoptedByCloudID[cloudID]; ok {
			keepKeys[local] = struct{}{}
		} else {
			keepKeys[cloudID] = struct{}{}
		}
	}

	for agentID := range live {
		if _, keep := keepKeys[agentID]; keep {
			continue
		}
		fmt.Printf("mesh node agent_sync stop agent=%s (no longer in pinned set)\n", agentID)
		if _, err := registry.Stop(agentID, true); err != nil {
			fmt.Printf("mesh node agent_sync stop err agent=%s: %v\n", agentID, err)
		}
		lastAppliedMu.Lock()
		delete(lastApplied, agentID)
		lastAppliedMu.Unlock()
	}

	for cloudID, def := range desired {
		tools := append([]string{}, def.GetTools()...)
		model := def.GetModel()
		systemPrompt := def.GetSystemPrompt()
		restartRequired := def.GetMetadata()["restart_required"] == "true"

		registryKey := cloudID
		adopted := false
		if local, ok := adoptedByCloudID[cloudID]; ok {
			registryKey = local
			adopted = true
		}

		lastAppliedMu.Lock()
		prev, had := lastApplied[registryKey]
		lastAppliedMu.Unlock()

		_, running := live[registryKey]

		changed := had && (prev.systemPrompt != systemPrompt ||
			prev.model != model ||
			!toolsEqual(prev.tools, tools))

		switch {
		case adopted && running && !changed && !restartRequired:
			fmt.Printf("mesh node agent_sync: agent %s (cloud=%s) was already adopted locally; merging cloud definition\n", registryKey, cloudID)
		case !running:
			if err := writeAgentPromptFile(registryKey, systemPrompt); err != nil {
				fmt.Printf("mesh node agent_sync prompt-file err agent=%s: %v\n", registryKey, err)
			}
			fmt.Printf("mesh node agent_sync start agent=%s model=%s\n", registryKey, model)
			if _, _, err := registry.Start(registryKey, model, ""); err != nil {
				fmt.Printf("mesh node agent_sync start err agent=%s: %v\n", registryKey, err)
				continue
			}
		case restartRequired || changed:
			reason := "prompt/tools/model changed"
			if restartRequired {
				reason = "restart_required flag"
			}
			if err := writeAgentPromptFile(registryKey, systemPrompt); err != nil {
				fmt.Printf("mesh node agent_sync prompt-file err agent=%s: %v\n", registryKey, err)
			}
			fmt.Printf("mesh node agent_sync restart agent=%s reason=%s\n", registryKey, reason)
			if _, err := registry.Stop(registryKey, true); err != nil {
				fmt.Printf("mesh node agent_sync stop-before-restart err agent=%s: %v\n", registryKey, err)
			}
			if _, err := registry.NewUp(registryKey, model); err != nil {
				fmt.Printf("mesh node agent_sync restart err agent=%s: %v\n", registryKey, err)
				continue
			}
		default:
		}

		lastAppliedMu.Lock()
		lastApplied[registryKey] = appliedAgent{
			systemPrompt: systemPrompt,
			tools:        tools,
			model:        model,
		}
		lastAppliedMu.Unlock()
	}
}

// deliverTaskPoke is a no-op stub since the channel-routing seat was
// vacated by Plan 00-05. The Phase 2 dispatcher rebuild (DSP-01..07) will
// reattach a real delivery path here. Until then we acknowledge the gRPC
// frame with delivered=false so mesh-api knows the node accepted but did
// not surface the poke.
func deliverTaskPoke(poke *meshv1.TaskPoke) bool {
	if poke == nil || poke.GetAgentId() == "" {
		return false
	}
	fmt.Printf("mesh node poke drop agent=%s task=%s reason=no-dispatcher (Phase 2 will rewire)\n",
		poke.GetAgentId(), poke.GetTaskId())
	return false
}

func apiFrameKind(frame *meshv1.ApiToNode) string {
	if frame == nil {
		return "<nil>"
	}
	switch frame.Payload.(type) {
	case *meshv1.ApiToNode_SessionList:
		return "session_list"
	case *meshv1.ApiToNode_SessionStart:
		return "session_start"
	case *meshv1.ApiToNode_SessionStop:
		return "session_stop"
	case *meshv1.ApiToNode_SessionNewUp:
		return "session_new_up"
	case *meshv1.ApiToNode_SessionAttachOpen:
		return "session_attach_open"
	case *meshv1.ApiToNode_SessionAttachInput:
		return "session_attach_input"
	case *meshv1.ApiToNode_SessionAttachClose:
		return "session_attach_close"
	case *meshv1.ApiToNode_AgentSync:
		return "agent_sync"
	case *meshv1.ApiToNode_TaskPoke:
		return "task_poke"
	case *meshv1.ApiToNode_ChannelInbound:
		return "channel_inbound"
	case *meshv1.ApiToNode_Hello:
		return "hello"
	default:
		return "<unknown>"
	}
}

func nextBackoff(prev time.Duration) time.Duration {
	return max(min(prev*2, reconnectMaxBackoff), reconnectInitialBackoff)
}

// Loop-template cache.
var (
	loopTemplateMu      sync.RWMutex
	loopTemplateBody    string
	loopTemplateVersion string
)

type loopTemplateResponse struct {
	OK       bool   `json:"ok"`
	Template string `json:"template"`
	Version  string `json:"version"`
	Cadence  struct {
		IdleSeconds   int `json:"idleSeconds"`
		ActiveSeconds int `json:"activeSeconds"`
	} `json:"cadence"`
}

func warmLoopTemplate(baseURL, token string) error {
	return fetchLoopTemplate(baseURL, token)
}

func ensureLoopTemplate(baseURL, token string) error {
	loopTemplateMu.RLock()
	have := loopTemplateBody != ""
	loopTemplateMu.RUnlock()
	if have {
		return nil
	}
	return fetchLoopTemplate(baseURL, token)
}

func fetchLoopTemplate(baseURL, token string) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/agent/loop-template", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("loop-template fetch: %s", resp.Status)
	}

	var parsed loopTemplateResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if !parsed.OK || parsed.Template == "" {
		return errors.New("loop-template response missing body")
	}

	loopTemplateMu.Lock()
	wasEmpty := loopTemplateBody == ""
	loopTemplateBody = parsed.Template
	loopTemplateVersion = parsed.Version
	loopTemplateMu.Unlock()
	fmt.Printf("mesh node loop-template cached version=%s len=%d\n", parsed.Version, len(parsed.Template))

	if wasEmpty {
		go rematerializeLocalPrompts()
	}

	return nil
}

func rematerializeLocalPrompts() {
	agents, err := state.ListAgents()
	if err != nil {
		fmt.Printf("mesh node loop-template backfill: list agents: %v\n", err)
		return
	}
	for _, a := range agents {
		if a.CloudAgentID != nil && *a.CloudAgentID != "" {
			continue
		}
		if strings.HasPrefix(a.TemplateSource, "cloud:") {
			continue
		}
		var prompt string
		if isnap.Exists(a.Name) {
			p, err := isnap.LoadSystemPrompt(a.Name)
			if err != nil {
				fmt.Printf("mesh node loop-template backfill: read snapshot %s: %v\n", a.Name, err)
				continue
			}
			prompt = p
		} else {
			tpl, err := reloadAgentTemplate(a)
			if err != nil {
				fmt.Printf("mesh node loop-template backfill: reload %s: %v\n", a.Name, err)
				continue
			}
			prompt = tpl.SystemPrompt
		}
		if err := writeAgentPromptFile(a.Name, prompt); err != nil {
			fmt.Printf("mesh node loop-template backfill: write %s: %v\n", a.Name, err)
			continue
		}
		fmt.Printf("mesh node loop-template backfill: rewrote prompt for %s\n", a.Name)
	}
}

func reloadAgentTemplate(def *state.AgentDef) (*itpl.Template, error) {
	src, err := itpl.ResolveTemplateSource(def.TemplateSource)
	if err != nil {
		return nil, err
	}
	if src.Kind == "git" && def.TemplateSha != "" {
		src.Ref = def.TemplateSha
	}
	return itpl.LoadTemplate(src)
}

func agentWorkdir(agentID string) (string, error) { return paths.AgentWorkdir(agentID) }

// WriteAgentPromptFile materializes <systemPrompt>\n\n<loopTemplate> to
// ~/.mesh/agents/<agentId>/system_prompt.md. Exported so the localsock Deps
// callback can reference it.
func WriteAgentPromptFile(agentID, systemPrompt string) error {
	return writeAgentPromptFile(agentID, systemPrompt)
}

func writeAgentPromptFile(agentID, systemPrompt string) error {
	dir, err := agentWorkdir(agentID)
	if err != nil {
		return err
	}

	loopTemplateMu.RLock()
	loop := loopTemplateBody
	loopTemplateMu.RUnlock()

	var body strings.Builder
	body.WriteString(systemPrompt)
	if !strings.HasSuffix(systemPrompt, "\n") {
		body.WriteString("\n")
	}
	if loop != "" {
		body.WriteString("\n")
		body.WriteString(loop)
	}

	// Write both: CLAUDE.md is what claude-code auto-loads from its cwd as
	// project instructions, so the agent's system prompt actually reaches the
	// model. system_prompt.md is the source-of-truth file the snapshot/template
	// flow reads back; we keep both in sync. Same content, different filenames.
	if err := os.WriteFile(filepath.Join(dir, "system_prompt.md"), []byte(body.String()), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(body.String()), 0o600)
}

func migrateLegacyAgentPromptFiles() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("mesh daemon migrate: home: %v\n", err)
		return
	}
	root := filepath.Join(home, ".mesh", "agents")
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Printf("mesh daemon migrate: readdir %s: %v\n", root, err)
		}
		return
	}
	migrated := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		agentID := strings.TrimSuffix(name, ".md")
		legacy := filepath.Join(root, name)
		targetDir := filepath.Join(root, agentID)
		target := filepath.Join(targetDir, "system_prompt.md")

		if err := os.MkdirAll(targetDir, 0o700); err != nil {
			fmt.Printf("mesh daemon migrate: mkdir %s: %v\n", targetDir, err)
			continue
		}
		if _, err := os.Stat(target); err == nil {
			if err := os.Remove(legacy); err != nil {
				fmt.Printf("mesh daemon migrate: remove legacy %s: %v\n", legacy, err)
				continue
			}
			fmt.Printf("mesh daemon migrate: removed redundant legacy %s\n", legacy)
			migrated++
			continue
		}
		if err := os.Rename(legacy, target); err != nil {
			fmt.Printf("mesh daemon migrate: move %s -> %s: %v\n", legacy, target, err)
			continue
		}
		fmt.Printf("mesh daemon migrate: %s -> %s\n", legacy, target)
		migrated++
	}
	if migrated > 0 {
		fmt.Printf("mesh daemon migrate: %d agent prompt file(s) migrated to per-agent dir\n", migrated)
	}
}

func mergeStringMaps(base []string, overrides map[string]string) map[string]string {
	out := map[string]string{}
	for _, kv := range base {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			out[kv[:eq]] = kv[eq+1:]
		}
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
