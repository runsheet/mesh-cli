package commands

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const credMachine = "86e228b4-1111-4111-8111-eeeeeeeeeeee"

// machineJWT is an unsigned access token whose sub is sub: the claims
// parser reads the payload and never the signature.
func machineJWT(sub string) string {
	payload, _ := json.Marshal(map[string]any{"sub": sub, "role": "node"})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// machineToken is ~/.mesh/token's machine shape for an access token and
// the machineId the token file carries ("" leaves it out).
func machineToken(t *testing.T, access, machineID string) string {
	t.Helper()
	raw, err := json.Marshal(auth.Session{Kind: auth.MachineKind, AccessToken: access, MachineID: machineID})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// registerInputs is every registerNode input the fake recorded.
func registerInputs(t *testing.T, f *gatewaytest.Fake) []map[string]any {
	t.Helper()
	var ins []map[string]any
	for _, c := range f.Calls {
		if c.Name == "registerNode" {
			ins = append(ins, c.Input)
		}
	}
	if len(ins) == 0 {
		t.Fatal("no registerNode call recorded")
	}
	return ins
}

// On a machine credential every registerNode names the token file's
// machineId as credentialId, on a platform that refuses a machine
// bearer's registration without it (runsheet#2290).
func TestNodeStartOnMachineTokenSendsCredentialID(t *testing.T) {
	f := credentialFake(t)
	access := machineJWT("machine:" + credMachine)
	f.Token = access
	f.TakesNodeCredential = true
	f.NodeCredentialID = credMachine
	logs := startAgainstOn(t, f, machineToken(t, access, credMachine))
	for _, in := range registerInputs(t, f) {
		if got := in[gateway.NodeCredentialField]; got != credMachine {
			t.Errorf("registerNode credentialId = %v, want %s", got, credMachine)
		}
	}
	if want := "credential: machine " + credMachine + " sent as credentialId"; !strings.Contains(logs, want) {
		t.Errorf("node.log lacks %q:\n%s", want, logs)
	}
}

// A person's rs_* console token sends no credentialId: the aggregate
// refuses a person naming one the row does not.
func TestNodeStartOnConsoleTokenSendsNoCredentialID(t *testing.T) {
	f := credentialFake(t)
	f.TakesNodeCredential = true
	logs := startAgainst(t, f)
	for _, in := range registerInputs(t, f) {
		if v, ok := in[gateway.NodeCredentialField]; ok {
			t.Errorf("registerNode on an rs_* token carries credentialId %v", v)
		}
	}
	if want := "credential: rs_* console token, no credentialId"; !strings.Contains(logs, want) {
		t.Errorf("node.log lacks %q:\n%s", want, logs)
	}
}

// A machine token file without machineId falls back to the access
// token's sub after "machine:"; a console token and a person's sub give
// none.
func TestNodeCredentialIDFallsBackToAccessTokenSub(t *testing.T) {
	cred := &auth.Credential{Machine: &auth.Session{Kind: auth.MachineKind, AccessToken: machineJWT("machine:" + credMachine)}}
	if got := nodeCredentialID(cred); got != credMachine {
		t.Errorf("nodeCredentialID = %q, want %q", got, credMachine)
	}
	for name, c := range map[string]*auth.Credential{
		"nil":         nil,
		"console":     {Console: "rs_test"},
		"person sub":  {Machine: &auth.Session{Kind: auth.MachineKind, AccessToken: machineJWT("user:1")}},
		"not a token": {Machine: &auth.Session{Kind: auth.MachineKind, AccessToken: "opaque"}},
	} {
		if got := nodeCredentialID(c); got != "" {
			t.Errorf("%s: nodeCredentialID = %q, want empty", name, got)
		}
	}
}

// A gateway before runsheet#2290 has no credentialId on
// RegisterNodeInput: the machine registers without it and says why.
func TestNodeStartLeavesCredentialIDOutOnAnOlderGateway(t *testing.T) {
	f := credentialFake(t)
	access := machineJWT("machine:" + credMachine)
	f.Token = access
	f.TakesNodeCredential = false
	logs := startAgainstOn(t, f, machineToken(t, access, credMachine))
	for _, in := range registerInputs(t, f) {
		if v, ok := in[gateway.NodeCredentialField]; ok {
			t.Errorf("registerNode on an older gateway carries credentialId %v", v)
		}
	}
	if !strings.Contains(logs, "credentialId left out") {
		t.Errorf("node.log does not say credentialId was left out:\n%s", logs)
	}
}
