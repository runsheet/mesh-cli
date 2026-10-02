package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// CredentialRow is a CredentialIndex row (runsheet trd §6i): one of an
// org's or its enterprise's credentials as anyone with standing on the
// org reads it — who it serves and where, never its secret. The node
// reads them at start to say which projects have no credential open to
// it (CredentialCoverage); which one a session runs on is still the
// claim-time read's (SessionCredentials).
type CredentialRow struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// OrgID is the owning org's; empty on an enterprise's credential,
	// whose EnterpriseID is set instead (runsheet ADR 030).
	OrgID        string `json:"orgId"`
	EnterpriseID string `json:"enterpriseId"`
	Vendor       string `json:"vendor"`
	Kind         string `json:"kind"`
	// Serves are the adapters and tools it pays for (claude-code, codex, …).
	Serves []string `json:"serves"`
	// ProjectIDs are the projects that may use it: an org's credential
	// serves only those it names; an enterprise's, every member org's
	// project when it names none.
	ProjectIDs []string `json:"projectIds"`
	// AllowedNodeIDs limits it to those nodes; empty is any node.
	AllowedNodeIDs []string `json:"allowedNodeIds"`
	// Status is active, disabled or deleted.
	Status string `json:"status"`
}

// Enterprise reports whether the credential is an enterprise's: one
// that names an enterprise, or (from a platform that does not serve
// enterpriseId) one whose org is empty.
func (c CredentialRow) Enterprise() bool { return c.EnterpriseID != "" || c.OrgID == "" }

// ServesAdapter reports whether the credential pays for adapter.
func (c CredentialRow) ServesAdapter(adapter string) bool { return slices.Contains(c.Serves, adapter) }

// Credentials lists the credentials open to an org's projects: the
// org's own, read on org_id, and — when the org sits under an
// enterprise — the enterprise's, read on enterprise_id, the two scopes
// the gateway reads CredentialIndex on. A platform without
// OrgIndex.enterpriseId has no enterprises, and the org's own are all.
// A refusal (a node the policy does not let read them) comes back as
// the error.
func (c *Client) Credentials(ctx context.Context, orgID string) ([]CredentialRow, error) {
	out, err := c.credentialsWhere(ctx, where("org_id", orgID))
	if err != nil {
		return nil, err
	}
	ent, err := c.orgEnterprise(ctx, orgID)
	if err != nil || ent == "" {
		return out, err
	}
	more, err := c.credentialsWhere(ctx, where("enterprise_id", ent))
	if err != nil {
		return nil, fmt.Errorf("enterprise %s: %w", ent, err)
	}
	for i := range more {
		if more[i].EnterpriseID == "" {
			more[i].EnterpriseID = ent
		}
	}
	return append(out, more...), nil
}

// credentialsWhere is one CredentialIndex read. It asks for the
// optional fields too (fields/CredentialIndex.txt's `?`: enterpriseId);
// a gateway that does not know one is an older platform, and from then
// on the client asks for credentialFields alone.
func (c *Client) credentialsWhere(ctx context.Context, filter string) ([]CredentialRow, error) {
	var out struct {
		Rows []CredentialRow `json:"credentialIndexs"`
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ credentialIndexs(namespace: %q, where: [%s]) { %s } }`, NS, filter, fields)
	}
	if !c.credentialsWithoutEnterprise.Load() {
		err := c.Query(ctx, q(credentialFields+" "+credentialOptionalFields), nil, &out)
		var ge *Error
		if err == nil || !errors.As(err, &ge) || !unknownField(ge.Message, credentialOptional...) {
			return out.Rows, unsupportedRead(err, "credentialIndexs")
		}
		c.credentialsWithoutEnterprise.Store(true)
	}
	err := c.Query(ctx, q(credentialFields), nil, &out)
	return out.Rows, unsupportedRead(err, "credentialIndexs")
}

// unsupportedRead marks a read the gateway does not know as
// Unsupported (IsUnsupported), a platform older than it.
func unsupportedRead(err error, field string) error {
	var ge *Error
	if err != nil && errors.As(err, &ge) && unknownOperation(ge.Message, field) {
		return &Error{Message: ge.Message, Unsupported: true}
	}
	return err
}

// orgEnterprise is the enterprise the org sits under, empty for none
// or a platform without enterprises.
func (c *Client) orgEnterprise(ctx context.Context, orgID string) (string, error) {
	if c.orgsWithoutEnterprise.Load() {
		return "", nil
	}
	var out struct {
		Org *struct {
			EnterpriseID string `json:"enterpriseId"`
		} `json:"orgIndex"`
	}
	err := c.Query(ctx, fmt.Sprintf(`{ orgIndex(namespace: %q, id: %q) { enterpriseId } }`, NS, orgID), nil, &out)
	var ge *Error
	if err != nil && errors.As(err, &ge) && unknownField(ge.Message, "enterpriseId") {
		c.orgsWithoutEnterprise.Store(true)
		return "", nil
	}
	if err != nil || out.Org == nil {
		return "", err
	}
	return out.Org.EnterpriseID, nil
}
