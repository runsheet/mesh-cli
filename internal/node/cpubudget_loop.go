package node

import (
	"runtime"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// setCPUBudget takes node.yaml's capacity.cpu_budget as reloaded: the
// budget changes on the next look, no restart.
func (l *Loop) setCPUBudget(configured float64) {
	if l.CPU == nil {
		return
	}
	want := CPUBudgetFor(configured, runtime.NumCPU())
	if was := l.CPU.Total(); was != want {
		l.CPU.SetTotal(want)
		l.logf("capacity: cpu budget %g → %g cpus", round2(was), round2(want))
	}
}

// cpuHeartbeat is the heartbeat's cpuBudget; nil with no budget.
func (l *Loop) cpuHeartbeat() *gateway.CPUBudget {
	if l.CPU == nil {
		return nil
	}
	r := l.CPU.Read()
	return &gateway.CPUBudget{Budget: round2(r.Budget), Reserved: round2(r.Reserved), Pods: r.Pods, Committed: round2(r.Committed), Jobs: r.Jobs, Waiting: r.Waiting}
}

// claimEstimate is what the next one claim may start, in CPUs: with a
// validation waiting the largest waiting one's estimate (the queue
// ranks validations first), else one session's pod.
func (l *Loop) claimEstimate(q *gateway.MainValidationQueue) float64 {
	est := l.CPU.podCPUs()
	if q == nil {
		return est
	}
	repos := make([]string, 0, len(q.Batches)+len(q.Requests)+len(q.PullRequests))
	for _, b := range q.Batches {
		repos = append(repos, b.RepoFullName)
	}
	for _, r := range q.Requests {
		repos = append(repos, r.RepoFullName)
	}
	for _, p := range q.PullRequests {
		repos = append(repos, p.RepoFullName)
	}
	for _, repo := range repos {
		est = max(est, l.CPU.ValidationEstimate(repo))
	}
	return est
}

// podCPUs is one session pod's CPUs; 0 with no budget.
func (b *CPUBudget) podCPUs() float64 {
	if b == nil {
		return 0
	}
	return b.Pod
}

// cpuFits says whether a claim of what, estimated at est CPUs, fits the
// budget; when it does not it says so once until it fits again.
func (l *Loop) cpuFits(est float64, what string) bool {
	if l.CPU.Fits(est) {
		l.mu.Lock()
		l.cpuFullSaid = ""
		l.mu.Unlock()
		return true
	}
	line := "capacity: cpu budget full for " + what + ": " + l.CPU.Read().Line() + "; claiming none until pods finish"
	l.mu.Lock()
	said := l.cpuFullSaid == what
	l.cpuFullSaid = what
	l.mu.Unlock()
	if !said {
		l.logf("%s (needs %g cpus)", line, round2(est))
	}
	return false
}

// PodCPUs is one session pod's CPUs in sb at the checkout dir: the
// agent's cpus, and the services' services_cpus when dir declares any
// (sandbox.Resolve). 0 with no sandbox.
func PodCPUs(sb *sandbox.Sandbox, dir string) float64 {
	if sb == nil {
		return 0
	}
	n := sb.Config.Limits.CPUs
	if svcs, err := sandbox.Resolve(dir, sb.Config.Registries); err == nil && len(svcs.Services) > 0 {
		n += sb.Config.ServicesLimits.CPUs
	}
	return n
}

// estimate sets the run's hold to what the selected steps run — every
// lane past the first a pod of its own beside the pods the run holds
// already — and learns the repo's whole estimate, every lane a pod, for
// its next claim. A run with no lanes (no sandbox) holds none past its
// pod.
func (vr validator) estimate(t validationTarget, v *buildValidation, steps []validate.Step) {
	if v.cpu == nil || v.pod <= 0 {
		return
	}
	if _, laned := v.runner.(validate.LaneRunner); !laned {
		return
	}
	n := validate.Lanes(steps)
	if n == 0 {
		return
	}
	b := v.cpu.b
	b.Learn(v.cpu.Repo(), float64(n)*v.pod)
	v.cpu.Estimate(v.cpu.Reserved() + float64(n-1)*v.pod)
	vr.logf("%s: validation: %d lane(s) of %g cpus each; cpu budget %s", t.prefix, n, round2(v.pod), b.Read().Line())
}
