package node

import (
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gitrelay"
)

// The node's git relay (runsheet ADR 076), on when node.yaml says
// git_relay: every git command the node runs on an item's token goes
// through it — GitHub's URL rewritten by insteadOf to a loopback path
// only that command is told — and the relay adds the token, so it is in
// no git command's environment, config or children. Off, commands carry
// the token's helper by the environment as before (withGitToken).
var gitRelayState struct {
	sync.Mutex
	srv *gitrelay.Server
}

// gitRelayLogf takes the relay's registration failures; nil is silent.
var gitRelayLogf func(string, ...any)

// SetGitRelay makes srv the node's git relay, nil turning it off.
func SetGitRelay(srv *gitrelay.Server, logf func(string, ...any)) {
	gitRelayState.Lock()
	defer gitRelayState.Unlock()
	gitRelayState.srv = srv
	gitRelayLogf = logf
}

// relayFor registers tok's repository on the node's relay and answers
// the base URL a command's insteadOf points GitHub at, or "" when the
// relay is off or the token names no repository (the command then
// carries the token's helper, as before). A write token may push the
// node's own branches (refs/heads/mesh/*) and pin changesets; a read
// token only fetches.
func relayFor(tok *gateway.GitHubToken, logf func(string, ...any)) string {
	gitRelayState.Lock()
	srv := gitRelayState.srv
	gitRelayState.Unlock()
	if srv == nil || tok == nil || tok.Repository == "" {
		return ""
	}
	// a token the platform calls read only fetches; any other (write,
	// or an older platform's that says neither) may push the node's
	// branches, GitHub's own check on the token still standing
	p := gitrelay.Policy{Repo: tok.Repository, Access: gitrelay.Write, PushRef: "refs/heads/mesh/*"}
	if strings.EqualFold(tok.Access, "read") {
		p = gitrelay.Policy{Repo: tok.Repository, Access: gitrelay.Read}
	}
	base, err := srv.Register(p, gitrelay.Token{Value: tok.Token, ExpiresAt: tok.ExpiresAt})
	if err != nil {
		if logf != nil {
			logf("git relay: %s stays on its token's helper: %v", tok.Repository, err)
		}
		return ""
	}
	return base
}

// gitRelayOn says whether the node's git relay is on.
func gitRelayOn() bool {
	gitRelayState.Lock()
	defer gitRelayState.Unlock()
	return gitRelayState.srv != nil
}
