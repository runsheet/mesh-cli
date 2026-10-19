package node

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// EvaluationStallWindow is how long the platform lets an evaluating
// item's heartbeat — the finish into evaluating, then the evaluator's
// ReportWorkItemProgress — go quiet before it stalls the item (runsheet
// ADR 018's amendment 2026-10-18). An evaluation's heartbeat interval
// is always below it.
const EvaluationStallWindow = 15 * time.Minute

// DefaultEvaluationHeartbeatEvery is the evaluation heartbeat interval
// when node.yaml's evaluate.heartbeat_every names none: a third of the
// stall window, so two beats can be lost before a stall.
const DefaultEvaluationHeartbeatEvery = 5 * time.Minute

// The phases an evaluation heartbeat's note names.
const (
	evalPhaseCheckout      = "checkout"
	evalPhaseSession       = "session"
	evalPhaseSecondOpinion = "second opinion"
)

// evalHeartbeat is an evaluation's heartbeat: from the moment the node
// holds the evaluation until its verdict, no-verdict or release is sent,
// a ticker independent of the session's output sends
// ReportWorkItemProgress on the item every interval, its note naming the
// phase and its cents 0. A heartbeat the platform refuses as not held
// (lostRefusal: the claim lost, the item no longer evaluating) is logged
// with the gateway's answer and gives the evaluation up as a lost claim:
// its ctx cancelled with ErrClaimLost, which stops its session.
type evalHeartbeat struct {
	gw     Gateway
	itemID string
	label  string
	every  time.Duration
	logf   func(string, ...any)
	watch  *claimWatch

	mu    sync.Mutex
	phase string

	once   sync.Once
	cancel context.CancelFunc
	done   chan struct{}
}

type evalHeartbeatKey struct{}

// heartbeatEvery is the evaluation heartbeat interval in force:
// e.HeartbeatEvery, else node.yaml's, else the default.
func (e *Evaluator) heartbeatEvery() time.Duration {
	if e.HeartbeatEvery > 0 {
		return e.HeartbeatEvery
	}
	return e.Cfg.EvaluationHeartbeatEvery()
}

// startHeartbeat starts the evaluation's heartbeat on itemID and returns
// the ctx the evaluation runs on — cancelled with ErrClaimLost when a
// heartbeat is refused as not held — carrying the heartbeat, so every
// path that sends the verdict stops it first (stopHeartbeat). The caller
// defers stop for every other way out. An evaluation already carrying
// one (an adopted session's tail inside Evaluate's) keeps it.
func (e *Evaluator) startHeartbeat(ctx context.Context, itemID, label, phase string) (context.Context, *evalHeartbeat) {
	if hb := heartbeatOf(ctx); hb != nil {
		hb.setPhase(phase)
		return ctx, hb
	}
	sctx, watch := newClaimWatch(ctx)
	// the beats' own ctx: stop cancels one in flight, so none lands
	// after the verdict
	bctx, cancel := context.WithCancel(sctx)
	hb := &evalHeartbeat{gw: e.GW, itemID: itemID, label: label, every: e.heartbeatEvery(), logf: e.logf, watch: watch, phase: phase, cancel: cancel, done: make(chan struct{})}
	go hb.run(bctx)
	return context.WithValue(sctx, evalHeartbeatKey{}, hb), hb
}

// heartbeatOf is the evaluation heartbeat ctx carries, nil when none.
func heartbeatOf(ctx context.Context) *evalHeartbeat {
	hb, _ := ctx.Value(evalHeartbeatKey{}).(*evalHeartbeat)
	return hb
}

// stopHeartbeat stops ctx's evaluation heartbeat, if it carries one,
// before the verdict, no-verdict or release goes: no beat after it.
func stopHeartbeat(ctx context.Context) {
	heartbeatOf(ctx).stop()
}

// setEvalPhase names the phase ctx's evaluation is in, for the next
// heartbeat's note.
func setEvalPhase(ctx context.Context, phase string) {
	heartbeatOf(ctx).setPhase(phase)
}

func (hb *evalHeartbeat) setPhase(phase string) {
	if hb == nil {
		return
	}
	hb.mu.Lock()
	hb.phase = phase
	hb.mu.Unlock()
}

func (hb *evalHeartbeat) note() string {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	return "evaluation heartbeat: " + hb.phase
}

func (hb *evalHeartbeat) run(ctx context.Context) {
	defer close(hb.done)
	t := time.NewTicker(hb.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := hb.gw.ReportWorkItemProgress(ctx, hb.itemID, hb.note(), 0, "")
		if ctx.Err() != nil {
			// stopped while it was in flight: nothing to say
			return
		}
		if why := lostRefusal(err); why != "" {
			hb.logf("item %s: evaluation heartbeat refused: %v; claim lost, evaluation stopped", hb.label, err)
			hb.watch.lose(why)
			return
		}
		if err != nil {
			hb.logf("item %s: evaluation heartbeat: %v; trying again in %s", hb.label, err, hb.every)
		}
	}
}

// stop ends the heartbeat and waits for it: once it returns no beat is
// in flight and none is sent again. Safe on nil and more than once.
func (hb *evalHeartbeat) stop() {
	if hb == nil {
		return
	}
	hb.once.Do(func() {
		hb.cancel()
		<-hb.done
	})
}

// lost is why the evaluation's claim was lost, "" while it is held.
func (hb *evalHeartbeat) lost() string {
	if hb == nil {
		return ""
	}
	return hb.watch.lost()
}

// end stops the heartbeat and releases the evaluation's ctx: deferred
// by whoever started it, after every other way out has run.
func (hb *evalHeartbeat) end() {
	if hb == nil {
		return
	}
	hb.stop()
	hb.watch.cancel(nil)
}

// claimLost is err, or, when the evaluation's heartbeat was refused as
// not held, the lost claim in its place: logged `item <label>: claim
// lost (<why>); evaluation stopped`, nothing sent for it.
func (e *Evaluator) claimLost(hb *evalHeartbeat, label string, err error) error {
	why := hb.lost()
	if why == "" {
		return err
	}
	e.logf("item %s: claim lost (%s); evaluation stopped", label, why)
	return fmt.Errorf("evaluation stopped: %w (%s)", ErrClaimLost, why)
}
