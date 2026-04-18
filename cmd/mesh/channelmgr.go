package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// RunningChannel is the daemon-side handle for one spawned channel server.
// Cmd.Process is the subprocess; Spec is the config that produced it so we can
// round-trip through Status / restart without re-resolving. `exited` is set by
// the reaper goroutine so Status can report running=false without racing on
// exec.Cmd.ProcessState (which isn't safe to read concurrently with Wait).
type RunningChannel struct {
	AgentName string
	Type      string
	Index     int
	Cmd       *exec.Cmd
	Spec      ChannelSpec
	exited    atomic.Bool
}

// ChannelStatus is the per-channel view surfaced via `mesh inspect`. Slice 1
// fills only Type / Running / Pid; message counters come with the real channel
// implementations (slice 2+).
type ChannelStatus struct {
	Type            string `json:"type"`
	Index           int    `json:"index"`
	Running         bool   `json:"running"`
	Pid             int    `json:"pid,omitempty"`
	MessagesIn      int    `json:"messagesIn,omitempty"`
	MessagesOut     int    `json:"messagesOut,omitempty"`
	MessagesDropped int    `json:"messagesDropped,omitempty"`
}

// ChannelManager owns the lifecycle of channel subprocesses for every agent
// the daemon has spawned. One per daemon; callers reach it through the
// package-level channelMgr singleton.
type ChannelManager struct {
	mu     sync.Mutex
	agents map[string][]*RunningChannel
}

func NewChannelManager() *ChannelManager {
	return &ChannelManager{agents: map[string][]*RunningChannel{}}
}

// channelMgr is the daemon-wide singleton. Created lazily so tests and CLI
// commands that import the package without starting a daemon don't pay the
// initialization cost.
var (
	channelMgrOnce sync.Once
	channelMgr     *ChannelManager
)

func ChannelMgr() *ChannelManager {
	channelMgrOnce.Do(func() { channelMgr = NewChannelManager() })
	return channelMgr
}

// Spawn launches one subprocess per channel in agent.Channels. Each channel's
// stdio is redirected to ~/.mesh/agents/<name>/channels/<type>-<idx>.{stdin,stdout}.log
// for debugging; slice 2 will wire these through MCP instead. Safe to call
// when an agent already has running channels — existing processes are left
// alone.
func (m *ChannelManager) Spawn(agent *AgentDef) error {
	if agent == nil || len(agent.Channels) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.agents[agent.Name]; ok {
		// Existing set — don't double-spawn. Callers invoke Restart for that.
		return nil
	}

	workdir, err := agentWorkdir(agent.Name)
	if err != nil {
		return err
	}
	channelsDir := filepath.Join(workdir, "channels")
	if err := os.MkdirAll(channelsDir, 0o700); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve mesh binary: %w", err)
	}

	var running []*RunningChannel
	for i, ch := range agent.Channels {
		args := []string{"channel-server", ch.Type, "--agent", agent.Name, "--index", fmt.Sprintf("%d", i)}
		cmd := exec.Command(exe, args...)
		cmd.Dir = workdir

		stdoutPath := filepath.Join(channelsDir, fmt.Sprintf("%s-%d.stdout.log", ch.Type, i))
		stderrPath := filepath.Join(channelsDir, fmt.Sprintf("%s-%d.stderr.log", ch.Type, i))
		stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open channel stdout %s: %w", stdoutPath, err)
		}
		stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			_ = stdout.Close()
			return fmt.Errorf("open channel stderr %s: %w", stderrPath, err)
		}
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		// No stdin in slice 1 — claude will own stdin in slice 2 via MCP.
		// For now the echo server treats EOF on stdin as "no host yet" and
		// keeps running on its notification loop.
		cmd.Stdin = nil

		if err := cmd.Start(); err != nil {
			_ = stdout.Close()
			_ = stderr.Close()
			return fmt.Errorf("start channel %s[%d]: %w", ch.Type, i, err)
		}
		fmt.Printf("mesh daemon channel spawn agent=%s type=%s idx=%d pid=%d\n",
			agent.Name, ch.Type, i, cmd.Process.Pid)

		rc := &RunningChannel{
			AgentName: agent.Name,
			Type:      ch.Type,
			Index:     i,
			Cmd:       cmd,
			Spec:      ch,
		}
		running = append(running, rc)

		// Reap the process when it exits so Status reflects reality.
		go func(rc *RunningChannel, stdout, stderr *os.File) {
			_ = rc.Cmd.Wait()
			rc.exited.Store(true)
			_ = stdout.Close()
			_ = stderr.Close()
		}(rc, stdout, stderr)
	}
	m.agents[agent.Name] = running
	return nil
}

// Shutdown signals each channel subprocess and waits up to 5 s per process
// before escalating to SIGKILL. Safe to call for agents that have no running
// channels — no-op.
func (m *ChannelManager) Shutdown(agentName string) {
	m.mu.Lock()
	running, ok := m.agents[agentName]
	delete(m.agents, agentName)
	m.mu.Unlock()

	if !ok {
		return
	}
	for _, rc := range running {
		m.shutdownOne(rc)
	}
}

func (m *ChannelManager) shutdownOne(rc *RunningChannel) {
	if rc.Cmd == nil || rc.Cmd.Process == nil {
		return
	}
	// Best-effort SIGTERM; if the process is already gone that's fine. The
	// reaper goroutine handles the actual Wait() — we just poll exited.Load.
	_ = rc.Cmd.Process.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for !rc.exited.Load() {
		if time.Now().After(deadline) {
			fmt.Printf("mesh daemon channel shutdown agent=%s type=%s idx=%d (SIGKILL escalated)\n",
				rc.AgentName, rc.Type, rc.Index)
			_ = rc.Cmd.Process.Kill()
			// Give the reaper a moment to run.
			for !rc.exited.Load() {
				time.Sleep(20 * time.Millisecond)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("mesh daemon channel shutdown agent=%s type=%s idx=%d (SIGTERM)\n",
		rc.AgentName, rc.Type, rc.Index)
}

// Restart is Shutdown + Spawn. The caller is responsible for having updated
// the agent's Channels on disk (via UpdateAgent) before invoking.
func (m *ChannelManager) Restart(agent *AgentDef) error {
	if agent == nil {
		return nil
	}
	m.Shutdown(agent.Name)
	return m.Spawn(agent)
}

// Status returns a snapshot per channel for the named agent. Agents with no
// running channels yield an empty slice. Exited processes surface Running=false
// with the last-known pid so `mesh inspect` can show "crashed".
func (m *ChannelManager) Status(agentName string) []ChannelStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	running, ok := m.agents[agentName]
	if !ok {
		return nil
	}
	out := make([]ChannelStatus, 0, len(running))
	for _, rc := range running {
		st := ChannelStatus{Type: rc.Type, Index: rc.Index}
		if rc.Cmd != nil && rc.Cmd.Process != nil {
			st.Pid = rc.Cmd.Process.Pid
			st.Running = !rc.exited.Load()
		}
		out = append(out, st)
	}
	return out
}
