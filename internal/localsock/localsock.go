package localsock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/accounts"
	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/channels"
	"projectmesh.io/mesh-cli/internal/ipc"
	"projectmesh.io/mesh-cli/internal/paths"
	"projectmesh.io/mesh-cli/internal/peers"
	isnap "projectmesh.io/mesh-cli/internal/snapshot"
	"projectmesh.io/mesh-cli/internal/sessions"
	"projectmesh.io/mesh-cli/internal/state"
	itpl "projectmesh.io/mesh-cli/internal/template"
	meshv1 "projectmesh.io/mesh-cli/proto/meshv1"
)

// Deps holds callbacks for helpers that live in internal/daemon (which
// imports internal/localsock, so we cannot import it back). Injected at
// StartLocalSocket time.
type Deps struct {
	// WritePromptFile materializes <systemPrompt>\n\n<loopTemplate> to disk for
	// the named agent. Daemon owns the loop-template cache.
	WritePromptFile func(agentID, systemPrompt string) error

	// WriteSettings writes .mcp.json / plugin manifest for the named agent.
	WriteSettings func(agentName string, ch []channels.Spec, envSource map[string]string) error

	// ProbeNodeID dials mesh-api to learn this node's registered id. Used by
	// the cluster-pull handler to verify the cloud agent is pinned here.
	ProbeNodeID func(baseURL, token string) (string, error)

	// AutoPushAgent is called after a new local agent is created (mesh run).
	// Best-effort: errors are logged, not returned. May be nil.
	AutoPushAgent func(agentName string)
}

// StartLocalSocket starts the Unix-socket HTTP server that serves the local
// daemon surface (mesh run/ps/attach/logs/...). It takes the same
// SessionRegistry the cluster-mode gRPC handlers use, so standalone-mode
// agents and pinned cluster agents share process-local state.
//
// chanMgr and chanRouter may be nil (used in tests / standalone contexts that
// don't need gateway supervision).
//
// Returns the listener so the caller can shut it down; server runs until
// ctx is cancelled.
func StartLocalSocket(ctx context.Context, registry *sessions.SessionRegistry, chanMgr *channels.ChannelManager, chanRouter *channels.ChannelRouter, broker *peers.Broker, deps Deps) (net.Listener, error) {
	if _, err := paths.EnsureMeshDir(); err != nil {
		return nil, err
	}

	sockPath, err := paths.DaemonSocketPath()
	if err != nil {
		return nil, err
	}

	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("clean stale socket: %w", err)
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	srv := &localServer{registry: registry, chanMgr: chanMgr, chanRouter: chanRouter, broker: broker, deps: deps}
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions", srv.handleSessions)
	mux.HandleFunc("/sessions/", srv.handleSessionItem)
	mux.HandleFunc("/templates", srv.handleTemplates)
	mux.HandleFunc("/templates/pull", srv.handleTemplatesPull)
	mux.HandleFunc("/cluster/pull", srv.handleClusterPull)
	mux.HandleFunc("/discord/pair", srv.handleDiscordPair)
	mux.HandleFunc("/discord/unpair", srv.handleDiscordUnpair)
	mux.HandleFunc("/discord/list-channels", srv.handleDiscordListChannels)
	mux.HandleFunc("/discord/status", srv.handleDiscordStatus)
	mux.HandleFunc("/whatsapp/pair", srv.handleWhatsappPair)
	mux.HandleFunc("/whatsapp/unpair", srv.handleWhatsappUnpair)
	mux.HandleFunc("/whatsapp/status", srv.handleWhatsappStatus)
	mux.HandleFunc("/peers/list", srv.handlePeersList)
	mux.HandleFunc("/peers/set-summary", srv.handlePeersSetSummary)

	httpSrv := &http.Server{
		Handler:      mux,
		ReadTimeout:  0,
		WriteTimeout: 0,
	}

	go func() {
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			fmt.Printf("mesh daemon local socket serve error: %v\n", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	fmt.Printf("mesh daemon local socket listening at %s (mode 0600)\n", sockPath)
	return listener, nil
}

type localServer struct {
	registry   *sessions.SessionRegistry
	chanMgr    *channels.ChannelManager
	chanRouter *channels.ChannelRouter
	broker     *peers.Broker
	deps       Deps
}

// -- JSON payload shapes --

type SessionView struct {
	Name            string `json:"name"`
	TemplateSource  string `json:"templateSource"`
	TemplateRef     string `json:"templateRef,omitempty"`
	Status          string `json:"status"`
	SessionID       string `json:"sessionId,omitempty"`
	ClaudeSessionID string `json:"claudeSessionId,omitempty"`
	PID             int    `json:"pid,omitempty"`
	StartedAt       string `json:"startedAt,omitempty"`
	LastActivityAt  string `json:"lastActivityAt,omitempty"`
	Model           string `json:"model,omitempty"`
}

type CreateSessionRequest struct {
	Name                       string            `json:"name"`
	TemplateSource             string            `json:"templateSource"`
	Env                        map[string]string `json:"env,omitempty"`
	AutoStart                  *bool             `json:"autoStart,omitempty"`
	DangerouslySkipPermissions *bool             `json:"dangerouslySkipPermissions,omitempty"`
	Values                     map[string]any    `json:"values,omitempty"`
}

type CreateSessionResponse struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	SessionID string `json:"sessionId,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// -- handlers --

func (s *localServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSessions(w, r)
	case http.MethodPost:
		s.createSession(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *localServer) handleSessionItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "missing session name")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	verb := ""
	if len(parts) == 2 {
		verb = parts[1]
	}

	switch verb {
	case "":
		switch r.Method {
		case http.MethodGet:
			s.inspectSession(w, r, name)
		case http.MethodDelete:
			s.removeSession(w, r, name)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "attach":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.attachSession(w, r, name)
	case "logs":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.logsSession(w, r, name)
	case "restart":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.restartSession(w, r, name)
	case "update":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.updateSession(w, r, name)
	case "push":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.pushSession(w, r, name)
	default:
		writeError(w, http.StatusNotFound, "unknown session verb")
	}
}

func (s *localServer) listSessions(w http.ResponseWriter, _ *http.Request) {
	defs, err := state.ListAgents()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	live := map[string]sessions.PtySessionSummary{}
	for _, sum := range s.registry.List() {
		live[sum.AgentID] = sum
	}

	out := make([]SessionView, 0, len(defs))
	for _, def := range defs {
		sv := SessionView{
			Name:           def.Name,
			TemplateSource: def.TemplateSource,
			TemplateRef:    def.TemplateRef,
			Status:         "stopped",
			Model:          def.Model,
		}
		if sum, ok := live[def.Name]; ok {
			sv.Status = stateName(sum.State)
			sv.SessionID = sum.SessionID
			sv.ClaudeSessionID = sum.ClaudeSessionID
			sv.PID = sum.PID
			sv.StartedAt = sum.StartedAt.UTC().Format(time.RFC3339)
			sv.LastActivityAt = sum.LastActivity.UTC().Format(time.RFC3339)
		}
		out = append(out, sv)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *localServer) createSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.TemplateSource == "" {
		writeError(w, http.StatusBadRequest, "templateSource is required")
		return
	}

	src, err := itpl.ResolveTemplateSource(req.TemplateSource)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tplDir, gitSHA, gitRef, err := itpl.MaterializeTemplateDir(src)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := isnap.ValidateSystemPromptPathInDir(tplDir, name, req.Values); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	base, err := itpl.LoadValues(tplDir, nil, nil, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mergedValues := itpl.DeepMerge(base, req.Values)
	agentBytes, promptBytes, err := itpl.RenderTemplate(tplDir, name, mergedValues)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tpl, values, err := itpl.ReadTemplateWithValues(tplDir, name, req.Values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tpl.Source = req.TemplateSource
	tpl.GitSHA = gitSHA
	tpl.GitRef = gitRef
	if src.Kind == "git" {
		tpl.GitURL = src.URL
	}

	effectiveEnv := mergeStringMaps(os.Environ(), req.Env)
	if missing := missingRequiredEnv(tpl.RequiredEnv, effectiveEnv); len(missing) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("required env not set: %s", strings.Join(missing, ", ")))
		return
	}

	skipPerms := tpl.DangerouslySkipPermissions
	if req.DangerouslySkipPermissions != nil {
		skipPerms = *req.DangerouslySkipPermissions
	}

	if err := isnap.Agent(name, agentBytes, promptBytes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	def := &state.AgentDef{
		Name:              name,
		TemplateSource:    req.TemplateSource,
		TemplateRef:       tpl.GitRef,
		TemplateSha:       tpl.GitSHA,
		TemplateCachePath: tpl.ResolvedPath,
		Description:       tpl.Description,
		Model:             tpl.Model,
		RequiredEnv:       append([]string{}, tpl.RequiredEnv...),
		CreatedAt:         time.Now().UTC(),
		ClaudeSessionID:   uuid.NewString(),
		DangerouslySkipPermissions: skipPerms,
		Channels:          append([]channels.Spec{}, tpl.Channels...),
		Values:            values,
	}
	if err := state.AddAgent(def); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Auto-push to cloud on new local agent creation. Best-effort goroutine —
	// must not block the HTTP response.
	if s.deps.AutoPushAgent != nil {
		go s.deps.AutoPushAgent(name)
	}

	if s.chanMgr != nil {
		if all, err := state.ListAgents(); err == nil {
			s.chanMgr.Sync(agentEntries(all))
		}
	}

	autoStart := true
	if req.AutoStart != nil {
		autoStart = *req.AutoStart
	}

	resp := CreateSessionResponse{Name: name, Status: "created"}
	if autoStart {
		if s.deps.WritePromptFile != nil {
			if err := s.deps.WritePromptFile(name, tpl.SystemPrompt); err != nil {
				fmt.Printf("mesh daemon prompt-file err name=%s: %v\n", name, err)
			}
		}
		if s.deps.WriteSettings != nil {
			if err := s.deps.WriteSettings(name, def.Channels, effectiveEnv); err != nil {
				fmt.Printf("mesh daemon claude-settings err name=%s: %v\n", name, err)
			}
		}
		sess, _, err := s.registry.StartWithOptions(sessions.StartOptions{
			AgentID:                    name,
			Model:                      tpl.Model,
			ResumeID:                   def.ClaudeSessionID,
			Env:                        pickEnv(effectiveEnv, tpl.RequiredEnv),
			WorkingDir:                 tpl.WorkingDir,
			DangerouslySkipPermissions: skipPerms,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("start session: %v", err))
			return
		}
		_ = state.UpdateAgent(name, func(a *state.AgentDef) { a.LastSessionID = sess.SessionID })
		resp.Status = "running"
		resp.SessionID = sess.SessionID
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *localServer) inspectSession(w http.ResponseWriter, _ *http.Request, name string) {
	def, ok, err := state.GetAgent(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	sum, running := s.registry.Get(name)
	out := map[string]any{
		"definition": def,
		"running":    running,
		"template": map[string]any{
			"source":    def.TemplateSource,
			"ref":       def.TemplateRef,
			"sha":       def.TemplateSha,
			"shortSha":  itpl.ShortSHA(def.TemplateSha),
			"cachePath": def.TemplateCachePath,
		},
		"requiredEnv": def.RequiredEnv,
	}
	if def.CloudAgentID != nil && *def.CloudAgentID != "" {
		out["cloudAgentId"] = *def.CloudAgentID
	}
	if len(def.Channels) > 0 {
		out["channels"] = channels.ChannelStatusesForAgent(name, def.Channels)
	}
	if running {
		session := map[string]any{
			"sessionId":      sum.SessionID,
			"pid":            sum.PID,
			"state":          stateName(sum.State),
			"startedAt":      sum.StartedAt.UTC().Format(time.RFC3339),
			"lastActivityAt": sum.LastActivity.UTC().Format(time.RFC3339),
			"cols":           sum.Cols,
			"rows":           sum.Rows,
		}
		if sum.ClaudeSessionID != "" {
			session["claudeSessionId"] = sum.ClaudeSessionID
		}
		out["session"] = session
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *localServer) removeSession(w http.ResponseWriter, _ *http.Request, name string) {
	_, ok, err := state.GetAgent(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if _, err := s.registry.Stop(name, true); err != nil {
		fmt.Printf("mesh daemon rm stop err name=%s: %v\n", name, err)
	}
	if _, err := state.RemoveAgent(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.chanMgr != nil {
		if all, err := state.ListAgents(); err == nil {
			s.chanMgr.Sync(agentEntries(all))
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "status": "removed"})
}

func (s *localServer) restartSession(w http.ResponseWriter, _ *http.Request, name string) {
	def, ok, err := state.GetAgent(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}

	if !isnap.Exists(name) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("agent %q has no snapshot — run `mesh update %s` to rebuild it, or `mesh rm %s` to remove", name, name, name))
		return
	}
	snapshotAY, err := isnap.LoadAgentYAML(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("read snapshot: %v", err))
		return
	}
	snapshotPrompt, err := isnap.LoadSystemPrompt(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("read snapshot: %v", err))
		return
	}

	if s.deps.WritePromptFile != nil {
		if err := s.deps.WritePromptFile(name, snapshotPrompt); err != nil {
			fmt.Printf("mesh daemon prompt-file err name=%s: %v\n", name, err)
		}
	}

	effectiveEnv := mergeStringMaps(os.Environ(), nil)
	required := def.RequiredEnv
	if len(snapshotAY.Spec.RequiredEnv) > 0 {
		required = snapshotAY.Spec.RequiredEnv
	}
	if missing := missingRequiredEnv(required, effectiveEnv); len(missing) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("required env not set: %s", strings.Join(missing, ", ")))
		return
	}

	if def.ClaudeSessionID == "" {
		def.ClaudeSessionID = uuid.NewString()
		_ = state.UpdateAgent(name, func(a *state.AgentDef) { a.ClaudeSessionID = def.ClaudeSessionID })
	}

	model := def.Model
	if model == "" {
		model = snapshotAY.Spec.Model
	}

	opts := sessions.StartOptions{
		AgentID:                    name,
		Model:                      model,
		ResumeID:                   def.ClaudeSessionID,
		Env:                        pickEnv(effectiveEnv, required),
		DangerouslySkipPermissions: def.DangerouslySkipPermissions,
	}

	if s.deps.WriteSettings != nil {
		if err := s.deps.WriteSettings(name, def.Channels, effectiveEnv); err != nil {
			fmt.Printf("mesh daemon claude-settings err name=%s: %v\n", name, err)
		}
	}

	sess, err := s.registry.NewUpWithOptions(opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = state.UpdateAgent(name, func(a *state.AgentDef) { a.LastSessionID = sess.SessionID })
	writeJSON(w, http.StatusOK, map[string]string{
		"name":      name,
		"sessionId": sess.SessionID,
		"status":    "running",
	})
}

func (s *localServer) updateSession(w http.ResponseWriter, _ *http.Request, name string) {
	def, ok, err := state.GetAgent(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if def.TemplateSource == "" {
		writeError(w, http.StatusBadRequest, "agent has no template source")
		return
	}

	src, err := itpl.ResolveTemplateSource(def.TemplateSource)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if def.TemplateRef != "" {
		src.Ref = def.TemplateRef
	}

	tplDir, gitSHA, _, err := itpl.MaterializeTemplateDir(src)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := isnap.ValidateSystemPromptPathInDir(tplDir, name, def.Values); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	base, err := itpl.LoadValues(tplDir, nil, nil, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mergedValues := itpl.DeepMerge(base, def.Values)
	agentBytes, promptBytes, err := itpl.RenderTemplate(tplDir, name, mergedValues)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	tpl, _, err := itpl.ReadTemplateWithValues(tplDir, name, def.Values)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tpl.GitSHA = gitSHA
	if src.Kind == "git" {
		tpl.GitURL = src.URL
	}

	oldSHA := def.TemplateSha
	newSHA := tpl.GitSHA

	_, wasRunning := s.registry.Get(name)

	resp := map[string]any{
		"name":      name,
		"oldSha":    oldSHA,
		"newSha":    newSHA,
		"status":    "up-to-date",
		"restarted": false,
	}

	if newSHA == oldSHA && oldSHA != "" && isnap.Exists(name) {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if err := isnap.Agent(name, agentBytes, promptBytes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	_ = state.UpdateAgent(name, func(a *state.AgentDef) {
		a.TemplateSha = newSHA
		a.TemplateCachePath = tpl.ResolvedPath
		a.Description = tpl.Description
		a.RequiredEnv = append([]string{}, tpl.RequiredEnv...)
		a.Channels = append([]channels.Spec{}, tpl.Channels...)
		if a.Model == "" {
			a.Model = tpl.Model
		}
	})
	def.Channels = append([]channels.Spec{}, tpl.Channels...)

	if s.chanMgr != nil {
		if all, err := state.ListAgents(); err == nil {
			s.chanMgr.Sync(agentEntries(all))
		}
	}
	if s.chanRouter != nil {
		if all, err := state.ListAgents(); err == nil {
			s.chanRouter.Sync(agentEntries(all))
		}
	}

	if wasRunning {
		effectiveEnv := mergeStringMaps(os.Environ(), nil)
		if missing := missingRequiredEnv(tpl.RequiredEnv, effectiveEnv); len(missing) > 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("required env not set: %s", strings.Join(missing, ", ")))
			return
		}
		if s.deps.WritePromptFile != nil {
			_ = s.deps.WritePromptFile(name, tpl.SystemPrompt)
		}
		if s.deps.WriteSettings != nil {
			if err := s.deps.WriteSettings(name, def.Channels, effectiveEnv); err != nil {
				fmt.Printf("mesh daemon claude-settings err name=%s: %v\n", name, err)
			}
		}
		sess, err := s.registry.NewUpWithOptions(sessions.StartOptions{
			AgentID:    name,
			Model:      def.Model,
			Env:        pickEnv(effectiveEnv, tpl.RequiredEnv),
			WorkingDir: tpl.WorkingDir,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = state.UpdateAgent(name, func(a *state.AgentDef) { a.LastSessionID = sess.SessionID })
		resp["restarted"] = true
		resp["sessionId"] = sess.SessionID
	}
	resp["status"] = "updated"
	writeJSON(w, http.StatusOK, resp)
}

// reloadAgentTemplate re-materializes the template referenced by def.
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

func (s *localServer) logsSession(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := s.registry.Get(name); !ok {
		writeError(w, http.StatusNotFound, "session not running")
		return
	}

	follow := r.URL.Query().Get("follow") == "true"
	attachID := uuid.NewString()
	_, frames, dropped, cleanup, err := s.registry.AttachWithID(name, attachID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer cleanup()

	w.Header().Set("Content-Type", "application/octet-stream")
	if dropped > 0 {
		w.Header().Set("X-Mesh-Dropped-Bytes", fmt.Sprintf("%d", dropped))
	}
	flusher, _ := w.(http.Flusher)

	if follow {
		for {
			select {
			case <-r.Context().Done():
				return
			case chunk, ok := <-frames:
				if !ok {
					return
				}
				if _, err := w.Write(chunk); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}

	idle := time.NewTimer(50 * time.Millisecond)
	defer idle.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, ok := <-frames:
			if !ok {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(50 * time.Millisecond)
		case <-idle.C:
			return
		}
	}
}

func (s *localServer) attachSession(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := s.registry.Get(name); !ok {
		writeError(w, http.StatusNotFound, "session not running")
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "hijack not supported")
		return
	}

	conn, brw, err := hj.Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("hijack: %v", err))
		return
	}
	defer conn.Close()

	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: mesh-attach\r\n\r\n"); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}

	attachID := uuid.NewString()
	_, frames, dropped, cleanup, err := s.registry.AttachWithID(name, attachID)
	if err != nil {
		return
	}
	defer cleanup()
	_ = dropped

	done := make(chan struct{})
	go func() {
		defer close(done)
		for chunk := range frames {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	}()

	readBuf := make([]byte, 4096)
	if brw.Reader.Buffered() > 0 {
		leftover, _ := brw.Reader.Peek(brw.Reader.Buffered())
		if len(leftover) > 0 {
			_, _ = s.registry.WriteInput(name, leftover)
			_, _ = brw.Reader.Discard(len(leftover))
		}
	}
	for {
		n, err := conn.Read(readBuf)
		if n > 0 {
			if _, werr := s.registry.WriteInput(name, readBuf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}

	s.registry.DetachAttacher(name, attachID)
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}
}

func (s *localServer) handleTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	entries, err := itpl.ListCachedTemplates()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

type PullTemplateRequest struct {
	Source string `json:"source"`
}

type PullTemplateResponse struct {
	SHA       string `json:"sha"`
	CachePath string `json:"cachePath"`
	Ref       string `json:"ref,omitempty"`
	URL       string `json:"url,omitempty"`
}

func (s *localServer) handleTemplatesPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req PullTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	src, err := itpl.ResolveTemplateSource(req.Source)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if src.Kind != "git" {
		writeError(w, http.StatusBadRequest, "only git sources can be pulled; local paths are read in place")
		return
	}
	sha, path, err := itpl.FetchGitTemplate(src)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, PullTemplateResponse{
		SHA:       sha,
		CachePath: path,
		Ref:       src.Ref,
		URL:       src.URL,
	})
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

func missingRequiredEnv(required []string, env map[string]string) []string {
	var missing []string
	for _, name := range required {
		if env[name] == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

func pickEnv(env map[string]string, names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		if v, ok := env[n]; ok {
			out[n] = v
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

// agentEntries converts a []*state.AgentDef slice to []channels.AgentEntry
// (the cycle-break type). Defined here to avoid the state↔channels import
// cycle: channels cannot import state.
func agentEntries(agents []*state.AgentDef) []channels.AgentEntry {
	out := make([]channels.AgentEntry, 0, len(agents))
	for _, a := range agents {
		specs := make([]channels.Spec, 0, len(a.Channels))
		for _, ch := range a.Channels {
			cfg := make(map[string]any, len(ch.Config))
			for k, v := range ch.Config {
				cfg[k] = v
			}
			specs = append(specs, channels.Spec{Type: ch.Type, Config: cfg})
		}
		out = append(out, channels.AgentEntry{
			Name:        a.Name,
			Description: a.Description,
			Channels:    specs,
		})
	}
	return out
}

// -- cluster push / pull --

type PushSessionRequest struct {
	ProjectID string `json:"projectId,omitempty"`
	As        string `json:"as,omitempty"`
}

type PushSessionResponse struct {
	Name         string `json:"name"`
	CloudAgentID string `json:"cloudAgentId"`
	ProjectID    string `json:"projectId,omitempty"`
}

type PullSessionRequest struct {
	CloudAgentID string `json:"cloudAgentId"`
	As           string `json:"as,omitempty"`
}

type PullSessionResponse struct {
	Name         string `json:"name"`
	CloudAgentID string `json:"cloudAgentId"`
}

func (s *localServer) pushSession(w http.ResponseWriter, r *http.Request, name string) {
	var req PushSessionRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	def, ok, err := state.GetAgent(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if def.CloudAgentID != nil && *def.CloudAgentID != "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        "already_adopted",
			"cloudAgentId": *def.CloudAgentID,
		})
		return
	}

	token, tokErr := auth.LoadNodeToken()
	if tokErr != nil || strings.TrimSpace(token) == "" {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error": "not_linked_to_cluster",
		})
		return
	}

	tpl, tplErr := reloadAgentTemplate(def)
	if tplErr != nil {
		tpl = nil
	}

	pushName := strings.TrimSpace(req.As)
	if pushName == "" {
		pushName = def.Name
	}

	payload := map[string]any{
		"name":  pushName,
		"model": def.Model,
	}
	if tpl != nil {
		payload["description"] = ""
		payload["systemPrompt"] = tpl.SystemPrompt
		payload["tools"] = tpl.Tools
		payload["requiredEnvNames"] = tpl.RequiredEnv
	} else {
		payload["requiredEnvNames"] = def.RequiredEnv
	}
	if def.TemplateSource != "" {
		payload["templateSource"] = def.TemplateSource
	}
	if def.TemplateSha != "" {
		payload["templateSha"] = def.TemplateSha
	}
	if v := strings.TrimSpace(req.ProjectID); v != "" {
		payload["projectId"] = v
	}

	baseURL := resolveDaemonBaseURL()
	status, body, err := nodeAPIRequest(baseURL, token, http.MethodPost, "/agent/import", payload)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("mesh-api import: %v", err))
		return
	}

	if status == http.StatusConflict {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(body)
		return
	}
	if status < 200 || status >= 300 {
		writeError(w, status, fmt.Sprintf("mesh-api import: %s", strings.TrimSpace(string(body))))
		return
	}

	var parsed struct {
		OK           bool   `json:"ok"`
		ID           string `json:"id"`
		Name         string `json:"name"`
		PinnedNodeID string `json:"pinned_node_id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("parse import response: %v", err))
		return
	}
	if parsed.ID == "" {
		writeError(w, http.StatusInternalServerError, "mesh-api import: missing id")
		return
	}

	cloudID := parsed.ID
	if err := state.UpdateAgent(name, func(a *state.AgentDef) { a.CloudAgentID = &cloudID }); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("persist cloudAgentId: %v", err))
		return
	}

	fmt.Printf("mesh daemon adopted name=%s cloudAgentId=%s project=%s\n", name, cloudID, req.ProjectID)

	writeJSON(w, http.StatusOK, PushSessionResponse{
		Name:         name,
		CloudAgentID: cloudID,
		ProjectID:    req.ProjectID,
	})
}

func (s *localServer) handleClusterPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req PullSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	cloudID := strings.TrimSpace(req.CloudAgentID)
	if cloudID == "" {
		writeError(w, http.StatusBadRequest, "cloudAgentId is required")
		return
	}

	token, tokErr := auth.LoadNodeToken()
	if tokErr != nil || strings.TrimSpace(token) == "" {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error": "not_linked_to_cluster",
		})
		return
	}

	baseURL := resolveDaemonBaseURL()
	status, body, err := nodeAPIRequest(baseURL, token, http.MethodGet, "/agent/get?id="+cloudID, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("mesh-api get: %v", err))
		return
	}
	if status < 200 || status >= 300 {
		writeError(w, status, fmt.Sprintf("mesh-api get: %s", strings.TrimSpace(string(body))))
		return
	}

	var parsed struct {
		OK    bool `json:"ok"`
		Agent struct {
			ID           string          `json:"id"`
			Name         string          `json:"name"`
			SystemPrompt string          `json:"system_prompt"`
			Model        string          `json:"model"`
			PinnedNodeID string          `json:"pinned_node_id"`
			ProjectID    *string         `json:"project_id"`
			Tools        json.RawMessage `json:"tools"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("parse agent response: %v", err))
		return
	}
	if parsed.Agent.ID == "" {
		writeError(w, http.StatusInternalServerError, "mesh-api get: missing agent")
		return
	}

	myNodeID, err := s.loadDaemonNodeID(baseURL, token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("resolve local node id: %v", err))
		return
	}
	if parsed.Agent.PinnedNodeID != myNodeID {
		writeError(w, http.StatusForbidden, "cloud agent is pinned to a different node")
		return
	}

	localName := strings.TrimSpace(req.As)
	if localName == "" {
		localName = parsed.Agent.Name
	}
	if _, exists, err := state.GetAgent(localName); err == nil && exists {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "local_name_taken",
		})
		return
	}

	if s.deps.WritePromptFile != nil {
		if err := s.deps.WritePromptFile(localName, parsed.Agent.SystemPrompt); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write prompt: %v", err))
			return
		}
	}

	cloud := parsed.Agent.ID
	def := &state.AgentDef{
		Name:           localName,
		TemplateSource: "cloud:" + cloud,
		Model:          parsed.Agent.Model,
		CreatedAt:      time.Now().UTC(),
		CloudAgentID:   &cloud,
	}
	if err := state.AddAgent(def); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, PullSessionResponse{
		Name:         localName,
		CloudAgentID: cloud,
	})
}

// loadDaemonNodeID probes mesh-api to learn this node's id. Cached per server
// instance so we don't hit the network on every pull.
var (
	cachedNodeIDMu sync.Mutex
	cachedNodeID   string
)

func (s *localServer) loadDaemonNodeID(baseURL, token string) (string, error) {
	cachedNodeIDMu.Lock()
	defer cachedNodeIDMu.Unlock()
	if cachedNodeID != "" {
		return cachedNodeID, nil
	}
	if s.deps.ProbeNodeID == nil {
		return "", errors.New("ProbeNodeID callback not configured")
	}
	id, err := s.deps.ProbeNodeID(baseURL, token)
	if err != nil {
		return "", err
	}
	cachedNodeID = id
	return id, nil
}

func nodeAPIRequest(baseURL, token, method, path string, body any) (int, []byte, error) {
	if baseURL == "" {
		return 0, nil, errors.New("daemon base URL is not configured")
	}
	url := strings.TrimRight(baseURL, "/") + path
	var reqBody *strings.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reqBody = strings.NewReader(string(data))
	}
	var req *http.Request
	var err error
	if reqBody != nil {
		req, err = http.NewRequest(method, url, reqBody)
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return resp.StatusCode, nil, rerr
	}
	return resp.StatusCode, raw, nil
}

func resolveDaemonBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("MESH_API_URL")); v != "" {
		return normalizeBaseURL(v)
	}
	return "https://api.projectmesh.io"
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func stateName(state meshv1.SessionState) string {
	switch state {
	case meshv1.SessionState_SESSION_STATE_RUNNING:
		return "running"
	case meshv1.SessionState_SESSION_STATE_EXITED:
		return "exited"
	case meshv1.SessionState_SESSION_STATE_CRASHED:
		return "crashed"
	default:
		return "unknown"
	}
}

// -- discord daemon handlers --

type DiscordAccountStatus struct {
	Account         string                   `json:"account"`
	Supervised      bool                     `json:"supervised"`
	SupervisorState string                   `json:"supervisorState"`
	Live            bool                     `json:"live"`
	Gateway         *accounts.GatewayStatus  `json:"gateway,omitempty"`
}

func (s *localServer) handleDiscordPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if req.Account == "" {
		writeError(w, http.StatusBadRequest, "account is required")
		return
	}
	if s.chanMgr != nil {
		s.chanMgr.StartAccount("discord", req.Account)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "account": req.Account})
}

func (s *localServer) handleDiscordUnpair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if req.Account == "" {
		writeError(w, http.StatusBadRequest, "account is required")
		return
	}
	if s.chanMgr != nil {
		s.chanMgr.StopAccount("discord", req.Account)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "account": req.Account})
}

func (s *localServer) handleDiscordListChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	account := r.URL.Query().Get("account")
	if account == "" {
		writeError(w, http.StatusBadRequest, "account query param is required")
		return
	}
	sockPath, err := paths.DiscordGatewaySockPath(account)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	conn, rd, wr, err := ipc.DialGateway(sockPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("gateway not running: %v", err))
		return
	}
	defer conn.Close()

	if err := wr.Write(ipc.Envelope{V: ipc.Version, Type: "list-channels"}); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("gateway write: %v", err))
		return
	}
	env, err := rd.Read()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("gateway read: %v", err))
		return
	}
	if env.Type == "error" {
		writeError(w, http.StatusBadGateway, env.Reason+": "+env.Detail)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": json.RawMessage(env.Content)})
}

func (s *localServer) handleDiscordStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	requested := strings.TrimSpace(r.URL.Query().Get("account"))

	var names []string
	if requested != "" {
		names = []string{requested}
	} else {
		all, err := accounts.ListDiscordAccounts()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		names = all
	}

	out := make([]DiscordAccountStatus, 0, len(names))
	for _, n := range names {
		entry := DiscordAccountStatus{Account: n}
		entry.Supervised = s.chanMgr != nil && s.chanMgr.IsManaging("discord", n)
		if entry.Supervised {
			entry.SupervisorState = "supervised"
		} else {
			entry.SupervisorState = "not-referenced"
		}
		entry.Live = discordGatewayLive(n)
		if entry.Live {
			st, err := accounts.ReadGatewayStatus("discord", n)
			if err == nil {
				entry.Gateway = &st
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func discordGatewayLive(account string) bool {
	sockPath, err := paths.DiscordGatewaySockPath(account)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// -- whatsapp daemon handlers --

type WhatsappAccountStatus struct {
	Account         string                   `json:"account"`
	Supervised      bool                     `json:"supervised"`
	SupervisorState string                   `json:"supervisorState"`
	Live            bool                     `json:"live"`
	Gateway         *accounts.GatewayStatus  `json:"gateway,omitempty"`
}

func (s *localServer) handleWhatsappPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if req.Account == "" {
		writeError(w, http.StatusBadRequest, "account is required")
		return
	}
	if s.chanMgr != nil {
		s.chanMgr.StartAccount("whatsapp", req.Account)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "account": req.Account})
}

func (s *localServer) handleWhatsappUnpair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if req.Account == "" {
		writeError(w, http.StatusBadRequest, "account is required")
		return
	}
	if s.chanMgr != nil {
		s.chanMgr.StopAccount("whatsapp", req.Account)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "account": req.Account})
}

func (s *localServer) handleWhatsappStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	requested := strings.TrimSpace(r.URL.Query().Get("account"))

	var names []string
	if requested != "" {
		names = []string{requested}
	} else {
		all, err := accounts.ListWhatsappAccounts()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		names = all
	}

	out := make([]WhatsappAccountStatus, 0, len(names))
	for _, n := range names {
		entry := WhatsappAccountStatus{Account: n}
		entry.Supervised = s.chanMgr != nil && s.chanMgr.IsManaging("whatsapp", n)
		if entry.Supervised {
			entry.SupervisorState = "supervised"
		} else {
			entry.SupervisorState = "not-referenced"
		}
		entry.Live = whatsappGatewayLive(n)
		if entry.Live {
			st, err := accounts.ReadGatewayStatus("whatsapp", n)
			if err == nil {
				entry.Gateway = &st
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func whatsappGatewayLive(account string) bool {
	sockPath, err := paths.WhatsappGatewaySockPath(account)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// -- peers daemon handlers --

type peersListResponse struct {
	Peers []peerListItem `json:"peers"`
}

type peerListItem struct {
	PeerID     string   `json:"peer_id"`
	NodeID     string   `json:"node_id"`
	Teams      []string `json:"teams"`
	Summary    string   `json:"summary,omitempty"`
	LastSeenAt string   `json:"last_seen_at"`
}

func (s *localServer) handlePeersList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	team := strings.TrimSpace(r.URL.Query().Get("team"))
	peerList := s.broker.ListPeers(team)

	items := make([]peerListItem, 0, len(peerList))
	for _, p := range peerList {
		teams := p.Teams
		if teams == nil {
			teams = []string{}
		}
		items = append(items, peerListItem{
			PeerID:     p.PeerID,
			NodeID:     p.NodeID,
			Teams:      teams,
			Summary:    p.Summary,
			LastSeenAt: p.LastSeenAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, peersListResponse{Peers: items})
}

func (s *localServer) handlePeersSetSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		PeerID  string `json:"peer_id"`
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if req.PeerID == "" {
		writeError(w, http.StatusBadRequest, "peer_id is required")
		return
	}
	if req.Summary == "" {
		writeError(w, http.StatusBadRequest, "summary is required")
		return
	}
	ok := s.broker.SetSummary(req.PeerID, req.Summary)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("peer %q not registered", req.PeerID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "peer_id": req.PeerID})
}
