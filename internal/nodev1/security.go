package nodev1

import (
	"context"
	"errors"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/secreport"
)

// What a lease's relay saw reaches Tripline through Mesh
// (projectmesh-io/api ADR 001 amendment, security events; mesh-specs gap
// 23). Each lease has a reporter (internal/secreport) from its take to
// its release, which posts to POST /v1/leases/{id}/security on the
// node's machine token, batched every few seconds and flushed once the
// lease has released:
//
//   - egress: each request of the lease's tunnel (internal/egress),
//     allowed with the bytes it carried or refused, by host and rule —
//     the in-process relay's tunnel and, with --relay-sidecar, the
//     node's side of the sidecar's tunnel alike — and an egress request
//     a relay refused for want of any egress (sessionproxy's
//     EgressRefused), which a sidecar passes to the node over the
//     lease's socket (relaysidecar.SecurityPath);
//   - sandbox: a lease's agent or suite step killed by its pod's memory
//     or pids limit.
//
// Every report is content-free (secreport.Check): never a URL, path,
// header, body or token. A sidecar holds no machine token: only the node
// posts.

// securityReportTimeout bounds a lease's last flush of its reports.
const securityReportTimeout = 30 * time.Second

// openSecurity starts lease id's reporter and keeps it in the lease's
// record.
func (r *Runtime) openSecurity(id string) *secreport.Reporter {
	rp := &secreport.Reporter{Lease: id, Mesh: r.API, Every: r.SecurityEvery, Logf: r.logf}
	rp.Start()
	r.mu.Lock()
	if l := r.lease(id); l != nil {
		l.security = rp
	}
	r.mu.Unlock()
	return rp
}

// closeSecurity flushes the lease's reports, once it has released.
func (r *Runtime) closeSecurity(rp *secreport.Reporter) {
	if rp == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), securityReportTimeout)
	defer cancel()
	rp.Close(ctx)
}

// reporter is the lease's reporter, nil when the node runs no such
// lease (and a nil reporter drops what it is given). A relay's hooks
// take it once, when they are made.
func (r *Runtime) reporter(id string) *secreport.Reporter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.lease(id); l != nil {
		return l.security
	}
	return nil
}

// egressReporter reports each request of lease id's relay: a refusal
// from its tunnel's Audit or the relay's own (EgressRefused), an allowed
// tunnel from its Done once it has closed.
func (r *Runtime) egressReporter(id string) func(egress.Call) {
	rp := r.reporter(id)
	return func(c egress.Call) {
		if rep, ok := secreport.Egress(c, time.Now()); ok {
			rp.Add(rep)
		}
	}
}

// securityReporter takes the reports a lease's relay sidecar passes to
// the node; secreport.Check drops one that is not content-free.
func (r *Runtime) securityReporter(id string) func(meshapi.SecurityReport) {
	return r.reporter(id).Add
}

// sandboxReporter reports an error that is a sandbox's limit killing
// what ran in lease id's pod: its memory or pids limit, not a timeout.
func (r *Runtime) sandboxReporter(id string) func(error) {
	rp := r.reporter(id)
	return func(err error) { reportSandbox(rp, err) }
}

func reportSandbox(rp *secreport.Reporter, err error) {
	var le *sandbox.LimitError
	if errors.As(err, &le) && (le.Limit == "memory" || le.Limit == "pids") {
		rp.Add(secreport.Sandbox("limit:"+le.Limit, time.Now()))
	}
}

// tunnelAudit and tunnelDone are a lease's tunnel's hooks: each request
// logged, and reported by report.
func (r *Runtime) tunnelAudit(id, what string, report func(egress.Call)) func(egress.Call) {
	return func(c egress.Call) {
		if c.Allowed {
			r.logf("nodev1: lease %s: %s to %s:%s", id, what, c.Host, c.Port)
			return
		}
		r.logf("nodev1: lease %s: %s to %s:%s refused: %s", id, what, c.Host, c.Port, c.Refused)
		report(c)
	}
}
