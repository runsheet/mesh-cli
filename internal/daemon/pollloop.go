package daemon

// pollloop.go — spec 04 agent-side polling fallback.
//
// When the gRPC poke chain is broken, tasks assigned to local agents
// would never be surfaced. This loop periodically fetches
// GET /task/list?assignedTo=<cloudAgentUUID> for each pinned agent and
// delivers pokes for any items not yet seen.
//
// Cadence per spec 04:
//   - 60s  when the agent is "active" (recently received a poke, completed
//     task-update / handoff, or had a new item surfaced by the poll).
//   - 300s when idle (no activity for >5 minutes).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/state"
)

// httpClient is shared across poll requests. 30s timeout matches the pattern
// used by other HTTP helpers in this package.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// newNodeRequest builds an authenticated HTTP request using the node token.
func newNodeRequest(ctx context.Context, token, method, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, err
}

const (
	pollCadenceActive = 60 * time.Second
	pollCadenceIdle   = 300 * time.Second
	pollActiveWindow  = 5 * time.Minute
)

// pollActivityCh is a buffered channel the daemon sends agent names into
// whenever any of the three activity triggers fires:
//  1. gRPC TaskPoke delivered successfully.
//  2. Shim mesh_update_task / mesh_handoff_task handled.
//  3. Poll itself surfaces a new unseen item.
//
// The poll loop drains this channel and resets the active timer.
var pollActivityCh = make(chan string, 64)

// MarkAgentActive signals that agentName has had recent task activity.
// Called from daemon.go (deliverTaskPoke) and from the RouterDeps callback.
// Safe to call from any goroutine.
func MarkAgentActive(agentName string) {
	select {
	case pollActivityCh <- agentName:
	default:
		// Channel full — drop; the poll loop will still pick up the next tick.
	}
}

// taskListRow is the subset of fields we need from /task/list items.
type taskListRow struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	UpdatedAt    string `json:"updated_at"`
	CurrentOwner string `json:"current_owner"`
}

// taskListResp is the JSON shape of GET /task/list.
type taskListResp struct {
	OK    bool          `json:"ok"`
	Items []taskListRow `json:"items"`
}

// seenKey identifies a (agent, task) pair in the last-seen map.
type seenKey struct{ agentName, taskID string }

// agentCadenceState tracks per-agent activity timing.
type agentCadenceState struct {
	lastActivAt time.Time // zero = never active
}

func (s *agentCadenceState) isActive() bool {
	return !s.lastActivAt.IsZero() && time.Since(s.lastActivAt) < pollActiveWindow
}

func (s *agentCadenceState) cadence() time.Duration {
	if s.isActive() {
		return pollCadenceActive
	}
	return pollCadenceIdle
}

// pollAgents is the entry point called from runClusteredDaemon as a goroutine.
// It runs until ctx is cancelled.
func pollAgents(ctx context.Context, baseURL, nodeToken string) {
	if baseURL == "" || nodeToken == "" {
		return
	}

	var (
		mu       sync.Mutex
		cadence  = map[string]*agentCadenceState{} // agentName → state
		lastSeen = map[seenKey]string{}            // → updatedAt string
	)

	markActive := func(agentName string) {
		mu.Lock()
		s := cadence[agentName]
		if s == nil {
			s = &agentCadenceState{}
			cadence[agentName] = s
		}
		s.lastActivAt = time.Now()
		mu.Unlock()
	}

	// nextTick returns the shortest cadence across all known agents, so the
	// outer loop wakes at the right frequency even with mixed active/idle agents.
	nextTick := func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		d := pollCadenceIdle
		for _, s := range cadence {
			if c := s.cadence(); c < d {
				d = c
			}
		}
		return d
	}

	// Run one poll pass for all local agents with a cloud ID.
	runPass := func() {
		agents, err := state.ListAgents()
		if err != nil {
			fmt.Printf("mesh poll: list agents err: %v\n", err)
			return
		}
		for _, a := range agents {
			if a.CloudAgentID == nil || *a.CloudAgentID == "" {
				continue
			}
			pollOneAgent(ctx, baseURL, nodeToken, a.Name, *a.CloudAgentID,
				&mu, cadence, lastSeen, markActive)
		}
	}

	// Initial pass immediately so we don't miss items that arrived while the
	// daemon was restarting.
	runPass()

	ticker := time.NewTicker(nextTick())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case agentName := <-pollActivityCh:
			markActive(agentName)
			// Reset ticker to the new (shorter) cadence.
			ticker.Reset(nextTick())

		case <-ticker.C:
			// Drain any pending activity signals before the pass so they're
			// reflected in the pass's cadence decisions.
		drainActivity:
			for {
				select {
				case agentName := <-pollActivityCh:
					markActive(agentName)
				default:
					break drainActivity
				}
			}

			runPass()
			ticker.Reset(nextTick())
		}
	}
}

// skipPollStatus lists terminal statuses that don't need polling.
var skipPollStatus = map[string]bool{
	"done": true, "archived": true,
}

// pollOneAgent fetches /task/list for one agent and delivers pokes for
// any items not already seen at their current updatedAt.
func pollOneAgent(
	ctx context.Context,
	baseURL, nodeToken string,
	localName, cloudAgentID string,
	mu *sync.Mutex,
	cadence map[string]*agentCadenceState,
	lastSeen map[seenKey]string,
	markActive func(string),
) {
	if ctx.Err() != nil {
		return
	}

	mu.Lock()
	s := cadence[localName]
	if s == nil {
		s = &agentCadenceState{}
		cadence[localName] = s
	}
	mu.Unlock()

	url := strings.TrimRight(baseURL, "/") + "/task/list?assignedTo=" + cloudAgentID
	req, err := newNodeRequest(ctx, nodeToken, "GET", url)
	if err != nil {
		fmt.Printf("mesh poll agent=%s req err: %v\n", localName, err)
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Printf("mesh poll agent=%s fetch err: %v\n", localName, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Printf("mesh poll agent=%s fetch status=%d\n", localName, resp.StatusCode)
		return
	}

	var parsed taskListResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		fmt.Printf("mesh poll agent=%s decode err: %v\n", localName, err)
		return
	}

	for _, item := range parsed.Items {
		if skipPollStatus[item.Status] {
			continue
		}

		k := seenKey{agentName: localName, taskID: item.ID}
		mu.Lock()
		prev := lastSeen[k]
		mu.Unlock()

		if prev == item.UpdatedAt && prev != "" {
			continue // already notified for this version of the item
		}

		// New or updated item: deliver a poke and record it.
		mu.Lock()
		lastSeen[k] = item.UpdatedAt
		mu.Unlock()

		deliverPollPoke(localName, item.ID, "poll")
		markActive(localName)
		fmt.Printf("mesh poll agent=%s task=%s surfaced (status=%s)\n",
			localName, item.ID, item.Status)
	}
}

// deliverPollPoke is a no-op since Plan 00-05 vacated the channel-routing
// seat. The Phase 2 dispatcher rebuild (DSP-01..07) will reattach a real
// delivery path; until then poll-surfaced items are logged but not pushed
// to the agent.
func deliverPollPoke(localName, taskID, reason string) {
	fmt.Printf("mesh poll poke drop agent=%s task=%s reason=no-dispatcher (Phase 2 will rewire)\n",
		localName, taskID)
}
