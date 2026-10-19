package sessionunit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The files of a session directory.
const (
	OutputFile = "output.jsonl"
	StderrFile = "stderr.log"
	StateFile  = "state.json"
	// PromptFile is a sandboxed session's prompt, which its detached
	// container reads from the file; removed once the agent has spoken.
	PromptFile = ".prompt"
)

// SandboxRunner is the Runner of a session whose agent runs detached in
// its own pod: its unit is the container.
const SandboxRunner = "podman"

// Dir is the session directory of a work dir: its sibling
// `<workdir>.session`, outside the git checkout.
func Dir(workdir string) string {
	return filepath.Clean(workdir) + ".session"
}

// State is what the next node process needs to adopt a session and
// resume reporting it. It never holds the prompt, a secret, the item's
// token or the environment: an adopting node does not need them.
type State struct {
	NodeID string `json:"nodeId,omitempty"`
	// ItemID is the item of a build, evaluate or rebase session; GoalID
	// the goal (a plan's subject); DirectionID a research session's.
	ItemID      string `json:"itemId,omitempty"`
	GoalID      string `json:"goalId,omitempty"`
	DirectionID string `json:"directionId,omitempty"`
	// Attempt is the item's claim: how many attempts it had when claimed.
	Attempt int    `json:"attempt,omitempty"`
	Kind    string `json:"kind"`
	Label   string `json:"label,omitempty"`
	Model   string `json:"model,omitempty"`
	Account string `json:"account,omitempty"`
	// Harness is the agent: "claude-code", or "codex" for a Codex
	// session, whose adopter is the Codex harness.
	Harness string `json:"harness,omitempty"`
	// Runner is the Unit's Name; PID and Unit where the session runs.
	// A sandboxed session's is SandboxRunner: no pid, its Unit the
	// agent's container, beside it the Pod and the Home it was given.
	Runner string `json:"runner"`
	PID    int    `json:"pid"`
	Unit   string `json:"unit"`
	// Pod, Container and Home are a sandboxed session's pod, its agent's
	// container and its temporary HOME on the host (mesh-specs ADR 007):
	// what a later node reattaches to wait on, stop and remove. Paths and
	// names only.
	Pod       string    `json:"pod,omitempty"`
	Container string    `json:"container,omitempty"`
	Home      string    `json:"home,omitempty"`
	Workspace string    `json:"workspace"`
	StartedAt time.Time `json:"startedAt"`
	// TimeoutSeconds is the session's wall-clock cap from StartedAt; 0 is
	// none.
	TimeoutSeconds int     `json:"timeoutSeconds,omitempty"`
	MaxBudgetUSD   float64 `json:"maxBudgetUsd,omitempty"`
	// TempDirs are the session's temporary dirs outside its directory
	// (its CLAUDE_CONFIG_DIR, GH_CONFIG_DIR), removed with it. Paths only.
	TempDirs []string `json:"tempDirs,omitempty"`
	// TokenFile is the session's runsheet token file, the bearer its
	// relay reads per request (mesh-cli ADR-029): the path a later node
	// keeps fresh when it adopts the session. A path only.
	TokenFile string `json:"tokenFile,omitempty"`
	// RelaySocket is a pod session's relay endpoint, the unix socket in
	// its HOME the node serves its runsheet relay on, and RelayURL where
	// the endpoint forwards to: a later node serves it again from
	// TokenFile when it adopts the session, so the bearer never enters
	// the pod. Paths and a URL only.
	RelaySocket string `json:"relaySocket,omitempty"`
	RelayURL    string `json:"relayUrl,omitempty"`
	// Proxy is the session's sessionproxy when its MCP servers are
	// reached through one: the socket the agent dials and the servers it
	// names, so the node that adopts the session starts a proxy on the
	// same socket with each upstream rendered again from the item's tools
	// and the session's bearer. A path and names only — never an
	// upstream's URL, header, env or bearer.
	Proxy *Proxy `json:"proxy,omitempty"`
	// LoginFile is the auth.json a sandboxed Codex session's login was
	// copied from, and LoginCache where a login it refreshed is kept for
	// the next session on its credential: the node that ends the session
	// writes a refreshed one back to both. Paths only, never the login.
	LoginFile  string `json:"loginFile,omitempty"`
	LoginCache string `json:"loginCache,omitempty"`
	// SessionID is the agent's own session id once it said it.
	SessionID string `json:"sessionId,omitempty"`
	// Offset is the byte offset in output.jsonl up to which the node has
	// read and reported, always at the end of a line.
	Offset int64 `json:"offset"`
}

// Proxy is a session's sessionproxy as its state names it.
type Proxy struct {
	// Socket is the unix socket the session's MCP entries dial; the
	// directory around it is the session's own (and a pod's mount).
	Socket string `json:"socket"`
	// Servers are the server names the session reaches through it,
	// sorted.
	Servers []string `json:"servers"`
}

// WriteState writes st to dir/state.json atomically: a temporary file
// in dir, then a rename over the old one.
func WriteState(dir string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, StateFile))
}

// ReadState reads dir/state.json.
func ReadState(dir string) (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}
