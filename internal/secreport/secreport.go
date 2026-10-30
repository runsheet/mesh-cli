// Package secreport is a lease's security reports, on their way to Mesh
// and from there to Tripline (projectmesh-io/api ADR 001 amendment,
// security events; mesh-specs gap 23): what the lease's relay saw —
// egress allowed and refused, by host, bytes and rule, and the sandbox's
// denials — each content-free by construction: a type, a bare hostname
// or IP address, a byte count, a verdict and a rule name, never a URL,
// path, header, body, match or token.
//
// A Reporter is one lease's: Add takes a report, checked as Mesh checks
// it; a report equal to one still pending but for its bytes is folded
// into it; the pending reports go to POST /v1/leases/{id}/security on
// the node's machine token every Every, at most MaxSecurityReports a
// post, and once more, whatever is left, when the lease releases
// (Close). A 422 or any other 4xx is final: the batch is dropped and
// logged. A 5xx or a transport error keeps the batch for the next
// send.
package secreport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// DefaultEvery is how often a reporter sends what is pending.
const DefaultEvery = 5 * time.Second

// MaxPending bounds the reports a reporter holds unsent; past it a new
// report is dropped, counted and logged once per send.
const MaxPending = 1000

// Poster is Mesh's security route (meshapi.Client.Security).
type Poster interface {
	Security(ctx context.Context, leaseID string, reports []meshapi.SecurityReport) (*meshapi.SecurityAccepted, error)
}

var (
	ruleName      = regexp.MustCompile(`^[a-z0-9_.:/-]{1,64}$`)
	hostnameLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Check says why Mesh would refuse r, nil when it would take it: the
// route's own validation, so a report that is not content-free never
// leaves the node.
func Check(r meshapi.SecurityReport) error {
	switch {
	case r.Type != meshapi.ReportEgress && r.Type != meshapi.ReportDLP && r.Type != meshapi.ReportFingerprint && r.Type != meshapi.ReportSandbox:
		return fmt.Errorf("type %q is not egress, dlp, fingerprint or sandbox", r.Type)
	case r.Verdict != meshapi.VerdictAllow && r.Verdict != meshapi.VerdictDeny && r.Verdict != meshapi.VerdictRedact:
		return fmt.Errorf("verdict %q is not allow, deny or redact", r.Verdict)
	case r.Type == meshapi.ReportEgress && r.Host == "":
		return errors.New("an egress report names its host")
	case r.Host != "" && !BareHost(r.Host):
		return errors.New("host is not a bare hostname or IP address")
	case r.Bytes < 0:
		return errors.New("bytes are never negative")
	case r.Rule != "" && !ruleName.MatchString(r.Rule):
		return errors.New("rule is not a name of at most 64 of a-z, 0-9 and _ . : / -")
	}
	return nil
}

// BareHost says h is a bare hostname (lower case, no scheme, port, path
// or user) or an IP address.
func BareHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if !hostnameLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// Host is h as a report names it: lower case, without a trailing dot or
// an IPv6 literal's brackets; "" when it is not a bare host at all.
func Host(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if !BareHost(h) {
		return ""
	}
	return h
}

// Egress is the report of one request of a lease's tunnel: allowed with
// the bytes it carried (egress.Tunnel.Done), or refused (its Audit). A
// call whose host is not a bare host — a garbled CONNECT — is reported
// with no host only when refused for that, never with what was sent.
func Egress(c egress.Call, at time.Time) (meshapi.SecurityReport, bool) {
	host := Host(c.Host)
	if host == "" {
		return meshapi.SecurityReport{}, false
	}
	rep := meshapi.SecurityReport{Type: meshapi.ReportEgress, Host: host, Verdict: meshapi.VerdictDeny, Rule: c.Rule}
	if c.Allowed {
		rep.Verdict, rep.Bytes = meshapi.VerdictAllow, c.Bytes
	}
	if rep.Rule == "" {
		rep.Rule = egress.RuleHosts
	}
	at = at.UTC().Truncate(time.Second)
	rep.At = &at
	return rep, Check(rep) == nil
}

// Sandbox is a sandbox denial the node saw: rule names what denied it
// (limit:memory, limit:pids).
func Sandbox(rule string, at time.Time) meshapi.SecurityReport {
	at = at.UTC().Truncate(time.Second)
	return meshapi.SecurityReport{Type: meshapi.ReportSandbox, Verdict: meshapi.VerdictDeny, Rule: rule, At: &at}
}

// Reporter batches one lease's reports to Mesh.
type Reporter struct {
	// Lease is the lease reported on.
	Lease string
	// Mesh takes the reports.
	Mesh Poster
	// Every is how often pending reports are sent; DefaultEvery when 0.
	Every time.Duration
	// Logf takes the reporter's lines; never a report's host.
	Logf func(format string, args ...any)

	mu      sync.Mutex
	pending []meshapi.SecurityReport
	dropped int
	kick    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	closed  bool
	started bool
}

// Start sends pending reports every Every until Close.
func (rp *Reporter) Start() {
	if rp == nil {
		return
	}
	rp.mu.Lock()
	if rp.started || rp.closed {
		rp.mu.Unlock()
		return
	}
	rp.started = true
	rp.kick, rp.stop, rp.stopped = make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
	rp.mu.Unlock()
	every := rp.Every
	if every <= 0 {
		every = DefaultEvery
	}
	go func() {
		defer close(rp.stopped)
		t := time.NewTicker(every)
		defer t.Stop()
		backoff := time.Duration(0)
		// hold, while set, is a failed send's backoff: nothing is sent
		// before it passes
		var hold <-chan time.Time
		for {
			select {
			case <-rp.stop:
				return
			case <-t.C:
				if hold != nil {
					continue
				}
			case <-rp.kick:
				if hold != nil {
					continue
				}
			case <-hold:
				hold = nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			retry := rp.send(ctx)
			cancel()
			if retry {
				backoff = min(max(2*backoff, every), time.Minute)
				hold = time.After(backoff)
				continue
			}
			backoff = 0
		}
	}()
}

// Add takes one report, dropping one Mesh would refuse. A pending
// report equal to it but for bytes and when takes its bytes instead.
func (rp *Reporter) Add(r meshapi.SecurityReport) {
	if rp == nil {
		return
	}
	if err := Check(r); err != nil {
		rp.logf("secreport: lease %s: a %s report dropped: %v", rp.Lease, r.Type, err)
		return
	}
	rp.mu.Lock()
	if rp.closed {
		rp.mu.Unlock()
		return
	}
	for i := range rp.pending {
		p := &rp.pending[i]
		if p.Type == r.Type && p.Host == r.Host && p.Verdict == r.Verdict && p.Rule == r.Rule {
			p.Bytes += r.Bytes
			rp.mu.Unlock()
			return
		}
	}
	if len(rp.pending) >= MaxPending {
		rp.dropped++
		rp.mu.Unlock()
		return
	}
	rp.pending = append(rp.pending, r)
	full := len(rp.pending) >= meshapi.MaxSecurityReports
	rp.mu.Unlock()
	if full && rp.kick != nil {
		select {
		case rp.kick <- struct{}{}:
		default:
		}
	}
}

// send posts the pending reports, MaxSecurityReports at a time, and
// answers whether a batch is worth sending again: it stays pending.
func (rp *Reporter) send(ctx context.Context) (retry bool) {
	for {
		rp.mu.Lock()
		if rp.dropped > 0 {
			rp.logf("secreport: lease %s: %d reports dropped: more than %d were pending", rp.Lease, rp.dropped, MaxPending)
			rp.dropped = 0
		}
		n := min(len(rp.pending), meshapi.MaxSecurityReports)
		batch := append([]meshapi.SecurityReport(nil), rp.pending[:n]...)
		rp.mu.Unlock()
		if n == 0 {
			return false
		}
		_, err := rp.Mesh.Security(ctx, rp.Lease, batch)
		var me *meshapi.Error
		switch {
		case err == nil:
		case errors.As(err, &me) && me.Status >= 400 && me.Status < 500 && me.Status != http.StatusTooManyRequests:
			rp.logf("secreport: lease %s: Mesh refused %d reports, dropped: %v", rp.Lease, n, err)
		default:
			rp.logf("secreport: lease %s: %d reports not sent, kept to send again: %v", rp.Lease, n, err)
			return true
		}
		rp.mu.Lock()
		rp.pending = rp.pending[n:]
		rp.mu.Unlock()
	}
}

// Close stops the timer and sends what is pending, again on a 5xx or a
// transport error until ctx ends: the lease's release, which Mesh takes
// after the lease has ended. Later reports are dropped.
func (rp *Reporter) Close(ctx context.Context) {
	if rp == nil {
		return
	}
	rp.mu.Lock()
	if rp.closed {
		rp.mu.Unlock()
		return
	}
	rp.closed = true
	started := rp.started
	rp.mu.Unlock()
	if started {
		close(rp.stop)
		<-rp.stopped
	}
	wait := 500 * time.Millisecond
	for rp.send(ctx) {
		select {
		case <-ctx.Done():
			rp.mu.Lock()
			n := len(rp.pending)
			rp.pending = nil
			rp.mu.Unlock()
			rp.logf("secreport: lease %s: %d reports not sent before the release ended", rp.Lease, n)
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, 10*time.Second)
	}
}

// Pending is how many reports wait to be sent.
func (rp *Reporter) Pending() int {
	if rp == nil {
		return 0
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return len(rp.pending)
}

func (rp *Reporter) logf(format string, args ...any) {
	if rp.Logf != nil {
		rp.Logf(format, args...)
	}
}
