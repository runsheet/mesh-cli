package nodev1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// connect offers the node to each of Connect (runsheet ADR 066). A
// connection Mesh refuses because its product does not exist yet (404:
// "no active product", the product has not published) or answers 429 or
// 5xx to is not fatal: it is logged and offered again in the background
// (retryConnect) while the node runs on its other connections, so one
// unpublished product never takes a node's others down with it. Any other
// refusal stops the node, as a misconfigured --connect should.
func (r *Runtime) connect(ctx context.Context, nodeID string) error {
	for _, want := range r.Connect {
		err := r.connectOne(ctx, nodeID, want)
		if err == nil {
			continue
		}
		if !connectRetryable(err) {
			return err
		}
		r.logf("nodev1: %v; the node runs on its other connections and offers this one again", err)
		go r.retryConnect(ctx, nodeID, want)
	}
	return nil
}

// connectOne offers the node to one connection and logs Mesh's answer.
func (r *Runtime) connectOne(ctx context.Context, nodeID string, want meshapi.ConnectRequest) error {
	c, err := r.API.Connect(ctx, nodeID, want)
	if err != nil {
		return fmt.Errorf("nodev1: connect to %s for %s: %w", want.Tenant, want.Product, err)
	}
	if c.Status == "accepted" {
		r.logf("nodev1: connected to %s for %s (%s)", c.Tenant, c.Product, c.Via)
		return nil
	}
	r.logf("nodev1: the connection to %s for %s is %s: it runs nothing until the tenant accepts it", c.Tenant, c.Product, c.Status)
	return nil
}

// retryConnect offers the connection again, waiting ConnectRetry and
// doubling to ConnectRetryMax, until Mesh answers it, refuses it for good
// (logged, the node runs on) or ctx ends.
func (r *Runtime) retryConnect(ctx context.Context, nodeID string, want meshapi.ConnectRequest) {
	wait, most := r.ConnectRetry, r.ConnectRetryMax
	if wait <= 0 {
		wait = time.Minute
	}
	if most <= 0 {
		most = 5 * time.Minute
	}
	most = max(most, wait)
	for {
		r.sleep(ctx, wait)
		if ctx.Err() != nil {
			return
		}
		err := r.connectOne(ctx, nodeID, want)
		if err == nil || ctx.Err() != nil {
			return
		}
		if !connectRetryable(err) {
			r.logf("nodev1: %v; the node gives up on this connection", err)
			return
		}
		wait = min(wait*2, most)
		r.logf("nodev1: %v; offered again in %s", err, wait)
	}
}

// connectRetryable is whether Mesh's refusal of a connection may pass:
// its product does not exist yet (404), or Mesh is busy or failing (429,
// 5xx).
func connectRetryable(err error) bool {
	var e *meshapi.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == http.StatusNotFound || e.Status == http.StatusTooManyRequests || e.Status >= 500
}
