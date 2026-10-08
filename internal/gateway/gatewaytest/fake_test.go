package gatewaytest_test

import (
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

func TestFakeRefusesMachineSignIn(t *testing.T) {
	f := gatewaytest.New("machine-token")
	defer f.Close()
	f.UserID = "user-1"
	f.NodeCredentialID = "e42ee625"
	f.RefusesMachineSignIn = true
	_, err := gateway.New(f.URL(), "machine-token").SignIn(t.Context())
	if err == nil || !strings.Contains(err.Error(), "a person's own, not a machine's") {
		t.Fatalf("machine signIn error = %v, want a person's own, not a machine's", err)
	}
	if f.SignInCalls != 1 {
		t.Fatalf("SignInCalls = %d, want 1", f.SignInCalls)
	}
	found := false
	for _, c := range f.Calls {
		found = found || c.Name == "signIn"
	}
	if !found {
		t.Fatal("signIn not recorded in Calls")
	}

	p := gatewaytest.New("person-token")
	defer p.Close()
	p.UserID = "user-2"
	id, err := gateway.New(p.URL(), "person-token").SignIn(t.Context())
	if err != nil || id != "user-2" {
		t.Fatalf("person signIn = %q, %v; want user-2", id, err)
	}
}
