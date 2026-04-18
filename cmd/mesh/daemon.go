package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

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
//     exists. Adds cluster features (AgentSync, WorkItemPoke, portal session
//     streaming) alongside the local surface.
//
// Both paths share a single SessionRegistry so locally-run and cluster-pinned
// agents are observable through either surface.
func RunDaemon(baseURL string, grpcAddr string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registry := NewSessionRegistry()

	// Local socket always starts — standalone mode requires it, cluster mode
	// piggybacks on the same registry.
	listener, err := StartLocalSocket(ctx, registry)
	if err != nil {
		return fmt.Errorf("start local socket: %w", err)
	}
	defer listener.Close()

	token, tokenErr := LoadNodeToken()
	if tokenErr != nil || strings.TrimSpace(token) == "" {
		if tokenErr != nil && !errors.Is(tokenErr, os.ErrNotExist) {
			return fmt.Errorf("read node token: %w", tokenErr)
		}
		fmt.Println("mesh node daemon running in standalone mode (no node token present; cluster features disabled)")
		<-ctx.Done()
		return ctx.Err()
	}

	return runClusteredDaemon(ctx, baseURL, grpcAddr, token, registry)
}

// runClusteredDaemon runs the existing gRPC-side behaviour when a node token
// is present. Extracted so the standalone path can skip it entirely.
func runClusteredDaemon(ctx context.Context, baseURL, grpcAddr, token string, registry *SessionRegistry) error {
	conn, err := dialGrpc(grpcAddr)
	if err != nil {
		return fmt.Errorf("grpc dial %s: %w", grpcAddr, err)
	}
	defer conn.Close()

	client := meshv1.NewNodeServiceClient(conn)

	// Register once to validate the token and pick up server epoch.
	nodeID, err := registerWithSkewLog(ctx, client, token, baseURL)
	if err != nil {
		return err
	}
	fmt.Printf("mesh node daemon registered nodeId=%s baseUrl=%s\n", nodeID, baseURL)

	// Warm the loop-template cache so the first agent_sync after reconnect can
	// materialize agent prompt files synchronously. Failure is non-fatal: we
	// retry on every sync that finds the cache empty.
	if baseURL != "" {
		if err := warmLoopTemplate(baseURL, token); err != nil {
			fmt.Printf("mesh node loop-template warm err: %v (will retry on next agent_sync)\n", err)
		}
	}

	// Heartbeat loop and NodeStream loop run concurrently; both reconnect on
	// their own timers / errors. The outer RunDaemon returns only on ctx
	// cancellation or an unrecoverable error.
	errCh := make(chan error, 2)
	go func() { errCh <- heartbeatLoop(ctx, client, token, nodeID, registry) }()
	go func() { errCh <- nodeStreamLoop(ctx, client, token, registry, baseURL) }()

	return <-errCh
}

func dialGrpc(addr string) (*grpc.ClientConn, error) {
	// Plaintext per A.2 — TLS terminates at Fly. grpc-go v1.80 requires an
	// explicit transport-credentials dial option.
	return grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

func authContext(ctx context.Context, token string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)
	return metadata.NewOutgoingContext(ctx, md)
}

func registerWithSkewLog(ctx context.Context, client meshv1.NodeServiceClient, token, baseURL string) (string, error) {
	ctx, cancel := context.WithTimeout(authContext(ctx, token), 15*time.Second)
	defer cancel()

	before := time.Now()
	resp, err := client.Register(ctx, &meshv1.RegisterRequest{
		NodeSlug: "", // populated by spec 08 node-start; stub uses empty.
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

func heartbeatLoop(ctx context.Context, client meshv1.NodeServiceClient, token, nodeID string, registry *SessionRegistry) error {
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

func registrySummariesProto(registry *SessionRegistry) []*meshv1.SessionSummary {
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

func nodeStreamLoop(ctx context.Context, client meshv1.NodeServiceClient, token string, registry *SessionRegistry, baseURL string) error {
	backoff := reconnectInitialBackoff
	for {
		if err := runNodeStream(ctx, client, token, registry, baseURL); err != nil {
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

// streamSender serializes concurrent writes to the bidi stream. The gRPC Go
// SendMsg contract is not safe for concurrent callers, so session-attach
// goroutines funnel through a mutex-guarded Send.
type streamSender struct {
	mu     sync.Mutex
	stream meshv1.NodeService_NodeStreamClient
}

func (s *streamSender) Send(frame *meshv1.NodeToApi) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(frame)
}

func runNodeStream(ctx context.Context, client meshv1.NodeServiceClient, token string, registry *SessionRegistry, baseURL string) error {
	streamCtx, cancel := context.WithCancel(authContext(ctx, token))
	defer cancel()

	stream, err := client.NodeStream(streamCtx)
	if err != nil {
		return fmt.Errorf("open node stream: %w", err)
	}
	fmt.Println("mesh node stream opened")

	sender := &streamSender{stream: stream}

	// Bridge per-session lifecycle events to the api so it can keep its view
	// consistent between heartbeats.
	registry.SetExitHook(func(agentID string, exitErr error) {
		state := meshv1.SessionState_SESSION_STATE_EXITED
		detail := ""
		exitCode := int32(0)
		if exitErr != nil {
			state = meshv1.SessionState_SESSION_STATE_CRASHED
			detail = exitErr.Error()
			if exitCode == 0 {
				exitCode = 1
			}
		}
		_ = sender.Send(&meshv1.NodeToApi{
			Payload: &meshv1.NodeToApi_SessionEvent{
				SessionEvent: &meshv1.SessionEvent{
					AgentId:    agentID,
					State:      state,
					ExitCode:   exitCode,
					Detail:     detail,
					OccurredAt: timestamppb.Now(),
				},
			},
		})
	})

	keepalive := time.NewTicker(nodeStreamKeepaliveTimer)
	defer keepalive.Stop()

	recvErr := make(chan error, 1)
	go func() {
		for {
			frame, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			handleApiFrame(streamCtx, sender, registry, frame, baseURL, token)
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
		case <-keepalive.C:
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

func handleApiFrame(ctx context.Context, sender *streamSender, registry *SessionRegistry, frame *meshv1.ApiToNode, baseURL, token string) {
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
					Revision:         p.AgentSync.GetRevision(),
					AppliedAgentIds:  applied,
					Failures:         nil,
				},
			},
		})

	case *meshv1.ApiToNode_WorkItemPoke:
		delivered := deliverWorkItemPoke(p.WorkItemPoke)
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_WorkItemPokeAck{
				WorkItemPokeAck: &meshv1.WorkItemPokeAck{
					WorkItemId: p.WorkItemPoke.GetWorkItemId(),
					Delivered:  delivered,
				},
			},
		})

	case *meshv1.ApiToNode_Hello:
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload:   &meshv1.NodeToApi_HelloAck{HelloAck: &meshv1.StreamHelloAck{AcceptedAt: timestamppb.Now()}},
		})

	default:
		// Unknown envelope — respond with an error so api doesn't block on a
		// missing request_id.
		_ = sender.Send(&meshv1.NodeToApi{
			RequestId: reqID,
			Payload: &meshv1.NodeToApi_Error{
				Error: &meshv1.StreamError{Code: "unknown_payload", Message: "node does not recognize envelope variant"},
			},
		})
	}
}

func runAttach(ctx context.Context, sender *streamSender, registry *SessionRegistry, open *meshv1.SessionAttachOpen) {
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
			for start := 0; start < len(chunk); start += attachFrameCap {
				end := start + attachFrameCap
				if end > len(chunk) {
					end = len(chunk)
				}
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

// attachRegistry maps attach_id → agent_id so SessionAttachInput and
// SessionAttachClose frames (which only carry attach_id) can be routed to the
// right PTY. Populated on SessionAttachOpen, cleaned on Close and on the
// attach goroutine's natural exit.
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
// authoritative agent set pushed by mesh-api. Full-replace semantics:
//   - agent in push not running → Start
//   - agent in push with changed prompt/tools/model (or metadata.restart_required) → Stop force + Start
//   - agent running but missing from push → Stop force
//
// The reconciler is serialized on a package-level mutex so concurrent pushes
// don't race each other's diffs. Per-agent work is still cheap (start/stop
// talk to the registry which has its own locking).
var agentSyncMu sync.Mutex

// lastApplied holds the last definition we applied per agent so we can diff
// against it on the next push. Keyed by agent id.
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

func ApplyAgentSync(registry *SessionRegistry, sync *meshv1.AgentSync, baseURL, token string) {
	if sync == nil || registry == nil {
		return
	}
	agentSyncMu.Lock()
	defer agentSyncMu.Unlock()

	// Ensure the loop template is cached. If not, fetch it now — best-effort;
	// absent template just means the agent file is written without the loop
	// directive appended.
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

	// Snapshot of what's currently running.
	live := map[string]struct{}{}
	for _, s := range registry.List() {
		live[s.AgentID] = struct{}{}
	}

	// 1. Stop anything not in the push.
	for agentID := range live {
		if _, keep := desired[agentID]; keep {
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

	// 2. Start / restart anything in the push.
	for agentID, def := range desired {
		tools := append([]string{}, def.GetTools()...)
		model := def.GetModel()
		systemPrompt := def.GetSystemPrompt()
		restartRequired := def.GetMetadata()["restart_required"] == "true"

		lastAppliedMu.Lock()
		prev, had := lastApplied[agentID]
		lastAppliedMu.Unlock()

		_, running := live[agentID]

		changed := had && (prev.systemPrompt != systemPrompt ||
			prev.model != model ||
			!toolsEqual(prev.tools, tools))

		switch {
		case !running:
			if err := writeAgentPromptFile(agentID, systemPrompt); err != nil {
				fmt.Printf("mesh node agent_sync prompt-file err agent=%s: %v\n", agentID, err)
			}
			fmt.Printf("mesh node agent_sync start agent=%s model=%s\n", agentID, model)
			if _, _, err := registry.Start(agentID, model, ""); err != nil {
				fmt.Printf("mesh node agent_sync start err agent=%s: %v\n", agentID, err)
				continue
			}
		case restartRequired || changed:
			reason := "prompt/tools/model changed"
			if restartRequired {
				reason = "restart_required flag"
			}
			if err := writeAgentPromptFile(agentID, systemPrompt); err != nil {
				fmt.Printf("mesh node agent_sync prompt-file err agent=%s: %v\n", agentID, err)
			}
			fmt.Printf("mesh node agent_sync restart agent=%s reason=%s\n", agentID, reason)
			if _, err := registry.Stop(agentID, true); err != nil {
				fmt.Printf("mesh node agent_sync stop-before-restart err agent=%s: %v\n", agentID, err)
			}
			if _, err := registry.NewUp(agentID, model); err != nil {
				fmt.Printf("mesh node agent_sync restart err agent=%s: %v\n", agentID, err)
				continue
			}
		default:
			// No-op — already running with the same config.
		}

		lastAppliedMu.Lock()
		lastApplied[agentID] = appliedAgent{
			systemPrompt: systemPrompt,
			tools:        tools,
			model:        model,
		}
		lastAppliedMu.Unlock()
	}
}

// deliverWorkItemPoke writes a JSON-line poke to a per-agent Unix socket so the
// in-session Claude Code hook (spec 04) can read it and accelerate its next
// tick. Missing sockets mean no hook is installed yet — that's expected during
// the spec 01 → 04 rollout, so we just log and drop.
func deliverWorkItemPoke(poke *meshv1.WorkItemPoke) bool {
	if poke == nil || poke.GetAgentId() == "" {
		return false
	}
	path := fmt.Sprintf("/tmp/mesh-agent-poke-%s.sock", poke.GetAgentId())
	if _, err := os.Stat(path); err != nil {
		fmt.Printf("mesh node poke drop agent=%s reason=no-listener\n", poke.GetAgentId())
		return false
	}
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		fmt.Printf("mesh node poke drop agent=%s reason=dial-%v\n", poke.GetAgentId(), err)
		return false
	}
	defer conn.Close()
	line := fmt.Sprintf(`{"work_item_id":%q,"reason":%q}`+"\n", poke.GetWorkItemId(), poke.GetReason().String())
	_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := conn.Write([]byte(line)); err != nil {
		fmt.Printf("mesh node poke write err agent=%s: %v\n", poke.GetAgentId(), err)
		return false
	}
	return true
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
	case *meshv1.ApiToNode_WorkItemPoke:
		return "work_item_poke"
	case *meshv1.ApiToNode_Hello:
		return "hello"
	default:
		return "<unknown>"
	}
}

func nextBackoff(prev time.Duration) time.Duration {
	next := time.Duration(math.Min(float64(prev)*2, float64(reconnectMaxBackoff)))
	if next < reconnectInitialBackoff {
		next = reconnectInitialBackoff
	}
	return next
}

// Loop-template cache — fetched once from mesh-api and appended to every
// agent's system_prompt when the prompt file is materialized. Empty string
// means "not fetched yet"; callers fall back to no loop directive.
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
	loopTemplateBody = parsed.Template
	loopTemplateVersion = parsed.Version
	loopTemplateMu.Unlock()
	fmt.Printf("mesh node loop-template cached version=%s len=%d\n", parsed.Version, len(parsed.Template))
	return nil
}

// writeAgentPromptFile materializes <systemPrompt>\n\n<loopTemplate> to
// ~/.mesh/agents/<agentId>.md. We drop it in a shared per-user location because
// nodes in the current pivot don't own a per-agent workdir — users manage repos
// out-of-band, so ~/.claude/agents/<id>.md may not have a correct anchor.
// Future work: when `mesh node start` learns about per-agent worktrees, write
// to <workdir>/.claude/agents/<agentId>.md instead.
func writeAgentPromptFile(agentID, systemPrompt string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".mesh", "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
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

	path := filepath.Join(dir, agentID+".md")
	return os.WriteFile(path, []byte(body.String()), 0o600)
}
