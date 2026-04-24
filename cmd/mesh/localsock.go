package main

import (
	"context"
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

	meshv1 "projectmesh.io/mesh-cli/proto/meshv1"
)

const daemonSocketName = "daemon.sock"

// DaemonSocketPath is ~/.mesh/daemon.sock. Exported so command code can print
// nice errors.
func DaemonSocketPath() (string, error) {
	dir, err := meshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, daemonSocketName), nil
}

// StartLocalSocket starts the Unix-socket HTTP server that serves the local
// daemon surface (mesh run/ps/attach/logs/...). It takes the same
// SessionRegistry the cluster-mode gRPC handlers use, so standalone-mode
// agents and pinned cluster agents share process-local state.
//
// chanMgr may be nil (used in tests / standalone contexts that don't need
// gateway supervision).
//
// Returns the listener so the caller can shut it down; server runs until
// ctx is cancelled.
func StartLocalSocket(ctx context.Context, registry *SessionRegistry, chanMgr *ChannelManager) (net.Listener, error) {
	if _, err := ensureMeshDir(); err != nil {
		return nil, err
	}

	sockPath, err := DaemonSocketPath()
	if err != nil {
		return nil, err
	}

	// Stale socket cleanup — if the previous daemon crashed we'll leave a dead
	// file behind. Remove it before bind; ignore "not found".
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

	srv := &localServer{registry: registry, chanMgr: chanMgr}
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
	mux.HandleFunc("/peers/list", srv.handlePeersList)
	mux.HandleFunc("/peers/set-summary", srv.handlePeersSetSummary)

	httpSrv := &http.Server{
		Handler:      mux,
		ReadTimeout:  0, // attach hijacks and runs long; no server-side timeout.
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
	registry *SessionRegistry
	chanMgr  *ChannelManager
}

// -- JSON payload shapes (kept here so there's one source of truth for both
// server and client). --

type sessionView struct {
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

type createSessionRequest struct {
	Name                       string            `json:"name"`
	TemplateSource             string            `json:"templateSource"`
	Env                        map[string]string `json:"env,omitempty"`
	AutoStart                  *bool             `json:"autoStart,omitempty"`
	DangerouslySkipPermissions *bool             `json:"dangerouslySkipPermissions,omitempty"`
	// Values is the already-merged (client-side) --values/--set map layered on
	// top of the template's values.yaml during createSession.
	Values map[string]any `json:"values,omitempty"`
}

type createSessionResponse struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	SessionID string `json:"sessionId,omitempty"`
}

type errorResponse struct {
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

// handleSessionItem routes /sessions/<name>[/verb].
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
	defs, err := ListAgents()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	live := map[string]PtySessionSummary{}
	for _, sum := range s.registry.List() {
		live[sum.AgentID] = sum
	}

	out := make([]sessionView, 0, len(defs))
	for _, def := range defs {
		sv := sessionView{
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
	var req createSessionRequest
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

	src, err := ResolveTemplateSource(req.TemplateSource)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Git templates fetch into the cache; local paths live in place. After this
	// we need the resolved directory to layer values.yaml on top of the client-
	// supplied overrides and render.
	tplDir, gitSHA, gitRef, err := materializeTemplateDir(src)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate systemPromptPath before we do anything persistent.
	if err := validateSystemPromptPathInDir(tplDir, name, req.Values); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Render with merged values. agentBytes / promptBytes are the frozen
	// rendered output that lands in the snapshot.
	base, err := LoadValues(tplDir, nil, nil, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mergedValues := deepMerge(base, req.Values)
	agentBytes, promptBytes, err := RenderTemplate(tplDir, name, mergedValues)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tpl, values, err := readTemplateWithValues(tplDir, name, req.Values)
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

	// Effective skip-permissions: CLI override wins; falls back to template.
	skipPerms := tpl.DangerouslySkipPermissions
	if req.DangerouslySkipPermissions != nil {
		skipPerms = *req.DangerouslySkipPermissions
	}

	// Write the snapshot before persisting the agent def so a failed snapshot
	// doesn't leave an orphan state.json entry.
	if err := snapshotAgent(name, agentBytes, promptBytes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	def := &AgentDef{
		Name:              name,
		TemplateSource:    req.TemplateSource,
		TemplateRef:       tpl.GitRef,
		TemplateSha:       tpl.GitSHA,
		TemplateCachePath: tpl.ResolvedPath,
		Model:             tpl.Model,
		RequiredEnv:       append([]string{}, tpl.RequiredEnv...),
		CreatedAt:         time.Now().UTC(),
		// Pre-generate Claude's session UUID so restart and auto-start land in
		// the same conversation. Regenerated only on explicit new-up.
		ClaudeSessionID:            uuid.NewString(),
		DangerouslySkipPermissions: skipPerms,
		Channels:                   append([]ChannelSpec{}, tpl.Channels...),
		Values:                     values,
	}
	if err := AddAgent(def); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Sync ChannelManager so any newly-referenced accounts get gateways.
	if s.chanMgr != nil {
		if all, err := ListAgents(); err == nil {
			s.chanMgr.Sync(all)
		}
	}

	autoStart := true
	if req.AutoStart != nil {
		autoStart = *req.AutoStart
	}

	resp := createSessionResponse{Name: name, Status: "created"}
	if autoStart {
		// Prompt file is written from the snapshot (no re-render needed).
		if err := writeAgentPromptFile(name, tpl.SystemPrompt); err != nil {
			fmt.Printf("mesh daemon prompt-file err name=%s: %v\n", name, err)
		}
		if err := WriteClaudeSettings(name, def.Channels, effectiveEnv); err != nil {
			fmt.Printf("mesh daemon claude-settings err name=%s: %v\n", name, err)
		}
		sess, _, err := s.registry.StartWithOptions(StartOptions{
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
		_ = UpdateAgent(name, func(a *AgentDef) { a.LastSessionID = sess.SessionID })
		resp.Status = "running"
		resp.SessionID = sess.SessionID
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *localServer) inspectSession(w http.ResponseWriter, _ *http.Request, name string) {
	def, ok, err := GetAgent(name)
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
			"shortSha":  ShortSHA(def.TemplateSha),
			"cachePath": def.TemplateCachePath,
		},
		"requiredEnv": def.RequiredEnv,
	}
	if def.CloudAgentID != nil && *def.CloudAgentID != "" {
		out["cloudAgentId"] = *def.CloudAgentID
	}
	if len(def.Channels) > 0 {
		// Model B: channel subprocesses are owned by claude, not the daemon, so
		// we can only surface configured type/index plus whatever status the
		// channel-server wrote to its side-file. Missing file → status "unknown".
		out["channels"] = ChannelStatusesForAgent(name, def.Channels)
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
	_, ok, err := GetAgent(name)
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
	if _, err := RemoveAgent(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Sync ChannelManager so gateways for no-longer-referenced accounts stop.
	if s.chanMgr != nil {
		if all, err := ListAgents(); err == nil {
			s.chanMgr.Sync(all)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "status": "removed"})
}

func (s *localServer) restartSession(w http.ResponseWriter, _ *http.Request, name string) {
	def, ok, err := GetAgent(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}

	// Load runtime config from the snapshot instead of re-resolving the template.
	// This is the key invariant: deleting the original template dir must not
	// break restart.
	if !snapshotExists(name) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("agent %q has no snapshot — run `mesh update %s` to rebuild it, or `mesh rm %s` to remove", name, name, name))
		return
	}
	snapshotAY, err := loadSnapshotAgentYAML(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("read snapshot: %v", err))
		return
	}
	snapshotPrompt, err := loadSnapshotSystemPrompt(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("read snapshot: %v", err))
		return
	}

	// Refresh the prompt file (appends loop-template) from the snapshot content.
	if err := writeAgentPromptFile(name, snapshotPrompt); err != nil {
		fmt.Printf("mesh daemon prompt-file err name=%s: %v\n", name, err)
	}

	// Channels come from state.json (set at run/update time) — snapshot doesn't
	// store them separately.
	effectiveEnv := mergeStringMaps(os.Environ(), nil)
	required := def.RequiredEnv
	if len(snapshotAY.Spec.RequiredEnv) > 0 {
		required = snapshotAY.Spec.RequiredEnv
	}
	if missing := missingRequiredEnv(required, effectiveEnv); len(missing) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("required env not set: %s", strings.Join(missing, ", ")))
		return
	}

	// Migrate legacy agents that predate pre-generated session IDs so they
	// start resuming their conversation from now on.
	if def.ClaudeSessionID == "" {
		def.ClaudeSessionID = uuid.NewString()
		_ = UpdateAgent(name, func(a *AgentDef) { a.ClaudeSessionID = def.ClaudeSessionID })
	}

	model := def.Model
	if model == "" {
		model = snapshotAY.Spec.Model
	}

	opts := StartOptions{
		AgentID:                    name,
		Model:                      model,
		ResumeID:                   def.ClaudeSessionID,
		Env:                        pickEnv(effectiveEnv, required),
		DangerouslySkipPermissions: def.DangerouslySkipPermissions,
	}

	// Refresh settings.json before claude comes back up so it re-reads the
	// MCP config on spawn. Model B: daemon does not spawn the channel
	// subprocesses itself — claude does.
	if err := WriteClaudeSettings(name, def.Channels, effectiveEnv); err != nil {
		fmt.Printf("mesh daemon claude-settings err name=%s: %v\n", name, err)
	}

	sess, err := s.registry.NewUpWithOptions(opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = UpdateAgent(name, func(a *AgentDef) { a.LastSessionID = sess.SessionID })
	writeJSON(w, http.StatusOK, map[string]string{
		"name":      name,
		"sessionId": sess.SessionID,
		"status":    "running",
	})
}

func (s *localServer) updateSession(w http.ResponseWriter, _ *http.Request, name string) {
	def, ok, err := GetAgent(name)
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

	src, err := ResolveTemplateSource(def.TemplateSource)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Preserve the user's original ref when re-resolving; a missing ref keeps
	// resolving HEAD so `mesh update` tracks the moving branch.
	if def.TemplateRef != "" {
		src.Ref = def.TemplateRef
	}

	tplDir, gitSHA, _, err := materializeTemplateDir(src)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Validate systemPromptPath before making persistent changes.
	if err := validateSystemPromptPathInDir(tplDir, name, def.Values); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Render raw bytes for the snapshot.
	base, err := LoadValues(tplDir, nil, nil, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mergedValues := deepMerge(base, def.Values)
	agentBytes, promptBytes, err := RenderTemplate(tplDir, name, mergedValues)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	tpl, _, err := readTemplateWithValues(tplDir, name, def.Values)
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

	if newSHA == oldSHA && oldSHA != "" && snapshotExists(name) {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Write the new snapshot unconditionally — even if SHA hasn't changed, this
	// ensures previously-missing snapshots (pre-migration agents) are populated.
	if err := snapshotAgent(name, agentBytes, promptBytes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	_ = UpdateAgent(name, func(a *AgentDef) {
		a.TemplateSha = newSHA
		a.TemplateCachePath = tpl.ResolvedPath
		a.RequiredEnv = append([]string{}, tpl.RequiredEnv...)
		a.Channels = append([]ChannelSpec{}, tpl.Channels...)
		if a.Model == "" {
			a.Model = tpl.Model
		}
	})
	def.Channels = append([]ChannelSpec{}, tpl.Channels...)

	if wasRunning {
		effectiveEnv := mergeStringMaps(os.Environ(), nil)
		if missing := missingRequiredEnv(tpl.RequiredEnv, effectiveEnv); len(missing) > 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("required env not set: %s", strings.Join(missing, ", ")))
			return
		}
		_ = writeAgentPromptFile(name, tpl.SystemPrompt)
		if err := WriteClaudeSettings(name, def.Channels, effectiveEnv); err != nil {
			fmt.Printf("mesh daemon claude-settings err name=%s: %v\n", name, err)
		}
		sess, err := s.registry.NewUpWithOptions(StartOptions{
			AgentID:    name,
			Model:      def.Model,
			Env:        pickEnv(effectiveEnv, tpl.RequiredEnv),
			WorkingDir: tpl.WorkingDir,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = UpdateAgent(name, func(a *AgentDef) { a.LastSessionID = sess.SessionID })
		resp["restarted"] = true
		resp["sessionId"] = sess.SessionID
	}
	resp["status"] = "updated"
	writeJSON(w, http.StatusOK, resp)
}

// reloadAgentTemplate re-materializes the template referenced by def. Git
// sources use the cached SHA to avoid a network hop; only `mesh update`
// re-resolves the ref. Renders with no stored values — used by flows (push,
// loop-template backfill) that only need the static prompt/tools shape.
func reloadAgentTemplate(def *AgentDef) (*Template, error) {
	src, err := ResolveTemplateSource(def.TemplateSource)
	if err != nil {
		return nil, err
	}
	if src.Kind == "git" && def.TemplateSha != "" {
		// Pin to the known-good SHA so slow networks don't block restart.
		src.Ref = def.TemplateSha
	}
	return LoadTemplate(src)
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

	// The initial snapshot comes through the frame channel (AttachWithID
	// primes it). For non-follow mode we drain the channel until it stalls
	// briefly, then return.
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

	// Non-follow: drain what's immediately available and return. A 50 ms
	// idle window after the last frame is the "nothing more for now" signal.
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

	// 101 Switching Protocols signals to the client's http.ReadResponse that
	// the body has no framing — no Content-Length, no chunked transfer — and
	// the caller owns the raw bytes from here on.
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
	_ = dropped // Dropped-bytes marker isn't meaningful mid-stream for interactive attach.

	// PTY → socket
	done := make(chan struct{})
	go func() {
		defer close(done)
		for chunk := range frames {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	}()

	// socket → PTY
	readBuf := make([]byte, 4096)
	// brw.Reader has a small buffer from the HTTP header read; flush it first.
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
	// Let the PTY→socket goroutine drain naturally when frames closes.
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
	entries, err := ListCachedTemplates()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

type pullTemplateRequest struct {
	Source string `json:"source"`
}

type pullTemplateResponse struct {
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
	var req pullTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	src, err := ResolveTemplateSource(req.Source)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if src.Kind != "git" {
		writeError(w, http.StatusBadRequest, "only git sources can be pulled; local paths are read in place")
		return
	}
	sha, path, err := FetchGitTemplate(src)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pullTemplateResponse{
		SHA:       sha,
		CachePath: path,
		Ref:       src.Ref,
		URL:       src.URL,
	})
}

// mergeStringMaps turns base (VAR=VALUE slice) plus overrides into a plain map
// keyed by variable name. Overrides win; empty-value entries in overrides
// still overwrite base (so callers can intentionally blank a var).
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

// missingRequiredEnv returns the subset of required var names that are absent
// or empty in env. Preserves input order so error messages are stable.
func missingRequiredEnv(required []string, env map[string]string) []string {
	var missing []string
	for _, name := range required {
		if env[name] == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// pickEnv extracts only the requested variable names from env so we don't
// leak unrelated daemon-process secrets into the Claude child.
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
	writeJSON(w, status, errorResponse{Error: msg})
}

// -- cluster push / pull --

type pushSessionRequest struct {
	ProjectID string `json:"projectId,omitempty"`
	As        string `json:"as,omitempty"`
}

type pushSessionResponse struct {
	Name         string `json:"name"`
	CloudAgentID string `json:"cloudAgentId"`
	ProjectID    string `json:"projectId,omitempty"`
}

type pullSessionRequest struct {
	CloudAgentID string `json:"cloudAgentId"`
	As           string `json:"as,omitempty"`
}

type pullSessionResponse struct {
	Name         string `json:"name"`
	CloudAgentID string `json:"cloudAgentId"`
}

// pushSession lifts a local agent definition into the cluster by calling
// mesh-api's POST /agent/import with the daemon's node token. The new cloud
// agent id is written back onto the local state row so future AgentSync pushes
// can be recognized as already-adopted.
func (s *localServer) pushSession(w http.ResponseWriter, r *http.Request, name string) {
	var req pushSessionRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	def, ok, err := GetAgent(name)
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

	token, tokErr := LoadNodeToken()
	if tokErr != nil || strings.TrimSpace(token) == "" {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error": "not_linked_to_cluster",
		})
		return
	}

	// Re-read the template so description/systemPrompt/tools/model reflect
	// whatever is currently cached. For cloud-originated locals (no template on
	// disk) we fall back to state.json values.
	tpl, tplErr := reloadAgentTemplate(def)
	if tplErr != nil {
		// Not fatal — the agent may have been pulled from cloud and has no
		// template. Use whatever the state row remembers.
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
		// Propagate the rename-collision response to the caller unchanged.
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
	if err := UpdateAgent(name, func(a *AgentDef) { a.CloudAgentID = &cloudID }); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("persist cloudAgentId: %v", err))
		return
	}

	fmt.Printf("mesh daemon adopted name=%s cloudAgentId=%s project=%s\n", name, cloudID, req.ProjectID)

	writeJSON(w, http.StatusOK, pushSessionResponse{
		Name:         name,
		CloudAgentID: cloudID,
		ProjectID:    req.ProjectID,
	})
}

// handleClusterPull materializes a cloud agent definition into local state. It
// intentionally does NOT start the session — adoption is explicit; the user
// runs `mesh restart` to spawn.
func (s *localServer) handleClusterPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req pullSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	cloudID := strings.TrimSpace(req.CloudAgentID)
	if cloudID == "" {
		writeError(w, http.StatusBadRequest, "cloudAgentId is required")
		return
	}

	token, tokErr := LoadNodeToken()
	if tokErr != nil || strings.TrimSpace(token) == "" {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error": "not_linked_to_cluster",
		})
		return
	}

	baseURL := resolveDaemonBaseURL()
	// Node tokens can call /agent/get per spec; but /agent/get currently gates
	// to user principals. Use user session as bearer if available, else fall
	// back to node token and let the server decide. Keeping this simple: use
	// node token — mesh-api may 403, in which case we surface the message.
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
			ID            string          `json:"id"`
			Name          string          `json:"name"`
			SystemPrompt  string          `json:"system_prompt"`
			Model         string          `json:"model"`
			PinnedNodeID  string          `json:"pinned_node_id"`
			ProjectID     *string         `json:"project_id"`
			Tools         json.RawMessage `json:"tools"`
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

	myNodeID, err := loadDaemonNodeID(baseURL, token)
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
	if _, exists, err := GetAgent(localName); err == nil && exists {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "local_name_taken",
		})
		return
	}

	// Materialize the system prompt so subsequent `mesh restart` behaves like
	// any other local agent. No template-cache dir; templateSource uses the
	// "cloud:<id>" sentinel so slice-1 carryover logic skips it.
	if err := writeAgentPromptFile(localName, parsed.Agent.SystemPrompt); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("write prompt: %v", err))
		return
	}

	cloud := parsed.Agent.ID
	def := &AgentDef{
		Name:           localName,
		TemplateSource: "cloud:" + cloud,
		Model:          parsed.Agent.Model,
		CreatedAt:      time.Now().UTC(),
		CloudAgentID:   &cloud,
	}
	if err := AddAgent(def); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, pullSessionResponse{
		Name:         localName,
		CloudAgentID: cloud,
	})
}

// nodeAPIRequest is a minimal HTTP helper for daemon→mesh-api calls that
// authenticate with the node token. Returns raw body so callers can parse
// structured error responses (409 name_taken, etc.) instead of a wrapped
// error string.
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

// resolveDaemonBaseURL reads the same precedence as the CLI (env → default).
// We intentionally don't depend on the user session here — the daemon talks to
// mesh-api on its own behalf using the node token.
func resolveDaemonBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("MESH_API_URL")); v != "" {
		return normalizeBaseURL(v)
	}
	return "https://api.projectmesh.io"
}

// loadDaemonNodeID probes mesh-api's /node/register to learn this node's id.
// Cached in-process so we don't hit the network on every pull.
var (
	cachedNodeIDMu sync.Mutex
	cachedNodeID   string
)

func loadDaemonNodeID(baseURL, token string) (string, error) {
	cachedNodeIDMu.Lock()
	defer cachedNodeIDMu.Unlock()
	if cachedNodeID != "" {
		return cachedNodeID, nil
	}
	// Best path: gRPC Register echoes node id. Re-use probeLocalNodeID logic.
	id, err := probeLocalNodeID(baseURL, token)
	if err != nil {
		return "", err
	}
	cachedNodeID = id
	return id, nil
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

// handleDiscordPair handles POST /discord/pair from `mesh discord pair`.
// The account directory must already be written by the CLI; this handler
// just tells the ChannelManager to start the gateway.
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

// handleDiscordUnpair handles POST /discord/unpair from `mesh discord unpair`.
// The CLI verifies agent refs; this handler stops the gateway.
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

// handleDiscordListChannels handles GET /discord/list-channels?account=<name>
// by proxying to the live gateway over the gateway.sock. The gateway returns a
// JSON list of visible channels. If the gateway is not running it returns an
// informative error.
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
	sockPath, err := DiscordGatewaySockPath(account)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	conn, rd, wr, err := dialGateway(sockPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("gateway not running: %v", err))
		return
	}
	defer conn.Close()

	if err := wr.Write(ipcEnvelope{V: ipcVersion, Type: "list-channels"}); err != nil {
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
	// Pass the raw content field (JSON array of channel objects) to the client.
	writeJSON(w, http.StatusOK, map[string]any{"channels": json.RawMessage(env.Content)})
}

// DiscordAccountStatus is the daemon's authoritative view of one Discord
// account: whether the ChannelManager is supervising it, whether its gateway
// socket is reachable right now, and the latest gateway-published metrics
// when reachable.
type DiscordAccountStatus struct {
	Account         string         `json:"account"`
	Supervised      bool           `json:"supervised"`
	SupervisorState string         `json:"supervisorState"` // "not-referenced" | "supervised"
	Live            bool           `json:"live"`            // gateway socket reachable now
	Gateway         *GatewayStatus `json:"gateway,omitempty"`
}

// handleDiscordStatus handles GET /discord/status[?account=<name>]. The
// daemon — not a stale on-disk file — is the source of truth here. Without
// ?account it returns one entry per paired account.
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
		all, err := ListDiscordAccounts()
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
			st, err := ReadGatewayStatus("discord", n)
			if err == nil {
				entry.Gateway = &st
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// discordGatewayLive returns true if the gateway socket for the given
// account accepts a connection within ~200ms. Cheap liveness probe — we
// close immediately without exchanging any IPC frames.
func discordGatewayLive(account string) bool {
	sockPath, err := DiscordGatewaySockPath(account)
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

// peersListResponse is the shape returned by GET /peers/list.
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

// handlePeersList handles GET /peers/list[?team=<name>].
func (s *localServer) handlePeersList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	team := strings.TrimSpace(r.URL.Query().Get("team"))
	peers := globalPeersBroker.ListPeers(team)

	items := make([]peerListItem, 0, len(peers))
	for _, p := range peers {
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

// handlePeersSetSummary handles POST /peers/set-summary.
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
	ok := globalPeersBroker.SetSummary(req.PeerID, req.Summary)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("peer %q not registered", req.PeerID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "peer_id": req.PeerID})
}
