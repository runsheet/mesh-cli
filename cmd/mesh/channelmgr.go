package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ChannelManager supervises one gateway subprocess per (platform, account)
// referenced by any registered agent. It replaces the v0 stub that only read
// status files.
//
// Gateway lifecycle:
//   - On Sync: compute the desired set of (platform, account) pairs from all
//     registered agents, start missing gateways, stop ones no longer needed.
//   - On gateway crash: respawn with exponential backoff capped at
//     gatewayMaxBackoff.
//
// The daemon calls Sync on boot and on every agent register/remove/restart.
// Gateway subprocesses are started before agent auto-start so shims connect
// to an already-running socket.
type ChannelManager struct {
	mu       sync.Mutex
	gateways map[string]*gatewayProc // key: "platform:account"
	exe      string                  // path to this binary (os.Executable)
}

const (
	gatewayInitialBackoff = 2 * time.Second
	gatewayMaxBackoff     = 60 * time.Second
)

type gatewayProc struct {
	platform string
	account  string
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewChannelManager creates a ChannelManager. exe is the path to the mesh
// binary used to launch gateway subprocesses.
func NewChannelManager(exe string) *ChannelManager {
	return &ChannelManager{
		gateways: map[string]*gatewayProc{},
		exe:      exe,
	}
}

// key returns the stable map key for a (platform, account) pair.
func gatewayKey(platform, account string) string {
	return platform + ":" + account
}

// Sync reconciles the running gateway set against the accounts referenced by
// all currently-registered agents. Safe to call from multiple goroutines.
func (cm *ChannelManager) Sync(agents []*AgentDef) {
	desired := map[string]struct{ platform, account string }{}
	for _, a := range agents {
		for _, ch := range a.Channels {
			switch ch.Type {
			case "discord":
				acc, _ := ch.Config["account"].(string)
				if acc != "" {
					k := gatewayKey("discord", acc)
					desired[k] = struct{ platform, account string }{"discord", acc}
				}
			// future platforms: case "whatsapp": …
			}
		}
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Start any newly-required gateways.
	for k, v := range desired {
		if _, running := cm.gateways[k]; !running {
			cm.startGatewayLocked(v.platform, v.account)
		}
	}

	// Stop gateways whose account is no longer needed.
	for k, gw := range cm.gateways {
		if _, want := desired[k]; !want {
			fmt.Printf("mesh channelmgr: stopping gateway platform=%s account=%s (no longer needed)\n", gw.platform, gw.account)
			gw.cancel()
			<-gw.done
			delete(cm.gateways, k)
		}
	}
}

// StartAccount ensures a gateway for the given account is running. Used when
// `mesh discord pair` tells the daemon to bring up a newly-paired account
// without a full Sync.
func (cm *ChannelManager) StartAccount(platform, account string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	k := gatewayKey(platform, account)
	if _, running := cm.gateways[k]; !running {
		cm.startGatewayLocked(platform, account)
	}
}

// StopAccount stops the gateway for an account. Used when the account is
// unpaired.
func (cm *ChannelManager) StopAccount(platform, account string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	k := gatewayKey(platform, account)
	gw, ok := cm.gateways[k]
	if !ok {
		return
	}
	fmt.Printf("mesh channelmgr: stopping gateway platform=%s account=%s (unpaired)\n", platform, account)
	gw.cancel()
	<-gw.done
	delete(cm.gateways, k)
}

// StopAll stops all supervised gateways. Called on daemon shutdown.
func (cm *ChannelManager) StopAll() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for k, gw := range cm.gateways {
		gw.cancel()
		<-gw.done
		delete(cm.gateways, k)
	}
}

// startGatewayLocked starts the supervision goroutine. Must hold cm.mu.
func (cm *ChannelManager) startGatewayLocked(platform, account string) {
	ctx, cancel := context.WithCancel(context.Background())
	gw := &gatewayProc{
		platform: platform,
		account:  account,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	cm.gateways[gatewayKey(platform, account)] = gw
	fmt.Printf("mesh channelmgr: starting gateway platform=%s account=%s\n", platform, account)
	go cm.superviseGateway(ctx, gw)
}

// superviseGateway runs the gateway subprocess in a retry loop with
// exponential backoff capped at gatewayMaxBackoff. It exits when ctx is
// cancelled.
func (cm *ChannelManager) superviseGateway(ctx context.Context, gw *gatewayProc) {
	defer close(gw.done)
	backoff := gatewayInitialBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		err := cm.runGatewayOnce(ctx, gw.platform, gw.account)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			fmt.Printf("mesh channelmgr: gateway platform=%s account=%s exited: %v (respawn in %s)\n",
				gw.platform, gw.account, err, backoff)
		} else {
			fmt.Printf("mesh channelmgr: gateway platform=%s account=%s exited cleanly (respawn in %s)\n",
				gw.platform, gw.account, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = time.Duration(math.Min(float64(backoff)*2, float64(gatewayMaxBackoff)))
		if backoff < gatewayInitialBackoff {
			backoff = gatewayInitialBackoff
		}
	}
}

// runGatewayOnce executes the gateway subprocess and blocks until it exits.
func (cm *ChannelManager) runGatewayOnce(ctx context.Context, platform, account string) error {
	subCmd := platform + "-gateway"
	cmd := exec.CommandContext(ctx, cm.exe, "channel-server", subCmd, "--account", account)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ChannelStatusesForAgent returns one ChannelStatus per configured channel,
// merging in any status.json a channel-server (shim) has written. Channels
// without a status file come back with Status="unknown".
func ChannelStatusesForAgent(agentName string, channels []ChannelSpec) []ChannelStatus {
	out := make([]ChannelStatus, 0, len(channels))
	for i, ch := range channels {
		st := ChannelStatus{Type: ch.Type, Index: i, Status: "unknown"}
		if disk, ok := readChannelStatusFile(agentName, ch.Type, i); ok {
			if disk.Type != "" {
				st.Type = disk.Type
			}
			st.Status = disk.Status
			st.ChannelID = disk.ChannelID
			st.MessagesIn = disk.MessagesIn
			st.MessagesOut = disk.MessagesOut
			st.MessagesDropped = disk.MessagesDropped
		}
		out = append(out, st)
	}
	return out
}

// ChannelStatus is the per-channel view surfaced via `mesh inspect`.
type ChannelStatus struct {
	Type            string         `json:"type"`
	Index           int            `json:"index"`
	Status          string         `json:"status,omitempty"`
	ChannelID       string         `json:"channelId,omitempty"`
	MessagesIn      int            `json:"messagesIn,omitempty"`
	MessagesOut     int            `json:"messagesOut,omitempty"`
	MessagesDropped map[string]int `json:"messagesDropped,omitempty"`
}

// channelStatusFilePath is the side-channel where shims publish their live
// metrics. The daemon reads it (best-effort) for inspect.
func channelStatusFilePath(agentName, chanType string, index int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/.mesh/agents/%s/channels/%s-%d.status.json", home, agentName, chanType, index), nil
}

func readChannelStatusFile(agentName, chanType string, index int) (ChannelStatus, bool) {
	path, err := channelStatusFilePath(agentName, chanType, index)
	if err != nil {
		return ChannelStatus{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ChannelStatus{}, false
	}
	var st ChannelStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return ChannelStatus{}, false
	}
	st.Index = index
	return st, true
}

// WriteChannelStatusFile is called by shim subcommands to publish metrics.
// Atomic write via temp + rename so concurrent inspect reads never see a
// partial file.
func WriteChannelStatusFile(agentName, chanType string, index int, st ChannelStatus) error {
	path, err := channelStatusFilePath(agentName, chanType, index)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(fmt.Sprintf("%s/.mesh/agents/%s/channels", homeDir(), agentName), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
