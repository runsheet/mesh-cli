package gateway

import "github.com/google/uuid"

// The platform's aggregate ids are v5 uuids over one fixed namespace
// (runsheet/platform/ids). Deriving them here means a node registers and
// a goal is addressed without a lookup — the same slug always lands on
// the same aggregate.
var idNamespace = uuid.MustParse("7b3c1d2e-4f5a-4b6c-8d7e-9f0a1b2c3d4e")

// NodeID is the Node aggregate id for a slug under an org.
func NodeID(orgID uuid.UUID, slug string) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte("node:"+orgID.String()+":"+slug))
}

// NodeOwnerID is the Node aggregate id for a slug under an owner on the
// owner contract (platform/ids/node.go, NodeOwnerID): kind is "person",
// "enterprise" or "org" (owner the org's id, runsheet goal
// a-node-is-owned-by-an-org), an empty kind reading as "person". This is the
// formula the platform derives a registration's aggregate id by, and
// refuses a registerNode whose aggregateId is not this — never NodeID,
// which is the per-org formula with the owner's uuid in the org's
// place (ADR-015, 2026-10-03).
func NodeOwnerID(kind string, owner uuid.UUID, slug string) uuid.UUID {
	if kind == "" {
		kind = "person"
	}
	return uuid.NewSHA1(idNamespace, []byte("node:"+kind+":"+owner.String()+":"+slug))
}

// GoalID is the Goal aggregate id for a slug under an org.
func GoalID(orgID uuid.UUID, slug string) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte("goal:"+orgID.String()+":"+slug))
}

// ProjectID is the Project aggregate id for a slug under an org.
func ProjectID(orgID uuid.UUID, slug string) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte("project:"+orgID.String()+":"+slug))
}

// AgentID is the Agent aggregate id for one definition of a slug in one
// scope: the platform's defaults (both ids uuid.Nil), an org's (the org
// alone) or a context project's (both).
func AgentID(orgID, projectID uuid.UUID, slug string) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte("agent:"+orgID.String()+":"+projectID.String()+":"+slug))
}
