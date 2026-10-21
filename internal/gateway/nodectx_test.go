package gateway_test

import (
	"context"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// nodeCtx is a node's request context: finishJob names the node that
// finishes (gateway.WithNodeID), as every node session's context does.
func nodeCtx() context.Context { return gateway.WithNodeID(context.Background(), "node-test") }
