package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
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

// An exposure is told to Latchkey in the same place, before the release
// and before Mesh hears the end (latchkey #697; ADR 031, As built —
// rollout step 3): when the exposed ref is a Secret the job's
// access.secrets names,
//
//	POST <Latchkey>/v1/credentials/for-job/exposed {secret_ref, version}
//	Authorization: DPoP <mesh-session token>, DPoP: <proof>
//	→ {exposed, already_exposed, exposed_versions, rotate, …}
//
// marks that version exposed on the Secret, prompting its rotation. The
// outcome is recorded on the lease and in the node's log (ref, version,
// Latchkey's answer or code), never a value. A scoped connection's child
// (access.mint) has no exposure route: the release that follows revokes
// it. Other values the relay holds — a federation target's cloud token,
// the git token — are dropped with the lease and end with its session.

// exposedPath is Latchkey's for-job exposure route.
const exposedPath = "/v1/credentials/for-job/exposed"

// exposedAnswer is Latchkey's answer to an exposure.
type exposedAnswer struct {
	Exposed         bool              `json:"exposed"`
	AlreadyExposed  bool              `json:"already_exposed"`
	ExposedVersions []json.RawMessage `json:"exposed_versions"`
	Rotate          bool              `json:"rotate"`
}

// SecretExposed tells Latchkey that version of the Secret ref was
// exposed.
func (s *sessionTokens) SecretExposed(ctx context.Context, ref, version string) (*exposedAnswer, error) {
	var out exposedAnswer
	if err := s.onToken(ctx, http.MethodPost, exposedPath, map[string]string{"secret_ref": ref, "version": version}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

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
	held func() []*heldCredential
	// fed are the federation targets' cloud tokens (federation.go),
	// dropped with the rest: nothing is kept across leases
	fed     func() []*heldFederation
	release func(ctx context.Context) (int, error)
	// secrets are the job's access.secrets, exposed tells Latchkey of
	// one's exposure; nil when the job carried none
	secrets []string
	exposed func(ctx context.Context, ref, version string) (*exposedAnswer, error)
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
	if rel.fed != nil {
		for _, h := range rel.fed() {
			h.drop()
		}
	}
	if e := v.exposure(); e != nil {
		r.tellExposed(id, rel, e)
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

// tellExposed tells Latchkey of lease id's exposure when its ref is a
// Secret of the job's, recording the outcome on the lease and in the
// log; otherwise it says why Latchkey is not told.
func (r *Runtime) tellExposed(id string, rel *releaser, e *credentialExposed) {
	if rel.exposed == nil || !slices.Contains(rel.secrets, e.ref) {
		r.logf("nodev1: lease %s: %s is not a Secret of the job's access.secrets; Latchkey has no exposure route for it: a connection's child is released with the lease, any other value ends with its session", id, e.ref)
		return
	}
	rec := meshapi.AccessRecord{Kind: "api", Vendor: "latchkey", Service: "exposed", Method: http.MethodPost, Path: exposedPath, At: time.Now().UTC()}
	if e.version == "" {
		rec.Outcome, rec.Reason = "failed", "exposed_not_told: no version of "+e.ref+" was held"
		r.logf("nodev1: lease %s: Latchkey was not told %s was exposed: no version of it was held; rotate it", id, e.ref)
		r.keepAccess(id, rec)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	ans, err := rel.exposed(ctx, e.ref, e.version)
	if err != nil {
		rec.Outcome, rec.Reason = "failed", "exposed_not_told: "+err.Error()
		r.logf("nodev1: lease %s: Latchkey was not told version %s of %s was exposed (%v): rotate it", id, e.version, e.ref, err)
		r.keepAccess(id, rec)
		return
	}
	vs := make([]string, 0, len(ans.ExposedVersions))
	for _, x := range ans.ExposedVersions {
		vs = append(vs, strings.Trim(string(x), `"`))
	}
	what := "exposed"
	if ans.AlreadyExposed {
		what = "already_exposed"
	}
	rotate := ""
	if ans.Rotate {
		rotate = "; rotate it"
	}
	rec.Outcome, rec.Reason = "ok", fmt.Sprintf("%s version %s of %s (exposed versions %s)%s", what, e.version, e.ref, strings.Join(vs, ", "), rotate)
	r.logf("nodev1: lease %s: Latchkey marked %s", id, rec.Reason)
	r.keepAccess(id, rec)
}
