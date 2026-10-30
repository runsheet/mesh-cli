package nodev1

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// The lease's pod (ADR-044 amendment, `mesh node run --sandbox`): with
// Runtime.Pods set, a lease's session and its services run in one
// rootless pod of the lease's own, the legacy node's sandbox (mesh-specs
// ADR 007) — the checkout and a HOME under the lease's directory the
// only host paths it sees, its environment an allow-list, its network
// the public internet and not the LAN or the host. The job actor names
// the pod before it exists (PodName of the lease) and holds it, so
// actor.json names it for a node that restarts; it is labelled with the
// job, the node and the lease (PodJobLabel, PodNodeLabel and
// PodLeaseLabel), which the supervisor's reconcile reads. The agent runs
// on the CPUs and memory the scheduler leased the lease (the brief's
// resources), as sandbox.AgentName(pod), which watchPod and the
// reconcile watch: an exit 137 or 143 the actor did not cause is
// pod_killed.
//
// The services are what the checkout's .mesh/services.yml declares
// (sandbox.Resolve on the node's registries) and the brief's pods, each a
// sidecar health-checked before the session starts; a required one that
// is not ready hands the lease back cannot_run, never a session without
// its database. The node tools' suites (run_suite) run in the same pod,
// beside them.
//
// The relay is reached from inside the pod only through the lease's
// socket, whose directory alone is mounted (sandbox.Session.ShareProxyDir,
// as the legacy node mounts its sessionproxy's): the product's MCP
// servers through the node image's proxy shim, vendor APIs at
// /latchkey/<vendor>/ on $MESH_RELAY_SOCKET, and git and the model
// through a loopback bridge in the agent's own container
// (gitBridgeScript), since neither git nor the harness speaks to a unix
// socket: the checkout's origin is rewritten onto the bridge's /git/
// (insteadOf), and the harness's ANTHROPIC_BASE_URL (OPENAI_BASE_URL)
// names the bridge's /model/<provider>/, the lease's model relay
// (modelrelay.go), holding only its dummy. No credential is in the pod.

// Labels a lease's pod carries beside PodLeaseLabel.
const (
	PodJobLabel  = "mesh.job"
	PodNodeLabel = "mesh.node"
)

// podGitPort is the loopback port, inside a lease's pod, of its git
// bridge: the pod's own network namespace, so every pod may use it.
const podGitPort = 47419

// gitBridgeName is the bridge's script, written in the session's HOME.
const gitBridgeName = "mesh-git-bridge.js"

// leasePod is a lease's sandbox session: its pod, the services started
// in it, and how its session's process runs there.
type leasePod struct {
	sess     *sandbox.Session
	name     string
	statuses []sandbox.ServiceStatus
	// shim is the proxy shim's path in the HOME, once written.
	shim string
	// gitPort is the bridge's port in the pod (podGitPort).
	gitPort int
	// proxyEnv points the pod's agent and steps at the relay, the only
	// exit of a pod whose kind declares its egress (egress.go); nil
	// otherwise.
	proxyEnv []string
	// denied hears a step the pod's limit killed, to report (security.go).
	denied func(error)
	// registryEnv names the relay's registry, MESH_REGISTRY, when the
	// brief names one (registry.go).
	registryEnv []string
}

// openLeasePod opens the lease's sandbox session, pinned to its pod name
// and labels and limited to the brief's resources, has the actor (or
// keep) hold it, and starts the checkout's services and the brief's pods
// in it. A required service that is not ready is the error.
func (r *Runtime) openLeasePod(ctx context.Context, b *meshapi.Brief, workspace string, a *actor.Actor, keep func(string, func() error)) (*leasePod, error) {
	sb := r.Pods.WithReserve(nil) // the scheduler holds the lease's CPUs
	sb.TempDir = r.leaseDir(b.LeaseID)
	switch {
	case r.sidecarMode():
		// the relay is a sidecar in the pod, reached on its loopback,
		// and reaches the node by a socket: the pod needs no network
		// at all (relaypod.go)
		sb.Config.Network = sandbox.Isolated()
	case b.Egress != nil:
		// the relay is the pod's only exit: no port out, no DNS
		sb.Config.Network = sb.Config.Network.RelayOnly()
	}
	if err := os.MkdirAll(sb.TempDir, 0o700); err != nil {
		return nil, err
	}
	name, email := podIdentity(b, r.Node.Name)
	sess, err := sb.Open(b.LeaseID, b.Kind, sandbox.GitIdentity{Name: name, Email: email})
	if err != nil {
		return nil, err
	}
	p := &leasePod{sess: sess, name: PodName(b.LeaseID), gitPort: cmp.Or(r.gitBridgePort, podGitPort),
		denied: r.sandboxReporter(b.LeaseID)}
	if b.Egress != nil || r.sidecarMode() {
		p.proxyEnv = proxyEnv(p.bridgeBase())
	}
	p.registryEnv = registryEnv(b, p.gitPort)
	sess.Pin(p.name, map[string]string{PodLeaseLabel: b.LeaseID, PodJobLabel: b.JobID, PodNodeLabel: r.NodeID()})
	sess.SetLimits(limitsOf(b.Resources))
	closeSess := func() error {
		if err := sess.Close(); err != nil {
			r.logf("nodev1: lease %s: %s", b.LeaseID, sandbox.CloseLine(b.LeaseID, err))
		}
		return nil
	}
	if a != nil {
		a.HoldPod(p.name, closeSess)
	} else {
		keep("pod "+p.name, closeSess)
	}
	svcs, err := r.leaseServices(b, workspace)
	if err != nil {
		return p, err
	}
	timeout := r.Pods.Config.ServiceTimeout
	if p.statuses, err = sess.Start(ctx, svcs, timeout); err != nil {
		return p, fmt.Errorf("starting the lease's pod: %w", err)
	}
	var missing []string
	for _, st := range p.statuses {
		if st.Ready {
			r.logf("nodev1: lease %s: service %s ready at %s", b.LeaseID, st.Describe(), st.Addrs())
			continue
		}
		r.logf("nodev1: lease %s: service %s: %s", b.LeaseID, st.Describe(), st.Why)
		if st.Required {
			missing = append(missing, st.Name+" ("+st.Why+")")
		}
	}
	if len(missing) > 0 {
		return p, fmt.Errorf("the lease's required services are not ready: %s", strings.Join(missing, "; "))
	}
	if list := sandbox.ServicesListing(p.statuses); len(list) > 0 {
		if _, err := sess.WriteFile(sandbox.ServicesFileName, list); err != nil {
			return p, err
		}
	}
	return p, nil
}

// leaseServices are the checkout's declared services and the brief's
// pods, each required.
func (r *Runtime) leaseServices(b *meshapi.Brief, workspace string) ([]sandbox.Service, error) {
	resolved, err := sandbox.Resolve(workspace, r.Pods.Config.Registries)
	if err != nil {
		return nil, err
	}
	for _, n := range resolved.Notes {
		r.logf("nodev1: lease %s: services: %s", b.LeaseID, n)
	}
	out := resolved.Services
	for i, raw := range b.Pods {
		var p podSpec
		if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" || p.Image == "" {
			return nil, fmt.Errorf("the brief's pod %d is not {name, image, env?, command?}: %s", i, raw)
		}
		svc := sandbox.Service{Name: p.Name, Image: p.Image, Registry: sandbox.Registry(p.Image), Command: p.Command, Required: true, Env: map[string]string{}}
		for _, kv := range p.Env {
			k, v, _ := strings.Cut(kv, "=")
			svc.Env[k] = v
		}
		allowed := r.Pods.Config.Registries
		if len(allowed) == 0 {
			allowed = sandbox.DefaultRegistries
		}
		svc.Refused = !sandbox.RegistryAllowed(svc.Registry, allowed)
		out = append(out, svc)
	}
	return out, nil
}

// limitsOf is the agent's share of what the scheduler leased the lease:
// the brief's CPUs and memory; a zero leaves the sandbox's own.
func limitsOf(res meshapi.Resources) sandbox.Limits {
	var l sandbox.Limits
	if res.CPUs > 0 {
		l.CPUs = res.CPUs
	}
	if res.MemGB > 0 {
		l.Memory = strconv.FormatInt(int64(math.Ceil(res.MemGB*1024)), 10) + "m"
	}
	return l
}

// podIdentity is the git identity the lease's commits in the pod carry:
// the brief's apply_as when it names one, else the node's agent
// identity — never the node's owner (ADR-024's shape).
func podIdentity(b *meshapi.Brief, node string) (string, string) {
	if b.Code != nil && b.Code.ApplyAs != nil && b.Code.ApplyAs.Name != "" && b.Code.ApplyAs.Email != "" {
		return b.Code.ApplyAs.Name, b.Code.ApplyAs.Email
	}
	slug := strings.Trim(unsafeSlug.ReplaceAllString(strings.ToLower(node), "-"), "-")
	if slug == "" {
		slug = "node"
	}
	return fmt.Sprintf("Mesh node (%s)", slug), fmt.Sprintf("mesh-node+%s@runsheet.dev", slug)
}

var unsafeSlug = regexp.MustCompile(`[^a-z0-9-]+`)

// podServers are servers as the pod's session reaches them: each
// sessionproxy entry (harness.ProxyRelay, the host's `mesh mcp-relay`)
// rewritten onto the node image's proxy shim, written into the HOME,
// and the socket's directory mounted.
func (p *leasePod) podServers(servers map[string]any, sockDir string) (map[string]any, error) {
	if sockDir != "" {
		// a relay sidecar's servers are on its loopback: nothing is
		// mounted
		p.sess.ShareProxyDir(sockDir)
	}
	if len(servers) == 0 {
		return servers, nil
	}
	if p.shim == "" {
		shim, err := p.sess.WriteFile(harness.ProxyShimScriptName, harness.ProxyShimScript())
		if err != nil {
			return nil, fmt.Errorf("the pod's proxy shim: %w", err)
		}
		p.shim = shim
	}
	out := make(map[string]any, len(servers))
	for name, srv := range servers {
		socket, server, ok := harness.ProxiedOf(srv)
		if !ok {
			out[name] = srv
			continue
		}
		out[name] = harness.ProxiedServer(p.shim, socket, server)
	}
	return out, nil
}

// run is the lease's PodRun: the session's process in the pod, as its
// agent. The session's variables the pod's allow-list admits (the
// model's credential, the commit identity) go in as the job's; the rest
// are the node's own (the relay socket's path, git's settings) and go
// past it. With a git grant or a relayed model account the agent runs
// under the bridge; with a grant the checkout's origin — the grant's
// loopback capability on the host, which the pod cannot reach — is
// rewritten onto it, and the model's base URL already names it
// (sessionAccount.podEnv).
func (p *leasePod) run(workspace, gitBase, sock string, model bool) (PodRun, error) {
	var bridge string
	if gitBase != "" || model || p.proxyEnv != nil || p.registryEnv != nil {
		path, err := p.sess.WriteFile(gitBridgeName, gitBridgeScript())
		if err != nil {
			return nil, fmt.Errorf("the pod's git bridge: %w", err)
		}
		bridge = path
	}
	return func(ctx context.Context, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
		var jobEnv, vars []string
		for _, kv := range env {
			name, _, _ := strings.Cut(kv, "=")
			if sandbox.Allowed(name) {
				jobEnv = append(jobEnv, kv)
			} else {
				vars = append(vars, kv)
			}
		}
		cmd := argv
		if bridge != "" {
			cmd = append([]string{"node", bridge, sock, strconv.Itoa(p.gitPort), "--"}, argv...)
			if gitBase != "" {
				vars = append(vars, bridgeGitConfig(gitBase, p.gitPort)...)
			}
			vars = append(vars, p.proxyEnv...)
			vars = append(vars, p.registryEnv...)
		}
		return p.sess.RunAgent(ctx, sandbox.RunSpec{Workspace: workspace, Env: jobEnv, Vars: vars, Command: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr})
	}, nil
}

// stopHookName is the pod's Stop hook shim, written in the session's
// HOME.
const stopHookName = "mesh-stop-hook.js"

// stopSettings are the pod session's --settings hooks: with a stop gate,
// a Stop hook the pod can run — the node image's node on
// stopHookScript, written in the HOME, asking the gate on its socket,
// which the pod has at its own path in the mounted socket directory
// (podServers shares it). nil without a gate.
func (p *leasePod) stopSettings(g *stopGate) (map[string]any, error) {
	if g == nil {
		return nil, nil
	}
	shim, err := p.sess.WriteFile(stopHookName, stopHookScript())
	if err != nil {
		return nil, fmt.Errorf("the pod's stop hook: %w", err)
	}
	return stopHookSettings("node " + shellQuote(shim) + " " + shellQuote(g.path)), nil
}

// stopHookScript is RunStopHook for a pod (node, in the node image):
// claude's Stop input on stdin is posted to the gate on the socket in
// argv[2], and a refusal ({"decision":"block","reason":…}) is written to
// stdout for claude to read. Anything that goes wrong lets the session
// stop — exit 0, nothing written — since the node checks the finish
// again once the session has ended.
func stopHookScript() []byte {
	return []byte(`// mesh: the lease's Stop hook; written per lease.
"use strict";
const http = require("http");
const target = process.argv[2];
// the gate's socket, or the relay sidecar's loopback root
// (http://127.0.0.1:<port>), which forwards /stop to the node
const conn = target.startsWith("http://")
  ? (() => { const u = new URL(target); return { host: u.hostname, port: Number(u.port) }; })()
  : { socketPath: target };
const done = () => process.exit(0);
setTimeout(done, ` + strconv.Itoa((stopHookTimeoutS-5)*1000) + `).unref();
const chunks = [];
process.stdin.on("data", (c) => chunks.push(c));
process.stdin.on("error", done);
process.stdin.on("end", () => {
  const body = Buffer.concat(chunks).subarray(0, 65536);
  const req = http.request({ ...conn, path: "/stop", method: "POST",
    headers: { "Content-Type": "application/json", "Content-Length": body.length } }, (res) => {
    const out = [];
    res.on("data", (c) => out.push(c));
    res.on("error", done);
    res.on("end", () => {
      try {
        const a = JSON.parse(Buffer.concat(out).toString());
        if (res.statusCode === 200 && a.decision === "block" && a.reason) {
          process.stdout.write(JSON.stringify({ decision: "block", reason: a.reason }) + "\n");
        }
      } catch (e) {}
      done();
    });
  });
  req.on("error", (e) => { process.stderr.write("mesh stop hook: " + e.message + "; letting the session stop\n"); done(); });
  req.end(body);
});
`)
}

// bridgeBase is the bridge's address inside the pod: the lease's socket
// at the pod's loopback, where the session reaches the model relay's
// /model/ route (and git's /git/).
func (p *leasePod) bridgeBase() string { return "http://127.0.0.1:" + strconv.Itoa(p.gitPort) }

// bridgeGitConfig points every git command in the pod at the bridge for
// the grant's base: git's own environment configuration, which neither
// the checkout's config nor the HOME's carries.
func bridgeGitConfig(base string, port int) []string {
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.http://127.0.0.1:" + strconv.Itoa(port) + "/git/.insteadOf",
		"GIT_CONFIG_VALUE_0=" + base,
		"GIT_TERMINAL_PROMPT=0",
	}
}

// suites is run_suite's runner in the lease's pod: each step a short
// container beside the agent and the services (sandbox.Session.Exec),
// in the checkout. A limit's kill is the step failing, 124 for the
// runtime's timeout and 137 otherwise, as the legacy node's pod runner.
func (p *leasePod) suites(ctx context.Context, workspace string, steps []validate.Step, env []string) ([]validate.StepResult, error) {
	return validate.Run(ctx, steps, podSteps{p: p, dir: workspace, env: env}, validate.Options{Dir: workspace, ContinueOnFailure: true})
}

// podSteps is validate.Runner in the lease's pod; env is the run's
// selection (nodetools.SelectionEnv).
type podSteps struct {
	p   *leasePod
	dir string
	env []string
}

func (s podSteps) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	err := s.p.sess.Exec(ctx, sandbox.RunSpec{Workspace: s.dir, Command: []string{"sh", "-c", cmd}, Timeout: timeout, Stdout: stdout, Stderr: stderr, Vars: append(append(append([]string{"GIT_TERMINAL_PROMPT=0"}, s.p.proxyEnv...), s.p.registryEnv...), s.env...)})
	var ee *sandbox.ExitError
	var le *sandbox.LimitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.Code, nil
	case errors.As(err, &le):
		fmt.Fprintf(stderr, "\nsandbox: %v\n", le)
		if s.p.denied != nil {
			s.p.denied(err)
		}
		if le.Limit == "timeout" {
			return 124, nil
		}
		return 137, nil
	}
	return -1, err
}

// gitBridgeScript is the git bridge (node, in the node image): it
// listens on 127.0.0.1:<port> inside the pod and pipes each connection
// to the lease's socket, whose sessionproxy serves the grant at /git/
// and the model relay at /model/,
// then runs the command after "--" with its own stdio and exits as it
// does — the signal's 128+n when one ended it — passing on the signals
// it is sent. It carries no credential: the relay adds the token on the
// host.
func gitBridgeScript() []byte {
	return []byte(`// mesh: the lease's git bridge; written per lease.
"use strict";
const net = require("net");
const os = require("os");
const { spawn } = require("child_process");
const socketPath = process.argv[2];
const port = Number(process.argv[3]);
const cmd = process.argv.slice(process.argv.indexOf("--") + 1);

const server = net.createServer((c) => {
  const u = net.connect(socketPath);
  c.pipe(u);
  u.pipe(c);
  c.on("error", () => u.destroy());
  u.on("error", () => c.destroy());
});
server.on("error", (e) => {
  process.stderr.write("mesh git bridge: " + e.message + "\n");
  process.exit(1);
});
server.listen(port, "127.0.0.1", () => {
  const child = spawn(cmd[0], cmd.slice(1), { stdio: "inherit" });
  for (const sig of ["SIGTERM", "SIGINT", "SIGHUP"]) {
    process.on(sig, () => child.kill(sig));
  }
  child.on("error", (e) => {
    process.stderr.write("mesh git bridge: " + cmd[0] + ": " + e.message + "\n");
    process.exit(127);
  });
  child.on("exit", (code, signal) => {
    if (signal) {
      process.exit(128 + (os.constants.signals[signal] || 1));
    }
    process.exit(code);
  });
});
`)
}

// runSidecar is the lease's PodRun when its relay is a sidecar in the
// pod: the agent's command as it is, no bridge, since the sidecar
// listens on the pod's loopback at the bridge's port. The checkout's
// origin is rewritten onto the sidecar's /git/, every HTTP client is
// pointed at it (proxyEnv), and the vendor API's root is named in
// RelayURLEnv.
func (p *leasePod) runSidecar(workspace, gitBase string) PodRun {
	return func(ctx context.Context, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
		var jobEnv, vars []string
		for _, kv := range env {
			name, _, _ := strings.Cut(kv, "=")
			if sandbox.Allowed(name) {
				jobEnv = append(jobEnv, kv)
			} else {
				vars = append(vars, kv)
			}
		}
		if gitBase != "" {
			vars = append(vars, bridgeGitConfig(gitBase, p.gitPort)...)
		}
		vars = append(vars, p.proxyEnv...)
		vars = append(vars, p.registryEnv...)
		vars = append(vars, RelayURLEnv+"="+p.bridgeBase())
		return p.sess.RunAgent(ctx, sandbox.RunSpec{Workspace: workspace, Env: jobEnv, Vars: vars, Command: argv, Stdin: stdin, Stdout: stdout, Stderr: stderr})
	}
}

// stopSettingsSidecar are the Stop hook's settings when the relay is a
// sidecar: the hook posts to the sidecar's loopback, which forwards it
// to the node's gate.
func (p *leasePod) stopSettingsSidecar(g *stopGate) (map[string]any, error) {
	if g == nil {
		return nil, nil
	}
	shim, err := p.sess.WriteFile(stopHookName, stopHookScript())
	if err != nil {
		return nil, fmt.Errorf("the pod's stop hook: %w", err)
	}
	return stopHookSettings("node " + shellQuote(shim) + " " + shellQuote(p.bridgeBase())), nil
}
