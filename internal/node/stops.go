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

// stoppable is one running job the node can stop: a session under an
// item's claim, which the heartbeat reports for the stop list, or a job
// of any other kind (a validation), stopped when its claim or job is
// found withdrawn or lost. One stoppable may stand under several keys —
// its kind and subject, its Mesh job — and stopping any of them stops
// it (stopJob).
type stoppable struct {
	cancel context.CancelCauseFunc
	since  time.Time
	// label is its log prefix (a job's); "" for a session, whose stop
	// line is applyStops'
	label string
	// session: reported on the heartbeat (Sessions) for the stop list
	session bool
	// keys are every key it stands under, all dropped when it stops
	keys []string
}

func stopKey(kind, itemID string) string { return kind + "/" + itemID }

// JobKey is a Mesh job's key among the node's running jobs: what a
// job's own heartbeat, cancelled or lost, stops it by (StopJob).
func JobKey(jobID string) string { return stopKey("job", jobID) }

// stoppableCtx derives a session's ctx the stop list can cancel, and
// registers it under kind and the item until done is called, which the
// session's goroutine defers.
func (l *Loop) stoppableCtx(ctx context.Context, kind, itemID string) (context.Context, func()) {
	return l.runningJob(ctx, "", true, stopKey(kind, itemID))
}

// runningJob derives a running job's ctx — any kind's — that stopJob
// can cancel, registered under each of keys until done is called, which
// the job's goroutine defers. session says the heartbeat reports it.
func (l *Loop) runningJob(ctx context.Context, label string, session bool, keys ...string) (context.Context, func()) {
	sctx, cancel := context.WithCancelCause(ctx)
	st := &stoppable{cancel: cancel, since: l.now(), label: label, session: session, keys: keys}
	l.mu.Lock()
	if l.stoppables == nil {
		l.stoppables = map[string]*stoppable{}
	}
	for _, k := range keys {
		l.stoppables[k] = st
	}
	l.mu.Unlock()
	return sctx, func() {
		l.mu.Lock()
		for _, k := range keys {
			if l.stoppables[k] == st {
				delete(l.stoppables, k)
			}
		}
		l.mu.Unlock()
		cancel(nil)
	}
}

// takeStoppable takes the running job key names off the node's running
// jobs, under every key it stands under; nil when none runs.
func (l *Loop) takeStoppable(key string) *stoppable {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.stoppables[key]
	if !ok {
		return nil
	}
	for _, k := range st.keys {
		if l.stoppables[k] == st {
			delete(l.stoppables, k)
		}
	}
	return st
}

// JobLostError is why the node stopped a job of its own: its claim or
// its Mesh job was withdrawn, cancelled or lost. Like a platform's stop
// (StoppedError) it Is harness.ErrStopSession: nothing is sent for the
// job, which is no longer the node's.
type JobLostError struct{ Reason string }

func (e *JobLostError) Error() string { return e.Reason }

// Unwrap makes it harness.ErrStopSession.
func (e *JobLostError) Unwrap() error { return harness.ErrStopSession }

// StopJob stops the running job key names (JobKey, or a kind and
// subject) — its steps cancelled, and with its ctx every pod it opened
// and its workspace removed by its own cleanup — saying so once, with
// reason. It answers whether such a job ran here. The one way any
// running job is stopped: the platform's stop list, a validation's
// claim found lost, a Mesh job found cancelled.
func (l *Loop) StopJob(key, reason string) bool {
	return l.stopJob(key, &JobLostError{Reason: reason})
}

// stopJob is StopJob with the cause the job's ctx is cancelled with.
func (l *Loop) stopJob(key string, cause error) bool {
	st := l.takeStoppable(key)
	if st == nil {
		return false
	}
	if st.label != "" {
		l.logf("%s: stopped: %v; its steps cancelled, its pods and workspace removed", st.label, cause)
	}
	st.cancel(cause)
	return true
}

// Sessions are the sessions the stop list can end, as the heartbeat
// reports them, sorted by item then kind; empty, never nil, so a node
// running none says so and the platform's record clears.
func (l *Loop) Sessions() []gateway.NodeSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []gateway.NodeSession{}
	for key, s := range l.stoppables {
		if !s.session {
			continue
		}
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
		st := l.stoppables[key]
		l.mu.Unlock()
		if st == nil {
			continue
		}
		if st.label == "" {
			l.logf("item %s: %s session stopped by the platform (%s)", s.ItemID, s.Kind, s.Reason)
		}
		l.stopJob(key, &StoppedError{Reason: s.Reason})
	}
}
