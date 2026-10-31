package nodev1

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// Lease end releases (mesh-specs gap 28 item 6; ADR-046): as a lease
// ends — finished, failed, cancelled, past its deadline or the node
// draining — and before its end is told to Mesh, which revokes its
// Latchkey session, the node drops every credential it held for the
// lease and, when the lease was minted children of a scoped connection
// (access.mint), asks Latchkey to revoke them on the same token:
//
//	POST <Latchkey>/v1/credentials/for-job/release {}
//	Authorization: DPoP <mesh-session token>, DPoP: <proof>
//	→ {released: n}
//
// The answer is recorded on the lease as an AccessRecord (released n,
// or release_failed and why); a failure never holds the lease open:
// Latchkey's own backstops — the session's end revoking its children,
// the five-minute sweep — revoke what the release did not.

// releasePath is Latchkey's for-job release route.
const releasePath = "/v1/credentials/for-job/release"

// releaseTimeout bounds the release, so a lease's end is not held by it.
const releaseTimeout = 15 * time.Second

// ReleaseForJob asks Latchkey to revoke every child minted on the
// session: the number it revoked.
func (s *sessionTokens) ReleaseForJob(ctx context.Context) (int, error) {
	var out struct {
		Released int `json:"released"`
	}
	if err := s.onToken(ctx, http.MethodPost, releasePath, map[string]string{}, &out); err != nil {
		return 0, err
	}
	return out.Released, nil
}

// releaser is how a lease's carried credentials end: drop is every held
// value, release Latchkey's call, nil when nothing was minted.
type releaser struct {
	held    func() []*heldCredential
	release func(ctx context.Context) (int, error)
}

// drop forgets the credential held: nothing is injected after.
func (h *heldCredential) drop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cred, h.spec.Hosts = nil, nil
}

// setReleaser has the lease's end run rel.
func (v *injectedValues) setReleaser(rel *releaser) {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.rel = rel
	v.mu.Unlock()
}

// releaseCarried ends lease id's carried credentials: every held value
// dropped and, when children were minted, Latchkey's release, its
// answer recorded on the lease. Run before the lease's end is told to
// Mesh.
func (r *Runtime) releaseCarried(id string) {
	v := r.injectedOf(id)
	if v == nil {
		return
	}
	v.mu.Lock()
	rel := v.rel
	v.rel = nil
	v.mu.Unlock()
	if rel == nil {
		return
	}
	if rel.held != nil {
		for _, h := range rel.held() {
			h.drop()
		}
	}
	if rel.release == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	rec := meshapi.AccessRecord{Kind: "api", Vendor: "latchkey", Service: "release", Method: http.MethodPost, Path: releasePath, At: time.Now().UTC()}
	n, err := rel.release(ctx)
	if err != nil {
		rec.Outcome, rec.Reason = "failed", "release_failed: "+err.Error()
		r.logf("nodev1: lease %s: release_failed: the minted children were not released (%v); Latchkey's sweep revokes them", id, err)
	} else {
		rec.Outcome, rec.Reason = "ok", fmt.Sprintf("released %d", n)
		r.logf("nodev1: lease %s: released; Latchkey revoked %d minted child(ren)", id, n)
	}
	r.keepAccess(id, rec)
}
