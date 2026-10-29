package nodetools

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// Poster is Mesh, as a tool posts its evidence there (meshapi.Client).
type Poster interface {
	Evidence(ctx context.Context, leaseID string, records []meshapi.EvidencePost) (*meshapi.EvidenceResponse, error)
	PutBlob(ctx context.Context, digest string, data []byte) error
}

// Recorder records a lease's tool runs: it signs each, uploads its
// output, posts it to Mesh and keeps its evidence id for the finish.
type Recorder struct {
	Lease  Lease
	Signer *Signer
	Mesh   Poster

	mu  sync.Mutex
	ids []string
}

// Record signs and posts r, its output (when any) uploaded first as the
// blob r.OutputDigest names, and answers its evidence id. The lease's
// own fields are filled in from the recorder's Lease.
func (rc *Recorder) Record(ctx context.Context, r Record, output []byte) (string, error) {
	if rc == nil || rc.Signer == nil || rc.Mesh == nil {
		return "", fmt.Errorf("the %s record cannot be kept: the node has no recorder for this lease", r.Tool)
	}
	l := rc.Lease
	r.JobID, r.LeaseID, r.LeaseN, r.Product, r.Tenant, r.NodeID = l.JobID, l.LeaseID, l.LeaseN, l.Product, l.Tenant, l.NodeID
	if output != nil {
		r.OutputDigest = Digest(output)
		if err := rc.Mesh.PutBlob(ctx, r.OutputDigest, output); err != nil {
			return "", fmt.Errorf("uploading the %s output: %w", r.Tool, err)
		}
	}
	payload, err := Statement(r)
	if err != nil {
		return "", err
	}
	post, err := rc.Signer.Sign(ctx, payload, slices.Contains(l.EvidenceRequired, r.Tool))
	if err != nil {
		return "", fmt.Errorf("signing the %s record: %w", r.Tool, err)
	}
	resp, err := rc.Mesh.Evidence(ctx, l.LeaseID, []meshapi.EvidencePost{post})
	if err != nil {
		return "", fmt.Errorf("posting the %s record: %w", r.Tool, err)
	}
	if len(resp.IDs) != 1 {
		return "", fmt.Errorf("Mesh answered %d evidence ids for one record", len(resp.IDs))
	}
	id := resp.IDs[0]
	if want := Digest(payload); id != want {
		return "", fmt.Errorf("Mesh named the %s record %s, not its digest %s", r.Tool, id, want)
	}
	rc.mu.Lock()
	rc.ids = append(rc.ids, id)
	rc.mu.Unlock()
	return id, nil
}

// IDs are the evidence ids of every record posted, in order.
func (rc *Recorder) IDs() []string {
	if rc == nil {
		return nil
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return slices.Clone(rc.ids)
}
