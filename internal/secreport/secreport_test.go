package secreport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// fakeMesh answers each post with the next status it is given (202 when
// none is left) and keeps every batch it took.
type fakeMesh struct {
	mu       sync.Mutex
	statuses []int
	posts    [][]meshapi.SecurityReport
	taken    [][]meshapi.SecurityReport
	leases   []string
}

func (m *fakeMesh) Security(_ context.Context, lease string, reports []meshapi.SecurityReport) (*meshapi.SecurityAccepted, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, reports)
	m.leases = append(m.leases, lease)
	status := http.StatusAccepted
	if len(m.statuses) > 0 {
		status, m.statuses = m.statuses[0], m.statuses[1:]
	}
	switch {
	case status == -1:
		return nil, errors.New("connection refused")
	case status != http.StatusAccepted:
		return nil, &meshapi.Error{Status: status, Code: "refused"}
	}
	m.taken = append(m.taken, reports)
	return &meshapi.SecurityAccepted{LeaseID: lease, Accepted: len(reports)}, nil
}

func (m *fakeMesh) takenReports() []meshapi.SecurityReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []meshapi.SecurityReport
	for _, b := range m.taken {
		out = append(out, b...)
	}
	return out
}

func egressReport(host, verdict string, bytes int64) meshapi.SecurityReport {
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	return meshapi.SecurityReport{Type: meshapi.ReportEgress, Host: host, Verdict: verdict, Bytes: bytes, Rule: egress.RuleHosts, At: &at}
}

// A report is checked as Mesh checks it: only a content-free one —
// never a URL, path, port, user or match — is kept.
func TestCheckIsMeshs(t *testing.T) {
	ok := []meshapi.SecurityReport{
		egressReport("api.github.com", "allow", 2048),
		egressReport("203.0.113.9", "deny", 0),
		egressReport("2001:db8::1", "deny", 0),
		{Type: "sandbox", Verdict: "deny", Rule: "limit:memory"},
		{Type: "dlp", Verdict: "redact", Rule: "aws-access-key"},
	}
	for _, r := range ok {
		if err := Check(r); err != nil {
			t.Errorf("%+v refused: %v", r, err)
		}
	}
	bad := []meshapi.SecurityReport{
		{Type: "keylog", Verdict: "allow"},
		{Type: "egress", Verdict: "allow"},
		{Type: "egress", Host: "https://api.github.com/repos", Verdict: "allow"},
		{Type: "egress", Host: "api.github.com:443", Verdict: "allow"},
		{Type: "egress", Host: "user@api.github.com", Verdict: "allow"},
		{Type: "egress", Host: "API.GITHUB.COM", Verdict: "allow"},
		{Type: "egress", Host: "api.github.com", Verdict: "maybe"},
		{Type: "egress", Host: "api.github.com", Verdict: "allow", Bytes: -1},
		{Type: "sandbox", Verdict: "deny", Rule: "Bearer abc.def"},
		{Type: "sandbox", Verdict: "deny", Rule: strings.Repeat("a", 65)},
	}
	for _, r := range bad {
		if Check(r) == nil {
			t.Errorf("%+v was taken", r)
		}
	}
}

// A tunnel's call is reported by its bare host, verdict, bytes and rule
// alone; a host that is not bare is not reported with what was sent.
func TestEgressReportsAreContentFree(t *testing.T) {
	at := time.Now()
	r, ok := Egress(egress.Call{Host: "Proxy.Golang.org.", Port: "443", Allowed: true, Rule: egress.RuleHosts, Bytes: 9000}, at)
	if !ok || r.Host != "proxy.golang.org" || r.Verdict != "allow" || r.Bytes != 9000 || r.Rule != "egress.hosts" || r.At == nil {
		t.Fatalf("allowed: %+v %v", r, ok)
	}
	r, ok = Egress(egress.Call{Host: "evil.example", Port: "22", Refused: "only port 443 leaves the relay, not evil.example:22", Rule: egress.RulePort}, at)
	if !ok || r.Verdict != "deny" || r.Rule != "egress.port" || r.Bytes != 0 {
		t.Fatalf("refused: %+v", r)
	}
	if _, ok := Egress(egress.Call{Host: "evil.example/steal?token=abc", Rule: egress.RuleHosts}, at); ok {
		t.Fatal("a host that is not bare was reported")
	}
	if r, ok := Egress(egress.Call{Host: "[2001:db8::1]", Rule: egress.RuleHosts}, at); !ok || r.Host != "2001:db8::1" {
		t.Fatalf("an IPv6 literal: %+v %v", r, ok)
	}
}

// Reports are batched per lease, equal ones folded with their bytes
// summed, sent at most MaxSecurityReports a post, and flushed whole when
// the lease releases.
func TestReporterBatchesAndFlushesOnRelease(t *testing.T) {
	m := &fakeMesh{}
	rp := &Reporter{Lease: "lease-1", Mesh: m, Every: time.Hour, Logf: t.Logf}
	rp.Start()
	rp.Add(egressReport("api.github.com", "allow", 100))
	rp.Add(egressReport("api.github.com", "allow", 50))
	rp.Add(egressReport("api.github.com", "deny", 0))
	for i := range 150 {
		rp.Add(egressReport(fmt.Sprintf("h%d.example", i), "allow", 1))
	}
	rp.Add(meshapi.SecurityReport{Type: "egress", Host: "https://x.example/p", Verdict: "allow"}) // refused here
	rp.Close(context.Background())
	got := m.takenReports()
	if len(got) != 152 {
		t.Fatalf("%d reports reached Mesh, want 152", len(got))
	}
	if got[0].Host != "api.github.com" || got[0].Bytes != 150 || got[1].Verdict != "deny" {
		t.Fatalf("equal reports are not folded: %+v %+v", got[0], got[1])
	}
	for _, b := range m.posts {
		if len(b) == 0 || len(b) > meshapi.MaxSecurityReports {
			t.Fatalf("a post of %d reports", len(b))
		}
	}
	for _, l := range m.leases {
		if l != "lease-1" {
			t.Fatalf("posted for lease %s", l)
		}
	}
	rp.Add(egressReport("late.example", "allow", 1))
	if rp.Pending() != 0 {
		t.Fatal("a report after the release was kept")
	}
}

// A 422 is final: the batch is dropped and the next one sent. A 5xx or a
// transport error keeps it, and it is sent again.
func TestReporterDropsOn422AndRetries5xx(t *testing.T) {
	m := &fakeMesh{statuses: []int{http.StatusUnprocessableEntity}}
	rp := &Reporter{Lease: "lease-1", Mesh: m, Logf: t.Logf}
	rp.Add(egressReport("a.example", "allow", 1))
	rp.Close(context.Background())
	if len(m.posts) != 1 || len(m.taken) != 0 || rp.Pending() != 0 {
		t.Fatalf("a refused batch: posts %d taken %d pending %d", len(m.posts), len(m.taken), rp.Pending())
	}

	m = &fakeMesh{statuses: []int{http.StatusServiceUnavailable, -1, http.StatusBadGateway}}
	rp = &Reporter{Lease: "lease-1", Mesh: m, Logf: t.Logf}
	rp.Add(egressReport("a.example", "allow", 1))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rp.Close(ctx)
	if len(m.posts) != 4 || len(m.taken) != 1 || m.taken[0][0].Host != "a.example" {
		t.Fatalf("a batch Mesh could not take: posts %d taken %v", len(m.posts), m.taken)
	}

	// a release whose time runs out drops what Mesh never took
	m = &fakeMesh{statuses: []int{503, 503, 503, 503, 503, 503, 503, 503}}
	rp = &Reporter{Lease: "lease-1", Mesh: m, Logf: t.Logf}
	rp.Add(egressReport("a.example", "allow", 1))
	short, cancel2 := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel2()
	rp.Close(short)
	if len(m.taken) != 0 || rp.Pending() != 0 {
		t.Fatalf("taken %v pending %d", m.taken, rp.Pending())
	}
}

// The timer sends what is pending while the lease runs; a full batch is
// sent at once.
func TestReporterSendsOnItsTimer(t *testing.T) {
	m := &fakeMesh{}
	rp := &Reporter{Lease: "lease-1", Mesh: m, Every: 20 * time.Millisecond, Logf: t.Logf}
	rp.Start()
	defer rp.Close(context.Background())
	rp.Add(egressReport("a.example", "allow", 1))
	deadline := time.Now().Add(5 * time.Second)
	for len(m.takenReports()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the timer sent nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
