package node

import (
	"context"
	"errors"
	"sort"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The platform's stop list (runsheet goal
// a-session-stops-when-its-item-finishes-elsewhere; ADR 066, decision
// 8): every heartbeat reports the build, evaluate and rebase sessions
// the node runs under an item's claim, and the answer names the ones to
// stop — their item merged, was cancelled or superseded elsewhere, or
// the node's connection to its org was revoked. A stopped session's ctx
// is cancelled with a *StoppedError, which Is harness.ErrStopSession, so
// its unit is stopped and killed rather than detached for adoption, and
// the session ends as a lost claim does: nothing is sent for the item,
// which is no longer the node's.

// StoppedError is the cause of a session's ctx the platform stopped,
// with the platform's reason.
type StoppedError struct{ Reason string }

func (e *StoppedError) Error() string { return "stopped by the platform: " + e.Reason }

// Unwrap makes it harness.ErrStopSession.
func (e *StoppedError) Unwrap() error { return harness.ErrStopSession }

// StoppedByPlatform is the platform's reason when ctx was cancelled by
// its stop list, and whether it was.
func StoppedByPlatform(ctx context.Context) (string, bool) {
	if ctx.Err() == nil {
		return "", false
	}
	var se *StoppedError
	if errors.As(context.Cause(ctx), &se) {
		return se.Reason, true
	}
	return "", false
}

// stoppable is one session the stop list can end.
type stoppable struct {
	cancel context.CancelCauseFunc
	since  time.Time
}

func stopKey(kind, itemID string) string { return kind + "/" + itemID }

// stoppableCtx derives a session's ctx the stop list can cancel, and
// registers it under kind and the item until done is called, which the
// session's goroutine defers.
func (l *Loop) stoppableCtx(ctx context.Context, kind, itemID string) (context.Context, func()) {
	sctx, cancel := context.WithCancelCause(ctx)
	key := stopKey(kind, itemID)
	l.mu.Lock()
	if l.stoppables == nil {
		l.stoppables = map[string]stoppable{}
	}
	l.stoppables[key] = stoppable{cancel: cancel, since: l.now()}
	l.mu.Unlock()
	return sctx, func() {
		l.mu.Lock()
		delete(l.stoppables, key)
		l.mu.Unlock()
		cancel(nil)
	}
}

// Sessions are the sessions the stop list can end, as the heartbeat
// reports them, sorted by item then kind; empty, never nil, so a node
// running none says so and the platform's record clears.
func (l *Loop) Sessions() []gateway.NodeSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []gateway.NodeSession{}
	for key, s := range l.stoppables {
		for i := 0; i < len(key); i++ {
			if key[i] == '/' {
				out = append(out, gateway.NodeSession{Kind: key[:i], ItemID: key[i+1:], Since: s.since.UTC().Format(time.RFC3339Nano)})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ItemID != out[j].ItemID {
			return out[i].ItemID < out[j].ItemID
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// applyStops ends each session the answer names that the node still
// runs, saying so once per session; one it no longer runs is nothing.
func (l *Loop) applyStops(stops []gateway.SessionStop) {
	for _, s := range stops {
		key := stopKey(s.Kind, s.ItemID)
		l.mu.Lock()
		st, ok := l.stoppables[key]
		if ok {
			delete(l.stoppables, key)
		}
		l.mu.Unlock()
		if !ok {
			continue
		}
		l.logf("item %s: %s session stopped by the platform (%s)", s.ItemID, s.Kind, s.Reason)
		st.cancel(&StoppedError{Reason: s.Reason})
	}
}
