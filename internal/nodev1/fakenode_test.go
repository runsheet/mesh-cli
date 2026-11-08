package nodev1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
)

// fakeNodeEnv, set to "1", makes this test binary stand in for a real
// node process in TestANodesDeathNeverTouchesItsLeasesEgressProcess
// (killnode_test.go): it starts a lease's egress process exactly as
// the node's work loop does (relaySidecar.startEgressProcess, ADR-051
// decision 1), prints the egress process's pid and the path of its own
// lane socket (the one leaseEgressCmd's --lane serves, where "a run_suite
// lane's CONNECT lands" directly on the egress process itself, never
// through the node) on stdout, then blocks forever. It is killed
// outright, never asked to release the lease, so the test can prove the
// egress process and an in-flight connection through its own lane
// socket outlive the node that started them -- the node's own
// forwarding sockets (openLaneTunnel, forwardConnect) are goroutines of
// this very process and are expected to die with it; they are not what
// decisions 1, 3 and 5 promise survives.
const fakeNodeEnv = "MESH_V1_FAKE_NODE"

// fakeNodeMeshBinaryEnv names the real mesh binary this fake node execs
// lease-egress from: testMeshBinary builds it (it needs a *testing.T),
// so the outer test passes the path down rather than this process
// building its own.
const fakeNodeMeshBinaryEnv = "MESH_V1_FAKE_NODE_MESH_BINARY"

// fakeNodeHostEnv names the one host the fake node's lease is allowed
// to reach; the test's CONNECT names it too.
const fakeNodeHostEnv = "MESH_V1_FAKE_NODE_HOST"

// maybeRunFakeNode runs instead of the test binary's own tests when
// fakeNodeEnv is set (TestMain calls it before m.Run()); it never
// returns, so m.Run() is never reached in this process.
func maybeRunFakeNode() {
	if os.Getenv(fakeNodeEnv) != "1" {
		return
	}
	host := os.Getenv(fakeNodeHostEnv)
	dir, err := os.MkdirTemp("", "fake-node")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake node: temp dir:", err)
		os.Exit(1)
	}
	rs := &relaySidecar{
		r:    &Runtime{Logf: func(string, ...any) {}, MeshBinary: os.Getenv(fakeNodeMeshBinaryEnv)},
		b:    &meshapi.Brief{LeaseID: "lease-kill", Egress: &meshapi.Egress{Hosts: []string{host}}},
		dir:  dir,
		node: &relaysidecar.Node{},
	}
	key, err := dpop.NewKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake node: dpop key:", err)
		os.Exit(1)
	}
	handoff, err := key.Handoff()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake node: handoff:", err)
		os.Exit(1)
	}
	creds := relaysidecar.Credentials{Key: handoff, Session: relaysidecar.Token{Value: "lease-kill-token"}}
	policy := egress.NewPolicy([]string{host}, nil, nil)
	noop := func(egress.Call) {}
	if err := rs.startEgressProcess(context.Background(), policy, rs.b, creds, noop, noop); err != nil {
		fmt.Fprintln(os.Stderr, "fake node: starting egress:", err)
		os.Exit(1)
	}
	laneSock := filepath.Join(rs.egressDir, egressLaneSocket)
	fmt.Printf("READY %d %s\n", rs.egressCmd.Process.Pid, laneSock)
	os.Stdout.Sync()
	select {}
}
