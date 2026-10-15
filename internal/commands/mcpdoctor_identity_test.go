package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/gateway"
)

// mesh mcp doctor opens with who the runsheet server acts as and what
// it reaches, by runsheet ADR 056 and Latchkey ADR 013: a person's MCP
// is the person and their orgs; a machine's is its operator, via the
// machine (ADR-025, Doctor).

// fakeIdentityGW answers me, myOrgs and projectIndexs from fields.
type fakeIdentityGW struct {
	email    string
	orgs     []gateway.Org
	projects map[string][]gateway.Project // by org id
	err      error                        // every read fails with it
	meCalls  int
}

func (f *fakeIdentityGW) Me(context.Context) (*gateway.Person, error) {
	f.meCalls++
	if f.err != nil {
		return nil, f.err
	}
	return &gateway.Person{ID: "u-1", Email: f.email}, nil
}

func (f *fakeIdentityGW) MyOrgs(context.Context) ([]gateway.Org, error) {
	return f.orgs, f.err
}

func (f *fakeIdentityGW) Projects(_ context.Context, orgID string) ([]gateway.Project, error) {
	return f.projects[orgID], f.err
}

// fakeIdentity makes doctor load cred (nil: none, not signed in) and
// ask gw.
func fakeIdentity(t *testing.T, cred *auth.Credential, gw *fakeIdentityGW) {
	t.Helper()
	mcpLoadCredential = func() (*auth.Credential, error) {
		if cred == nil {
			return nil, errors.New("no platform credential")
		}
		return cred, nil
	}
	mcpIdentityGateway = func(*auth.Credential) identityGateway { return gw }
	t.Cleanup(func() {
		mcpLoadCredential = auth.LoadCredential
		mcpIdentityGateway = func(cred *auth.Credential) identityGateway {
			return gateway.NewWithSource(gatewayURL(), auth.NewSource(cred, refreshMachine))
		}
	})
}

// runsheetOrg is one org, runsheet, with projects runsheet and mesh.
func runsheetOrg(email string) *fakeIdentityGW {
	return &fakeIdentityGW{
		email: email,
		orgs:  []gateway.Org{{ID: "org-1", Login: "runsheet"}},
		projects: map[string][]gateway.Project{"org-1": {
			{ID: "p-2", Slug: "runsheet"}, {ID: "p-1", Slug: "mesh"},
		}},
	}
}

func TestMcpDoctorNamesTheConsoleTokensPersonAndProjects(t *testing.T) {
	installHome(t)
	fakeIdentity(t, &auth.Credential{Console: "rs_abc"}, runsheetOrg("chris@runsheet.dev"))

	out := doctor(t, false, true)
	want := "identity: chris@runsheet.dev (person)\nprojects: runsheet/mesh, runsheet/runsheet\nno client found\n"
	if out != want {
		t.Fatalf("doctor = %q, want %q", out, want)
	}

	got := parseDoctorJSON(t, doctor(t, true, true))
	if got.Identity.Person != "chris@runsheet.dev" || got.Identity.Via != "" ||
		strings.Join(got.Identity.Projects, ",") != "runsheet/mesh,runsheet/runsheet" {
		t.Fatalf("doctor --json identity = %+v", got.Identity)
	}
}

func TestMcpDoctorNamesAMachinesOperatorViaTheMachine(t *testing.T) {
	installHome(t)
	cred := &auth.Credential{Machine: &auth.Session{
		Kind: auth.MachineKind, AccessToken: "not-a-jwt", MachineID: "m-42", UserEmail: "op@runsheet.dev",
	}}
	gw := runsheetOrg("machine-not-a-person@runsheet.dev")
	fakeIdentity(t, cred, gw)

	out := doctor(t, false, true)
	if !strings.HasPrefix(out, "identity: op@runsheet.dev (person, via machine m-42)\nprojects: runsheet/mesh, runsheet/runsheet\n") {
		t.Fatalf("doctor:\n%s", out)
	}
	got := parseDoctorJSON(t, doctor(t, true, true))
	if got.Identity.Person != "op@runsheet.dev" || got.Identity.Via != "machine m-42" || len(got.Identity.Projects) != 2 {
		t.Fatalf("doctor --json identity = %+v", got.Identity)
	}
}

func TestMcpDoctorMachineWithNoRecordedEmailAsksTheGateway(t *testing.T) {
	installHome(t)
	cred := &auth.Credential{Machine: &auth.Session{Kind: auth.MachineKind, AccessToken: "x", MachineID: "m-7"}}
	gw := runsheetOrg("op@runsheet.dev")
	fakeIdentity(t, cred, gw)

	out := doctor(t, false, true)
	if !strings.HasPrefix(out, "identity: op@runsheet.dev (person, via machine m-7)\n") || gw.meCalls != 1 {
		t.Fatalf("doctor (me asked %d times):\n%s", gw.meCalls, out)
	}
}

func TestMcpDoctorNotSignedInExits1(t *testing.T) {
	installHome(t, ".cursor")
	fakeIdentity(t, nil, &fakeIdentityGW{})

	out, err := runDoctor(t, false)
	if err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("doctor err = %v, want not signed in\n%s", err, out)
	}
	if !strings.HasPrefix(out, "identity: not signed in; run mesh auth login\n") || !strings.Contains(out, "cursor: missing in ") {
		t.Fatalf("doctor:\n%s", out)
	}

	// signed out with no client found still exits 1
	installHome(t)
	fakeIdentity(t, nil, &fakeIdentityGW{})
	if out, err := runDoctor(t, true); err == nil {
		t.Fatalf("doctor --json exited 0 signed out:\n%s", out)
	} else if got := parseDoctorJSON(t, out); got.Identity.Person != "" || !strings.Contains(got.Identity.Error, "not signed in") {
		t.Fatalf("doctor --json identity = %+v", got.Identity)
	}
}

func TestMcpDoctorGatewayErrorStillPrintsTheClients(t *testing.T) {
	installHome(t)
	mustInstall(t, false, "codex")
	gw := runsheetOrg("chris@runsheet.dev")
	gw.err = errors.New("gateway: 503 Service Unavailable")
	fakeIdentity(t, &auth.Credential{Machine: &auth.Session{
		Kind: auth.MachineKind, AccessToken: "x", MachineID: "m-42", UserEmail: "op@runsheet.dev",
	}}, gw)

	out := doctor(t, false, true) // a gateway error is not fatal
	if !strings.HasPrefix(out, "identity: op@runsheet.dev (projects unavailable: gateway: 503 Service Unavailable)\n") {
		t.Fatalf("doctor:\n%s", out)
	}
	if !strings.Contains(out, "codex: configured "+testExe(t)+" (") {
		t.Fatalf("doctor: no codex line after a gateway error:\n%s", out)
	}
	got := parseDoctorJSON(t, doctor(t, true, true))
	if got.Identity.Person != "op@runsheet.dev" || got.Identity.Error == "" || len(got.Identity.Projects) != 0 || len(got.Clients) != 1 {
		t.Fatalf("doctor --json = %+v", got)
	}
}
