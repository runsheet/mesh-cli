package node

import (
	"context"
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

// pushRulesKey carries the refs a job's push may touch (WithPushRules).
type pushRulesKey struct{}

// WithPushRules marks ctx's git commands as a job's whose brief grants
// rules: a write token is then relayed too, its pushes held to them.
func WithPushRules(ctx context.Context, rules []gateway.JobPushRef) context.Context {
	if len(rules) == 0 {
		return ctx
	}
	return context.WithValue(ctx, pushRulesKey{}, rules)
}

// relayFor registers tok's repository on the node's relay and answers
// the base URL a command's insteadOf points GitHub at, or "" when the
// relay is off or the token names no repository. A read token is
// relayed read-only. A write token is relayed only when ctx carries the
// claim's push rules (WithPushRules) — which refs a push may touch is the
// product's to say — and is held to exactly those; without them its
// commands carry its helper as before.
func relayFor(ctx context.Context, tok *gateway.GitHubToken, logf func(string, ...any)) string {
	gitRelayState.Lock()
	srv := gitRelayState.srv
	gitRelayState.Unlock()
	if srv == nil || tok == nil || tok.Repository == "" {
		return ""
	}
	p := gitrelay.Policy{Repo: tok.Repository, Access: gitrelay.Read}
	if !strings.EqualFold(tok.Access, "read") {
		rules, _ := ctx.Value(pushRulesKey{}).([]gateway.JobPushRef)
		if len(rules) == 0 {
			return ""
		}
		p.Access = gitrelay.Write
		for _, r := range rules {
			p.Push = append(p.Push, gitrelay.RefRule{Ref: r.Ref, CreateOnly: r.CreateOnly})
		}
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

// GitRelayCounts is every request the node's git relay has answered since
// the node started, by repository, service and outcome; nil when the
// relay is off or keeps no counts.
func GitRelayCounts() []gitrelay.Count {
	gitRelayState.Lock()
	srv := gitRelayState.srv
	gitRelayState.Unlock()
	if srv == nil || srv.Counts == nil {
		return nil
	}
	return srv.Counts.Total()
}
