package node

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/validate"
)

// The heartbeat reports the node's sessions by kind (trd §6a) and its
// validation capacity (runsheet ADR 079, the chain window within
// capacity): validating is the main validations running, and
// validationSlots the free slots the last pass offered validations —
// none on the first heartbeat, which goes out before any pass judged
// it, and capacity less what runs on the next.
func TestHeartbeatReportsValidatingAndValidationSlots(t *testing.T) {
	p := newPriorityFake(t)
	p.register()
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 6\n")
	loop.ValidateBatch = func(context.Context, gateway.MainValidationBatch) (validate.Attestation, error) {
		return validate.Attestation{}, nil
	}
	loop.mu.Lock()
	loop.validating = map[string]bool{"batch-1": true, "batch-2": true}
	loop.evaluating = map[string]bool{"item-1": true}
	loop.mu.Unlock()

	loop.Tick(context.Background())
	loop.wg.Wait()
	hb := p.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		t.Fatalf("no heartbeat:\n%s", logs)
	}
	f, ok := hb[len(hb)-1].Input["inFlight"].(map[string]any)
	if !ok {
		t.Fatalf("the heartbeat carries no inFlight: %#v", hb[len(hb)-1].Input)
	}
	if f["validating"] != 2.0 || f["evaluating"] != 1.0 || f["building"] != 0.0 {
		t.Fatalf("inFlight %v, want validating 2, evaluating 1, building 0", f)
	}
	if _, ok := f["validationSlots"]; ok {
		t.Fatalf("the first heartbeat, before any pass, carries validationSlots: %v", f)
	}

	before := len(hb)
	loop.Tick(context.Background())
	loop.wg.Wait()
	hb = p.CallsNamed("heartbeatNode")[before:]
	if len(hb) == 0 {
		t.Fatalf("the second tick sent no heartbeat:\n%s", logs)
	}
	f, _ = hb[len(hb)-1].Input["inFlight"].(map[string]any)
	// capacity 6 less the two validations running (Running counts them)
	if f["validationSlots"] != 4.0 {
		t.Fatalf("validationSlots %v, want 4 (capacity 6, 2 validating): %v", f["validationSlots"], f)
	}
}

// A platform whose NodeInFlightInput predates runsheet ADR 079 is sent
// the five kinds alone: validating and validationSlots would have it
// refuse the whole heartbeat.
func TestHeartbeatInFlightWithoutValidatingOnAnOlderPlatform(t *testing.T) {
	p := newPriorityFake(t)
	p.register()
	p.UnknownFields = map[string]bool{"validating": true, "validationSlots": true}
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 6\n")
	loop.mu.Lock()
	loop.validating = map[string]bool{"batch-1": true}
	loop.mu.Unlock()
	loop.Tick(context.Background())
	loop.Tick(context.Background())
	loop.wg.Wait()
	hb := p.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		t.Fatalf("no heartbeat:\n%s", logs)
	}
	f, ok := hb[len(hb)-1].Input["inFlight"].(map[string]any)
	if !ok || len(f) != 5 {
		t.Fatalf("inFlight %#v, want the five kinds alone", hb[len(hb)-1].Input["inFlight"])
	}
}

// validationSlotsFor offers validations what validateBatches would: none
// on a node that validates nothing or waits out its validations for an
// update, else the free slots within node.yaml's evaluate cap.
func TestValidationSlotsFor(t *testing.T) {
	l := &Loop{Cfg: &Config{}}
	if n := l.validationSlotsFor(5); n != 0 {
		t.Fatalf("a node that validates nothing: %d slots, want 0", n)
	}
	l.ValidateMain = func(context.Context, gateway.MainValidationRequest) (validate.Attestation, error) {
		return validate.Attestation{}, nil
	}
	if n := l.validationSlotsFor(5); n != 5 {
		t.Fatalf("free 5: %d slots, want 5", n)
	}
	if n := l.validationSlotsFor(-2); n != 0 {
		t.Fatalf("over capacity: %d slots, want 0", n)
	}
	l.Cfg.Takes = Takes{{Kind: "evaluate", Cap: 2}}
	l.validating = map[string]bool{"b": true}
	if n := l.validationSlotsFor(5); n != 1 {
		t.Fatalf("evaluate cap 2, 1 validating: %d slots, want 1", n)
	}
	l.Cfg.Takes = nil
	l.validationHold = true
	if n := l.validationSlotsFor(5); n != 0 {
		t.Fatalf("waiting out validations for an update: %d slots, want 0", n)
	}
}
