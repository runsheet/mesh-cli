package node

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// One mechanism stops any running job: a job registered under its
// subject and its Mesh job stops by either key, once, saying so once
// with the reason; the heartbeat never reports it as a session; and a
// key no job runs under is nothing.
func TestAJobStopsByAnyOfItsKeysOnce(t *testing.T) {
	var logs strings.Builder
	l := &Loop{Log: log.New(&logs, "", 0)}
	ctx, done := l.runningJob(context.Background(), "pr acme/widgets#12@33333333", false, validationStopKey("v-1"), JobKey("job-1"))
	defer done()
	if got := l.Sessions(); len(got) != 0 {
		t.Fatalf("a validation reported as a session: %+v", got)
	}
	if !l.StopJob(JobKey("job-1"), "job cancelled: superseded") {
		t.Fatal("the job did not stop by its Mesh job")
	}
	if !errors.Is(context.Cause(ctx), harness.ErrStopSession) {
		t.Fatalf("cause %v is not ErrStopSession", context.Cause(ctx))
	}
	if l.StopJob(validationStopKey("v-1"), "again") || l.StopJob("validate/none", "nothing") {
		t.Fatal("a stopped job, or none, stopped again")
	}
	if n := strings.Count(logs.String(), "stopped:"); n != 1 || !strings.Contains(logs.String(), "pr acme/widgets#12@33333333: stopped: job cancelled: superseded") {
		t.Fatalf("log:\n%s", logs.String())
	}
}

// A pull request validation handed over by the one claim, whose head is
// superseded mid-step (its validation withdrawn), is stopped within the
// claim check: every step in every pod cancelled, every pod — the run's
// own and each shard's, sidecars with them — removed, nothing finished
// or failed, and node.log says so once. Before, the queued run had no
// check at all and went on to its end (dl360, 2026-10-28).
func TestAWithdrawnQueuedValidationStopsAndRemovesItsPods(t *testing.T) {
	r := newKindsRig(t)
	every := 20 * time.Millisecond
	r.loop.BatchCheckEvery = every
	rt := &sandbox.Fake{Agent: func(ctx context.Context, _ sandbox.AgentSpec) (sandbox.Exit, error) {
		<-ctx.Done() // a step that would run for half an hour
		return sandbox.Exit{Code: -1}, nil
	}}
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	sb.Config.ServiceTimeout = 2 * time.Second
	running := make(chan struct{})
	r.loop.ValidatePullRequest = func(ctx context.Context, p gateway.MainValidationPullRequest) (validate.Attestation, error) {
		// as validateCheckout: the run's pod with the checkout's
		// services, a lane per shard, every pod closed on the way out
		dir := t.TempDir()
		writeServices(t, dir, servicesYML)
		logf := func(string, ...any) {}
		first, err := OpenSessionPod(ctx, sb, dir, "pr-12", harness.Kind("validate"), sandbox.GitIdentity{}, logf)
		defer first.Close()
		if err != nil {
			return validate.Attestation{}, err
		}
		lr := laneSandboxRunner{sandboxRunner: sandboxRunner{sess: first.Session, dir: dir},
			open: func(ctx context.Context, name string) (*SessionPod, error) {
				return OpenSessionPod(ctx, sb, dir, "pr-12-"+name, harness.Kind("validate"), sandbox.GitIdentity{}, logf)
			}}
		steps := []validate.Step{
			{Name: "unit", Kind: validate.KindUnit, Command: "go test ./...", Timeout: time.Hour},
			{Name: "integration", Kind: validate.KindIntegration, Command: "go test ./e2e", Timeout: time.Hour, Shards: 3},
		}
		close(running)
		_, err = validate.Run(ctx, steps, lr, validate.Options{Dir: dir})
		return validate.Attestation{}, err
	}
	r.f.SeedPullRequestValidation(gateway.MainValidationPullRequest{PullRequestValidationID: prValidationID, Number: 12, OrgID: orgID,
		RepoID: repoID, RepoFullName: "acme/widgets", DefaultBranch: "main", HeadBranch: "someones-fix", Ref: "refs/pull/12/head",
		SHA: "3333333333333333333333333333333333333333", RequestedAt: time.Now(), BudgetSeconds: 1800})
	r.f.Queue = []gatewaytest.QueuedJob{{Kind: "runsheet/validate", SubjectType: "pull_request", SubjectID: prValidationID}}
	r.loop.Tick(context.Background())
	<-running
	// every step running in its pod: the run's own and three shards'
	deadline := time.Now().Add(5 * time.Second)
	for len(rt.PodSpecs()) < 4 || len(rt.AgentSpecs()) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("pods %d, steps %d, want 4 and 4", len(rt.PodSpecs()), len(rt.AgentSpecs()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	withdrawn := time.Now()
	r.f.WithdrawPullRequestValidation(prValidationID)
	ended := make(chan struct{})
	go func() { r.loop.wg.Wait(); close(ended) }()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatalf("the withdrawn run still runs\n%s", r.logs.String())
	}
	if took := time.Since(withdrawn); took > 50*every {
		t.Fatalf("stopped %s after the withdrawal, want within the check (%s)", took, every)
	}
	for _, p := range rt.PodSpecs() {
		if _, ok := rt.Called("remove " + p.Name); !ok {
			t.Fatalf("pod %s (%s) was left", p.Name, p.Labels[sandbox.SessionLabel])
		}
	}
	logs := r.logs.String()
	if n := strings.Count(logs, "stopped: claim lost"); n != 1 {
		t.Fatalf("%d stop lines, want one:\n%s", n, logs)
	}
	for _, name := range []string{"finishJob", "failPullRequestValidation"} {
		if n := len(r.f.CallsNamed(name)); n != 0 {
			t.Fatalf("%s called %d times for a withdrawn validation", name, n)
		}
	}
	if r.loop.Validating() != 0 {
		t.Fatalf("Validating() = %d after the stop", r.loop.Validating())
	}
}
