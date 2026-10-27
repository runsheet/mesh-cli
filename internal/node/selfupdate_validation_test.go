package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/validate"
)

// A validation runs in the node's own process, so unlike a session it
// does not outlive a self-update's restart: batch #262 died
// mid-materialise ("context canceled") when dl360 updated at 20:21:58Z
// on 2026-10-27, and was abandoned. A verified update now waits for the
// running validations before it swaps, claiming no new one meanwhile,
// up to updateValidationWait.

// validatingUpdateLoop is updateLoop with a merge batch candidate whose
// validation runs until finish is closed, started by one tick.
func validatingUpdateLoop(t *testing.T) (*Loop, *fakeUpdater, chan struct{}, func() string) {
	t.Helper()
	p := newPriorityFake(t)
	loop, u, release, logs := p.updateLoop("")
	t.Cleanup(func() { close(release) })
	p.SeedMergeBatch(gateway.MainValidationBatch{MergeBatchID: batchID, Number: 262, CandidateBranch: "mesh/batch/acme/widgets/262",
		SHA: "2222222222222222222222222222222222222222", Attempt: 1, RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID,
		DefaultBranch: "main", RequestedAt: time.Now()})
	finish := make(chan struct{})
	loop.ValidateBatch = func(ctx context.Context, b gateway.MainValidationBatch) (validate.Attestation, error) {
		select {
		case <-finish:
		case <-ctx.Done():
			return validate.Attestation{}, ctx.Err()
		}
		return validate.Attestation{Commit: b.SHA, Steps: []validate.StepResult{{Name: "unit"}}}, nil
	}
	loop.Tick(loop.Context(context.Background()))
	if loop.Validating() != 1 {
		t.Fatalf("validating = %d, want the batch running\n%s", loop.Validating(), logs.String())
	}
	p.Nodes[p.scopes[0].NodeID]["desiredVersion"] = "v1.1.0"
	return loop, u, finish, logs.String
}

func swapped(u *fakeUpdater) bool {
	for _, e := range u.Events() {
		if strings.HasPrefix(e, "swap") {
			return true
		}
	}
	return false
}

// The swap waits for the running validation; once it finishes, the
// update swaps and Run returns ErrRestartForUpdate.
func TestSelfUpdateWaitsForRunningValidations(t *testing.T) {
	loop, u, finish, logs := validatingUpdateLoop(t)
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background()) }()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Run = %v with a validation still running, want it to wait", err)
	default:
	}
	if swapped(u) {
		t.Fatalf("swapped with a validation running: %q", u.Events())
	}
	if !loop.holdingValidations() {
		t.Fatal("new validations are not held while the update waits")
	}
	close(finish)
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("Run = %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not restart once the validation finished")
	}
	if !swapped(u) {
		t.Fatalf("no swap after the validation finished: %q", u.Events())
	}
	out := logs()
	for _, want := range []string{"waiting up to", "1 running validation(s)", "the running validations have finished"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q:\n%s", want, out)
		}
	}
}

// Past updateValidationWait the update swaps anyway and says so.
func TestSelfUpdateSwapsPastTheValidationWait(t *testing.T) {
	defer func(d time.Duration) { updateValidationWait = d }(updateValidationWait)
	updateValidationWait = 200 * time.Millisecond
	loop, u, finish, logs := validatingUpdateLoop(t)
	defer close(finish)
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("Run = %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run waited past updateValidationWait")
	}
	if !swapped(u) {
		t.Fatalf("no swap: %q", u.Events())
	}
	if out := logs(); !strings.Contains(out, "still running after") {
		t.Fatalf("log does not say it swapped anyway:\n%s", out)
	}
}

// With no validation running the update swaps at once, as before.
func TestSelfUpdateWithNoValidationDoesNotWait(t *testing.T) {
	loop := &Loop{}
	if !loop.waitValidations(context.Background(), "v1.1.0") || loop.holdingValidations() {
		t.Fatal("an update with no validation running waited or held")
	}
}
