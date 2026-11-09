package node

import (
	"context"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// CredentialGateway is what the claim-time read and the usage ledger
// need of the platform; *gateway.Client is one.
type CredentialGateway interface {
	Session(ctx context.Context, ref gateway.SessionRef, nodeID string) (*gateway.Session, error)
	RecordToolUsage(ctx context.Context, itemID, toolCallID, tool string, usage gateway.UsageReport) error
}
