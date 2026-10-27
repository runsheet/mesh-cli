package node

import (
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gitrelay"
)

// The node's git relay, on when node.yaml says git_relay: a git command
// the node runs on a read token goes through it — GitHub's URL rewritten by insteadOf to a loopback path
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
// relay is off, the token names no repository, or the token may write:
// which refs a push may touch is the product's to say, in the claim, and
// until a claim says so a write token's commands carry its helper as
// before. A read token is relayed read-only.
func relayFor(tok *gateway.GitHubToken, logf func(string, ...any)) string {
	gitRelayState.Lock()
	srv := gitRelayState.srv
	gitRelayState.Unlock()
	if srv == nil || tok == nil || tok.Repository == "" || !strings.EqualFold(tok.Access, "read") {
		return ""
	}
	p := gitrelay.Policy{Repo: tok.Repository, Access: gitrelay.Read}
	base, err := srv.Register(p, gitrelay.Token{Value: tok.Token, ExpiresAt: tok.ExpiresAt})
	if err != nil {
		if logf != nil {
			logf("git relay: %s stays on its token's helper: %v", tok.Repository, err)
		}
		return ""
	}
	return base
}
