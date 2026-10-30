package nodev1

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/secreport"
	"projectmesh.io/mesh-cli/internal/validate"
)

// A suite step the pod's memory or pids limit killed is a sandbox denial
// the node saw: reported as one, by its limit alone; a timeout is not.
func TestASandboxLimitKillIsReported(t *testing.T) {
	f := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "mesh-node:test", Limits: sandbox.Limits{Memory: "1g"}}}
	sess, err := sb.Open("lease-1", "conform/validate", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.Pin("mesh-v1-lease-1", nil)
	var denied []error
	p := &leasePod{sess: sess, name: "mesh-v1-lease-1", denied: func(err error) { denied = append(denied, err) }}
	f.Agent = func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) {
		return sandbox.Exit{Code: 137, OOMKilled: true}, nil
	}
	res, err := p.suites(context.Background(), t.TempDir(), []validate.Step{{Name: "big", Command: "oom"}}, nil)
	if err != nil || len(res) != 1 || res[0].ExitCode != 137 {
		t.Fatalf("the step %+v %v", res, err)
	}
	if len(denied) != 1 {
		t.Fatalf("denials heard %v", denied)
	}

	rp := &secreport.Reporter{Lease: "lease-1"}
	reportSandbox(rp, denied[0])
	reportSandbox(rp, fmt.Errorf("claude: %w", &sandbox.LimitError{Limit: "pids", Value: "4096"}))
	reportSandbox(rp, &sandbox.LimitError{Limit: "timeout", Value: "30m0s"})
	reportSandbox(rp, errors.New("exit 1"))
	reportSandbox(rp, nil)
	if rp.Pending() != 2 {
		t.Fatalf("%d sandbox reports, want memory and pids", rp.Pending())
	}
}
