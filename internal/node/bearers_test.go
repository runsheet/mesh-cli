package node

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// Bearer kinds as the rule fixture names them (gatewaytest.Fake.BearerKind).
const (
	personBearer  = "person"
	machineBearer = "machine"
)

// bearerMachineID is the Latchkey machine id of newBearerFake's machine
// bearer: the credentialId a node on it sends.
const bearerMachineID = "86e228b4-5c1d-4e2a-9b3f-0a1b2c3d4e5f"

// newBearerFake starts gatewaytest's Fake, taking credentialId as a
// gateway after runsheet#2290 does, and returns it with a client on its
// bearer: a person's rs_* token for kind "person", the machine
// credential bearerMachineID for kind "machine" (mesh-cli ADR-026).
func newBearerFake(t *testing.T, kind string) (*gatewaytest.Fake, *gateway.Client) {
	t.Helper()
	f := gatewaytest.New("rs_bearer-token")
	t.Cleanup(f.Close)
	f.TakesNodeCredential = true
	switch kind {
	case machineBearer:
		f.NodeCredentialID = bearerMachineID
	case personBearer:
	default:
		t.Fatalf("newBearerFake: bearer kind %q is neither person nor machine", kind)
	}
	if got := f.BearerKind(f.Token); got != kind {
		t.Fatalf("newBearerFake(%s): the fake sees a %s bearer", kind, got)
	}
	return f, gateway.New(f.URL(), f.Token)
}

// bearerCredentialID is the credentialId a node on kind's bearer has:
// the machine id on a machine credential, none on a console token.
func bearerCredentialID(kind string) string {
	if kind == machineBearer {
		return bearerMachineID
	}
	return ""
}
