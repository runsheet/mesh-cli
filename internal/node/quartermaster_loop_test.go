package node

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// quartermasterLoop is a priorityFake loop at t0 whose Quartermaster
// runs one fake check, every ten minutes, and writes its findings to a
// RunState of its own; the check answers what *answer holds.
func quartermasterLoop(t *testing.T, p *priorityFake, answer *[]Finding) (*Loop, *strings.Builder, *clock, RunState, *atomic.Int32) {
	t.Helper()
	p.register()
	loop, logs := p.loop(1)
	c := &clock{now: t0}
	loop.Now = c.Now
	runs := &atomic.Int32{}
	check := func(_ context.Context, now time.Time) []Finding {
		runs.Add(1)
		out := append([]Finding(nil), (*answer)...)
		for i := range out {
			out[i].At = now
		}
		return out
	}
	loop.Quartermaster = &Quartermaster{Checks: []Check{check}, Every: 10 * time.Minute}
	state := RunState{Dir: t.TempDir()}
	loop.Findings = func(f []Finding) {
		if err := state.WriteFindings(f); err != nil {
			t.Error(err)
		}
	}
	return loop, logs, c, state, runs
}

// A Loop with a Quartermaster writes node.findings on Tick, and a tick
// inside Every runs no check again; one after it does.
func TestLoopQuartermasterWritesFindingsAndRunsOnItsCadence(t *testing.T) {
	p := newPriorityFake(t)
	answer := []Finding{{Kind: "disk", Severity: "ok", Subject: "/", Detail: "60% free"}}
	loop, _, c, state, runs := quartermasterLoop(t, p, &answer)
	ctx := context.Background()

	loop.Tick(ctx)
	loop.wg.Wait()
	got, ok := state.Findings()
	if !ok || len(got) != 1 || got[0].Kind != "disk" || got[0].Detail != "60% free" || !got[0].At.Equal(t0) {
		t.Fatalf("node.findings after the first tick = %+v, %v", got, ok)
	}
	if runs.Load() != 1 {
		t.Fatalf("checks ran %d times on the first tick, want 1", runs.Load())
	}

	answer[0].Detail = "59% free"
	c.Set(t0.Add(5 * time.Minute))
	loop.Tick(ctx)
	loop.wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("checks ran again inside Every: %d runs", runs.Load())
	}
	if got, _ := state.Findings(); got[0].Detail != "60% free" {
		t.Fatalf("node.findings rewritten inside Every: %+v", got)
	}

	c.Set(t0.Add(10 * time.Minute))
	loop.Tick(ctx)
	loop.wg.Wait()
	if runs.Load() != 2 {
		t.Fatalf("checks ran %d times after Every, want 2", runs.Load())
	}
	if got, _ := state.Findings(); got[0].Detail != "59% free" || !got[0].At.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("node.findings after Every = %+v", got)
	}
}

// A non-ok finding logs its quartermaster line once while it is
// unchanged, again when its detail changes, and a reclaim is logged on
// the run that reclaimed.
func TestLoopQuartermasterLogsANonOkFindingOncePerChange(t *testing.T) {
	p := newPriorityFake(t)
	answer := []Finding{
		{Kind: "disk", Severity: "critical", Subject: "/home", Detail: "3% free"},
		{Kind: "disk", Severity: "ok", Subject: "/tmp", Detail: "70% free"},
		{Kind: "scratch", Severity: "ok", Subject: "/tmp", Detail: "reaped 2 dirs", Reclaimed: 3 << 30},
	}
	loop, logs, c, _, _ := quartermasterLoop(t, p, &answer)
	ctx := context.Background()
	tick := func(at time.Duration) {
		c.Set(t0.Add(at))
		loop.Tick(ctx)
		loop.wg.Wait()
	}
	tick(0)
	answer[2].Reclaimed = 0
	tick(10 * time.Minute)
	tick(20 * time.Minute)
	answer[0].Detail = "2% free"
	tick(30 * time.Minute)

	count := func(line string) int { return strings.Count(logs.String(), line+"\n") }
	if n := count("quartermaster: disk critical /home: 3% free"); n != 1 {
		t.Fatalf("unchanged critical finding logged %d times, want once:\n%s", n, logs)
	}
	if n := count("quartermaster: disk critical /home: 2% free"); n != 1 {
		t.Fatalf("changed finding logged %d times, want once:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "quartermaster: disk ok") {
		t.Fatalf("an ok finding was logged:\n%s", logs)
	}
	if n := count("quartermaster: reclaimed 3.0G (scratch)"); n != 1 {
		t.Fatalf("reclaim logged %d times, want once:\n%s", n, logs)
	}
}

// The latest findings ride every heartbeat on a gateway whose schema
// takes `findings`; on one without it the heartbeat lands with no
// `findings` key and the node says once that it keeps them.
func TestLoopHeartbeatCarriesTheLatestFindings(t *testing.T) {
	for _, takes := range []bool{true, false} {
		p := newPriorityFake(t)
		if !takes {
			p.UnknownFields["findings"] = true
		}
		answer := []Finding{{Kind: "version", Severity: "warn", Subject: "v1", Detail: "behind v2", Bytes: 0}}
		loop, logs, c, _, _ := quartermasterLoop(t, p, &answer)
		ctx := context.Background()
		for i := range 3 {
			c.Set(t0.Add(time.Duration(i) * time.Minute))
			loop.Tick(ctx)
			loop.wg.Wait()
		}
		hb := p.CallsNamed("heartbeatNode")
		// one heartbeat per org a tick; the first tick's go before its
		// checks run
		first := len(p.scopes)
		if len(hb) != 3*first {
			t.Fatalf("takes=%v: heartbeats %d", takes, len(hb))
		}
		for i, call := range hb[first:] {
			v, ok := call.Input["findings"]
			if takes {
				rows, _ := v.([]any)
				if len(rows) != 1 || rows[0].(map[string]any)["kind"] != "version" {
					t.Fatalf("heartbeat %d: findings = %#v", i+1, v)
				}
			} else if ok {
				t.Fatalf("heartbeat %d carries findings the schema lacks: %#v", i+1, v)
			}
		}
		n := strings.Count(logs.String(), "platform has no heartbeat findings; kept on the node\n")
		if takes && n != 0 || !takes && n != 1 {
			t.Fatalf("takes=%v: kept-on-the-node line %d times:\n%s", takes, n, logs)
		}
	}
}
