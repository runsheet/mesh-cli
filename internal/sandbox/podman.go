package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

// Podman is the Runtime of ADR 007: rootless pods, one `podman` call per
// step, run as the node's user with no daemon.
type Podman struct {
	// Binary is the podman executable ("podman" on PATH by default).
	Binary string
	// Exec runs podman with args (tests stub it); nil runs the binary.
	// env is the process's whole environment.
	Exec func(ctx context.Context, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error
}

func (p *Podman) bin() string {
	if p.Binary != "" {
		return p.Binary
	}
	return "podman"
}

func (p *Podman) run(ctx context.Context, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if p.Exec != nil {
		return p.Exec(ctx, args, env, stdin, stdout, stderr)
	}
	cmd := exec.CommandContext(ctx, p.bin(), args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// runner is a Runner over the Podman's own run path (Exec when tests
// stub it), for the image helpers: name is always podman, so only the
// args are passed on.
func (p *Podman) runner(ctx context.Context, _ string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	err := p.run(ctx, args, podmanEnv(os.Environ(), nil), nil, &out, &out)
	return out.Bytes(), err
}

// output runs a short podman command and returns its stdout, with its
// stderr in the error.
func (p *Podman) output(ctx context.Context, args []string, stdin io.Reader) (string, error) {
	var out, errb bytes.Buffer
	if err := p.run(ctx, args, podmanEnv(os.Environ(), nil), stdin, &out, &errb); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return "", fmt.Errorf("podman %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("podman %s: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

// Version implements Runtime: `podman --version`.
func (p *Podman) Version(ctx context.Context) (string, error) {
	out, err := p.output(ctx, []string{"--version"}, nil)
	if err != nil {
		return "", err
	}
	// "podman version 5.2.1"
	return strings.Replace(out, " version ", " ", 1), nil
}

// PodCreateArgs is `podman pod create` for spec: rootless with the
// host user mapped onto the image's user (spec.UID:GID, KeepIDUserns),
// so the HOME, workspace and context the node writes are the agent's
// own inside, on pasta with no route to the host's loopback or
// gateway (no --map-gw, no host.containers.internal), DNS through the
// forwarder the rules allow.
func PodCreateArgs(spec PodSpec) []string {
	args := []string{"pod", "create", "--name", spec.Name, "--userns", KeepIDUserns(spec.UID, spec.GID),
		"--network", "pasta:--no-map-gw,--dns-forward," + DNSForward,
		"--dns", DNSForward, "--no-hosts"}
	keys := make([]string, 0, len(spec.Labels))
	for k := range spec.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}
	return args
}

// KeepIDUserns is the --userns value that maps the host user onto the
// given uid:gid inside the pod. A zero uid or gid is an image that
// declares no user (ImageUser found none to read) and falls back to
// DefaultImageUID, so the pod never maps onto root inside.
func KeepIDUserns(uid, gid int) string {
	if uid == 0 {
		uid = DefaultImageUID
	}
	if gid == 0 {
		gid = DefaultImageUID
	}
	return fmt.Sprintf("keep-id:uid=%d,gid=%d", uid, gid)
}

// NetworkInitArgs is the init container that loads the network rules
// (on stdin) into the pod's network namespace: NET_ADMIN in the pod's
// user namespace only, and gone once they are loaded.
func NetworkInitArgs(pod Pod, image string) []string {
	return []string{"run", "--rm", "-i", "--pod", pod.Name, "--name", pod.Name + "-net",
		"--user", "0:0", "--cap-drop", "all", "--cap-add", "NET_ADMIN",
		"--security-opt", "no-new-privileges", "--pull", "missing",
		image, "nft", "-f", "-"}
}

// AgentRunArgs is `podman run` for the agent's container. Every
// variable — the limit variables (LimitEnv) included — is passed by name
// (`--env NAME`) and its value put in podman's own environment
// (AgentProcessEnv), HOME and PATH as much as a secret, so no value is
// on a command line. --unsetenv-all drops the image's and podman's
// defaults.
func AgentRunArgs(pod Pod, spec AgentSpec) []string {
	args := agentArgs("-i", pod, spec, spec.Mounts)
	args = append(args, spec.Image)
	return append(args, spec.Command...)
}

// StartAgentArgs is `podman run -d` for a detached agent (StartAgent):
// AgentRunArgs' flags — the pod, limits, --timeout, mounts, env by
// name — with -d for -i, the directory of each stream file mounted
// read-write at its own path unless a mount already holds it, and the
// command run by `sh -c` with its stdin, stdout and stderr redirected
// to those files. The shell execs the command, so the container's exit
// is the agent's. The streams' paths are on the command line, never a
// secret.
func StartAgentArgs(pod Pod, spec AgentSpec) []string {
	mounts := append([]Mount(nil), spec.Mounts...)
	for _, f := range []string{spec.StdinFile, spec.StdoutFile, spec.StderrFile} {
		if f == "" {
			continue
		}
		dir := filepath.Dir(f)
		if !mountsHold(mounts, dir) {
			mounts = append(mounts, Mount{Source: dir, Target: dir})
		}
	}
	args := agentArgs("-d", pod, spec, mounts)
	args = append(args, spec.Image, "/bin/sh", "-c", detachedScript(spec), "mesh-agent")
	return append(args, spec.Command...)
}

// detachedScript execs "$@" with its streams on the spec's files.
func detachedScript(spec AgentSpec) string {
	file := func(f string) string {
		if f == "" {
			return "/dev/null"
		}
		return shellQuote(f)
	}
	return `exec "$@" <` + file(spec.StdinFile) + ` >` + file(spec.StdoutFile) + ` 2>` + file(spec.StderrFile)
}

// shellQuote quotes s for sh as one word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// mountsHold says whether dir is writable in the pod: the deepest mount
// whose target is dir or holds it is read-write.
func mountsHold(mounts []Mount, dir string) bool {
	dir = filepath.Clean(dir)
	best, held := -1, false
	for _, m := range mounts {
		t := filepath.Clean(m.Target)
		if dir == t || strings.HasPrefix(dir, t+string(filepath.Separator)) {
			if d := mountDepth(m); d > best {
				best, held = d, !m.ReadOnly
			}
		}
	}
	return held
}

// mountDepth is how many path elements a mount's target has.
func mountDepth(m Mount) int {
	return strings.Count(filepath.Clean(m.Target), string(filepath.Separator))
}

// agentArgs are the agent's `podman run` flags up to its image: mode is
// -i (attached) or -d (detached).
func agentArgs(mode string, pod Pod, spec AgentSpec, mounts []Mount) []string {
	args := []string{"run", mode, "--pod", pod.Name, "--name", spec.Name,
		"--cap-drop", "all", "--security-opt", "no-new-privileges",
		"--pull", "missing", "--unsetenv-all"}
	args = append(args, LimitArgs(spec.Limits)...)
	if spec.TimeoutSeconds > 0 {
		args = append(args, "--timeout", strconv.Itoa(spec.TimeoutSeconds))
	}
	// A mount nested in another (a worktree's own entry inside the
	// read-only .git/worktrees inside the common dir) must come after
	// it so the deeper one wins: shallowest target first, else as given.
	mounts = append([]Mount(nil), mounts...)
	sort.SliceStable(mounts, func(i, j int) bool { return mountDepth(mounts[i]) < mountDepth(mounts[j]) })
	for _, m := range mounts {
		opt := "rw"
		if m.ReadOnly {
			opt = "ro"
		}
		args = append(args, "--volume", m.Source+":"+m.Target+":"+opt)
	}
	if spec.Workdir != "" {
		args = append(args, "--workdir", spec.Workdir)
	}
	return append(args, envArgs(ContainerEnv(spec.Limits, spec.Env))...)
}

// SidecarName is the container name of a service in a pod.
func SidecarName(pod Pod, service string) string {
	return pod.Name + "-" + strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(service), "-"), "-")
}

// SidecarRunArgs is `podman run -d` for a service in the pod: nothing
// mounted from the host, its env by name as the agent's is. --pull
// missing keeps a pulled image in the node user's store, so the next
// session's is not pulled again.
func SidecarRunArgs(pod Pod, spec SidecarSpec) []string {
	args := []string{"run", "-d", "--pod", pod.Name, "--name", SidecarName(pod, spec.Name),
		"--security-opt", "no-new-privileges", "--pull", "missing"}
	args = append(args, LimitArgs(spec.Limits)...)
	args = append(args, envArgs(ContainerEnv(spec.Limits, spec.Env))...)
	args = append(args, spec.Image)
	return append(args, spec.Command...)
}

// LimitArgs are --cpus, --memory (swap the same, so memory is a hard
// cap) and --pids-limit, each only when set.
func LimitArgs(l Limits) []string {
	var args []string
	if l.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(l.CPUs, 'f', -1, 64))
	}
	if l.Memory != "" {
		args = append(args, "--memory", l.Memory, "--memory-swap", l.Memory)
	}
	if l.Pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(l.Pids))
	}
	return args
}

// LimitEnv tells the tools inside the container about --cpus, so Go,
// OpenMP and make fan out to the limit rather than the host's own core
// count: GOMAXPROCS, OMP_NUM_THREADS (GNU nproc honours it) and
// MAKEFLAGS=-j<n>, all at ceil(l.CPUs), at least 1; none when CPUs is
// not set.
func LimitEnv(l Limits) []string {
	if l.CPUs <= 0 {
		return nil
	}
	n := int(math.Ceil(l.CPUs))
	if n < 1 {
		n = 1
	}
	v := strconv.Itoa(n)
	return []string{"GOMAXPROCS=" + v, "OMP_NUM_THREADS=" + v, "MAKEFLAGS=-j" + v}
}

// ContainerEnv is a container's whole environment: env, then those of
// LimitEnv(l)'s variables env does not set itself — the item's or repo's
// own value wins over the derived one. Every one of them is passed by
// name (envArgs), its value in podman's own environment.
func ContainerEnv(l Limits, env []string) []string {
	derived := LimitEnv(l)
	if len(derived) == 0 {
		return env
	}
	set := make(map[string]bool, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		set[name] = true
	}
	out := append([]string(nil), env...)
	for _, kv := range derived {
		name, _, _ := strings.Cut(kv, "=")
		if !set[name] {
			out = append(out, kv)
		}
	}
	return out
}

// AgentProcessEnv is podman's own process environment for running spec:
// the node's passthrough variables, then the values of every container
// variable (ContainerEnv: the limit variables included) its `--env NAME`
// reads.
func AgentProcessEnv(environ []string, spec AgentSpec) []string {
	return podmanEnv(environ, ContainerEnv(spec.Limits, spec.Env))
}

// podmanPassthrough are the node's variables podman itself needs: its
// storage, runtime directory and config (podmanStorage), and PATH, HOME,
// USER, LOGNAME and TMPDIR. The container sees none of the node's values.
var podmanPassthrough = []string{"PATH", "HOME", "USER", "LOGNAME", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CONTAINERS_CONF", "CONTAINERS_STORAGE_CONF", "TMPDIR"}

// podmanStorage are the passthrough variables rootless podman finds its
// store, runtime directory and config by: always the node's, never a
// container's, so a container cannot carry one.
var podmanStorage = []string{"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CONTAINERS_CONF", "CONTAINERS_STORAGE_CONF"}

func isPassthrough(name string) bool { return slices.Contains(podmanPassthrough, name) }

func isStorage(name string) bool { return slices.Contains(podmanStorage, name) }

// envArgs are the --env arguments for a container's variables: a bare
// `--env NAME` each — HOME and PATH included — its value read from
// podman's environment (podmanEnv), so no value is on the command line.
// A podmanStorage name is not passed: it is podman's own.
func envArgs(env []string) []string {
	var args []string
	seen := map[string]bool{}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if isStorage(name) || seen[name] {
			continue
		}
		seen[name] = true
		args = append(args, "--env", name)
	}
	return args
}

// podmanEnv is podman's own process environment: what it needs from the
// node's, then the values of the container's variables, which
// `--env NAME` reads from here, a container's value replacing the
// node's of the same name (HOME, PATH). Podman's store stays the node's
// whatever the container's HOME: the podmanStorage variables are always
// the node's, and XDG_DATA_HOME and XDG_CONFIG_HOME — where rootless
// podman would otherwise look under HOME for its storage and config —
// are pinned to the node's HOME's when the node leaves them unset (a
// session's temporary HOME once sent podman to an empty store and a pull
// it could not make: the dl360, 2026-10-10).
func podmanEnv(environ, container []string) []string {
	vals := map[string]string{}
	var order []string
	set := func(name, v string) {
		if _, ok := vals[name]; !ok {
			order = append(order, name)
		}
		vals[name] = v
	}
	for _, kv := range environ {
		name, v, _ := strings.Cut(kv, "=")
		if isPassthrough(name) {
			set(name, v)
		}
	}
	if home := vals["HOME"]; home != "" {
		if _, ok := vals["XDG_DATA_HOME"]; !ok {
			set("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
		}
		if _, ok := vals["XDG_CONFIG_HOME"]; !ok {
			set("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		}
	}
	for _, kv := range container {
		name, v, _ := strings.Cut(kv, "=")
		if !isStorage(name) {
			set(name, v)
		}
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		out = append(out, name+"="+vals[name])
	}
	return out
}

// CreatePod implements Runtime: the pod, then its network rules. A pod
// whose rules will not load is removed and not used.
// The pod maps the host user onto the image's declared user, read by
// ImageUser (`podman image inspect --format {{.Config.User}}`) when the
// spec names none; an inspect that fails falls back to DefaultImageUID
// and the pod is made anyway.
//
// A failure of either call is the runtime's own, not the session's: a
// `pod create` or the network-rules init container refused for the
// rootless store or the user namespace (validate.IsRuntimeBroken) comes
// back as a *RuntimeError.
func (p *Podman) CreatePod(ctx context.Context, spec PodSpec) (Pod, error) {
	if spec.UID == 0 && spec.GID == 0 {
		uid, gid, err := ImageUser(ctx, p.runner, spec.Image)
		if err != nil {
			uid, gid = DefaultImageUID, DefaultImageUID
		}
		spec.UID, spec.GID = uid, gid
	}
	if _, err := p.output(ctx, PodCreateArgs(spec), nil); err != nil {
		if validate.IsRuntimeBroken(err.Error()) {
			return Pod{}, &RuntimeError{Op: "pod create", Err: err}
		}
		return Pod{}, err
	}
	pod := Pod{Name: spec.Name}
	if _, err := p.output(ctx, NetworkInitArgs(pod, spec.Image), strings.NewReader(spec.Network.Ruleset())); err != nil {
		_ = p.Remove(context.WithoutCancel(ctx), pod)
		if validate.IsRuntimeBroken(err.Error()) {
			return Pod{}, &RuntimeError{Op: "network rules", Err: err}
		}
		return Pod{}, fmt.Errorf("network rules: %w", err)
	}
	return pod, nil
}

// podmanExitFailedToRun is the exit podman itself ends with when it
// never ran the agent's command at all — the same 125 validate's own
// podmanExit names for a step.
const podmanExitFailedToRun = 125

// RunAgent implements Runtime. The container is left for Remove, so its
// state can be read: an exit of 137 asks whether it was the OOM killer.
//
// The run's own stderr (its last 8KB, as sandbox.go's own tail) is kept
// to classify a failure, but only when podman itself is the one that
// failed: its own exit of 125, or no run at all (no *exec.ExitError).
// Then, and only then, its words for a rootless store or user namespace
// it could not open (validate.IsRuntimeBroken) make it the runtime's
// own failure, a *RuntimeError, never the agent's. An agent command
// that exits on its own — 125 among them, podman's or not — is read
// from its exit code alone: its stderr is never searched for runtime
// words, so an agent's own non-zero exit is never mistaken for the
// runtime breaking under it.
func (p *Podman) RunAgent(ctx context.Context, pod Pod, spec AgentSpec) (Exit, error) {
	tail := &tailBuffer{max: 8 << 10}
	stderr := io.Writer(tail)
	if spec.Stderr != nil {
		stderr = io.MultiWriter(spec.Stderr, tail)
	}
	// podman's own env keeps the node's XDG_* for its storage; the
	// container's HOME and PATH are read from it by --env NAME
	err := p.run(ctx, AgentRunArgs(pod, spec), AgentProcessEnv(os.Environ(), spec), spec.Stdin, spec.Stdout, stderr)
	if err == nil {
		return Exit{}, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return Exit{}, &RuntimeError{Op: "agent run", Err: err}
	}
	exit := Exit{Code: ee.ExitCode()}
	if exit.Code == podmanExitFailedToRun && validate.IsRuntimeBroken(tail.String()) {
		return Exit{}, &RuntimeError{Op: "agent run", Err: errors.New(strings.TrimSpace(tail.String()))}
	}
	if exit.Code == 137 {
		if out, ierr := p.output(context.WithoutCancel(ctx), []string{"inspect", "--format", "{{.State.OOMKilled}}", spec.Name}, nil); ierr == nil {
			exit.OOMKilled = out == "true"
		}
	}
	return exit, nil
}

// StartAgent implements Runtime: `podman run -d` (StartAgentArgs), with
// podman's own env as RunAgent's — the node's XDG_* for its store,
// the container's values for its `--env NAME`s. It returns once podman
// has started the container.
//
// A refusal for the rootless store or the user namespace
// (validate.IsRuntimeBroken) is the runtime's own failure, a
// *RuntimeError, never the agent's.
func (p *Podman) StartAgent(ctx context.Context, pod Pod, spec AgentSpec) error {
	var errb bytes.Buffer
	if err := p.run(ctx, StartAgentArgs(pod, spec), AgentProcessEnv(os.Environ(), spec), nil, io.Discard, &errb); err != nil {
		msg := strings.TrimSpace(errb.String())
		if validate.IsRuntimeBroken(msg) {
			return &RuntimeError{Op: "agent start", Err: errors.New(msg)}
		}
		if msg != "" {
			return fmt.Errorf("podman run %s: %s", spec.Name, msg)
		}
		return fmt.Errorf("podman run %s: %w", spec.Name, err)
	}
	return nil
}

// AgentStateArgs reads one container's record as JSON.
func AgentStateArgs(name string) []string {
	return []string{"container", "inspect", "--format", "json", name}
}

// StopAgentArgs stops one container, SIGKILL after 10 seconds.
func StopAgentArgs(name string) []string {
	return []string{"stop", "--time", "10", name}
}

// ParseAgentState reads `podman container inspect --format json` of one
// container: its State's Status, Running, ExitCode and OOMKilled. An
// empty list is ErrNoAgent.
func ParseAgentState(out string) (AgentState, error) {
	var rows []struct {
		State struct {
			Status    string `json:"Status"`
			Running   bool   `json:"Running"`
			ExitCode  int    `json:"ExitCode"`
			OOMKilled bool   `json:"OOMKilled"`
		} `json:"State"`
	}
	if strings.TrimSpace(out) == "" {
		return AgentState{}, ErrNoAgent
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return AgentState{}, fmt.Errorf("podman container inspect: %w", err)
	}
	if len(rows) == 0 {
		return AgentState{}, ErrNoAgent
	}
	st := rows[0].State
	return AgentState{Status: st.Status, Running: st.Running,
		Exit: Exit{Code: st.ExitCode, OOMKilled: st.OOMKilled}}, nil
}

// noSuchContainer is podman's word for a name it does not have.
var noSuchContainer = regexp.MustCompile(`(?i)no such (container|object)`)

// AgentState implements Runtime.
func (p *Podman) AgentState(ctx context.Context, name string) (AgentState, error) {
	out, err := p.output(ctx, AgentStateArgs(name), nil)
	if err != nil {
		if noSuchContainer.MatchString(err.Error()) {
			return AgentState{}, fmt.Errorf("%w: %s", ErrNoAgent, name)
		}
		return AgentState{}, err
	}
	return ParseAgentState(out)
}

// StopAgent implements Runtime. A container already gone is no error.
func (p *Podman) StopAgent(ctx context.Context, name string) error {
	if _, err := p.output(ctx, StopAgentArgs(name), nil); err != nil && !noSuchContainer.MatchString(err.Error()) {
		return err
	}
	return nil
}

// AddSidecar implements Runtime.
func (p *Podman) AddSidecar(ctx context.Context, pod Pod, spec SidecarSpec) error {
	var errb bytes.Buffer
	if err := p.run(ctx, SidecarRunArgs(pod, spec), podmanEnv(os.Environ(), ContainerEnv(spec.Limits, spec.Env)), nil, io.Discard, &errb); err != nil {
		return fmt.Errorf("podman run %s: %s", spec.Name, strings.TrimSpace(errb.String()))
	}
	return nil
}

// ExecSidecar implements Runtime: `podman exec` in the service's
// container, which has the service's own env.
func (p *Podman) ExecSidecar(ctx context.Context, pod Pod, sidecar string, cmd []string) error {
	_, err := p.output(ctx, append([]string{"exec", SidecarName(pod, sidecar)}, cmd...), nil)
	return err
}

// ProbeTCPArgs is the short-lived container that connects to
// 127.0.0.1:port in the pod's network (bash's /dev/tcp, from the node
// image), with every capability dropped.
func ProbeTCPArgs(pod Pod, image string, port int) []string {
	return []string{"run", "--rm", "--pod", pod.Name, "--cap-drop", "all",
		"--security-opt", "no-new-privileges", "--pull", "missing", "--unsetenv-all",
		image, "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(port)}
}

// ProbeTCP implements Runtime.
func (p *Podman) ProbeTCP(ctx context.Context, pod Pod, image string, port int) error {
	_, err := p.output(ctx, ProbeTCPArgs(pod, image, port), nil)
	return err
}

// RemoveArgs are the podman calls that remove a pod, in order: first
// the session's own containers in it — the agent's, the sidecars', all
// named `<pod>-…` — with their anonymous volumes (`podman rm
// --volumes`), then the pod itself, forced, with whatever is left (its
// infra container, which podman removes only with the pod).
func RemoveArgs(pod Pod) [][]string {
	return [][]string{
		{"rm", "--force", "--volumes", "--time", "5",
			"--filter", "pod=" + pod.Name, "--filter", "name=^" + regexp.QuoteMeta(pod.Name) + "-"},
		{"pod", "rm", "--force", "--time", "5", pod.Name},
	}
}

// Remove implements Runtime: the pod, all its containers and their
// anonymous volumes, forced. Every call runs; their errors are joined.
func (p *Podman) Remove(ctx context.Context, pod Pod) error {
	var errs []error
	for _, args := range RemoveArgs(pod) {
		if _, err := p.output(ctx, args, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RemoveTreeArgs is `podman unshare rm -rf <path>`: rm run inside the
// node user's user namespace, as root there, so the subuid-owned and
// read-only files a pod left are its to remove.
func RemoveTreeArgs(path string) []string {
	return []string{"unshare", "rm", "-rf", "--", path}
}

// RemoveTree implements Runtime.
func (p *Podman) RemoveTree(ctx context.Context, path string) error {
	_, err := p.output(ctx, RemoveTreeArgs(path), nil)
	return err
}

// ContainersArgs lists every container, any state, as JSON.
func ContainersArgs() []string { return []string{"ps", "--all", "--format", "json"} }

// InspectArgs reads the named containers' full records (their mounts'
// host sources), as JSON.
func InspectArgs(names []string) []string {
	return append([]string{"container", "inspect", "--format", "json"}, names...)
}

// PodsArgs lists every pod as JSON.
func PodsArgs() []string { return []string{"pod", "ps", "--format", "json"} }

// RemoveContainerArgs removes one container and its anonymous volumes.
func RemoveContainerArgs(name string) []string {
	return []string{"rm", "--force", "--volumes", "--time", "5", name}
}

// psEntry is one row of `podman ps --format json`.
type psEntry struct {
	Names   []string          `json:"Names"`
	PodName string            `json:"PodName"`
	State   string            `json:"State"`
	IsInfra bool              `json:"IsInfra"`
	Labels  map[string]string `json:"Labels"`
}

// inspectEntry is the part of `podman container inspect` Sweep reads.
type inspectEntry struct {
	Name   string `json:"Name"`
	Mounts []struct {
		Source string `json:"Source"`
	} `json:"Mounts"`
}

// ParseContainers reads `podman ps --all --format json` (the mounts are
// filled from the inspect, ParseInspectMounts).
func ParseContainers(out string) ([]Container, error) {
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var rows []psEntry
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("podman ps: %w", err)
	}
	cs := make([]Container, 0, len(rows))
	for _, r := range rows {
		name := ""
		if len(r.Names) > 0 {
			name = r.Names[0]
		}
		cs = append(cs, Container{Name: name, Pod: r.PodName, Labels: r.Labels,
			Running: strings.EqualFold(r.State, "running"), Infra: r.IsInfra})
	}
	return cs, nil
}

// ParseInspectMounts reads `podman container inspect --format json` into
// each container's mounted host paths, by name.
func ParseInspectMounts(out string) (map[string][]string, error) {
	var rows []inspectEntry
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("podman container inspect: %w", err)
	}
	m := map[string][]string{}
	for _, r := range rows {
		for _, mt := range r.Mounts {
			if mt.Source != "" {
				m[strings.TrimPrefix(r.Name, "/")] = append(m[strings.TrimPrefix(r.Name, "/")], mt.Source)
			}
		}
	}
	return m, nil
}

// ParsePods reads `podman pod ps --format json`.
func ParsePods(out string) ([]PodInfo, error) {
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var rows []PodInfo
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("podman pod ps: %w", err)
	}
	return rows, nil
}

// ListContainers implements Runtime: every container, and for the running
// ones the host paths they mount.
func (p *Podman) ListContainers(ctx context.Context) ([]Container, error) {
	out, err := p.output(ctx, ContainersArgs(), nil)
	if err != nil {
		return nil, err
	}
	cs, err := ParseContainers(out)
	if err != nil {
		return nil, err
	}
	var running []string
	for _, c := range cs {
		if c.Running && c.Name != "" {
			running = append(running, c.Name)
		}
	}
	if len(running) == 0 {
		return cs, nil
	}
	out, err = p.output(ctx, InspectArgs(running), nil)
	if err != nil {
		return nil, err
	}
	mounts, err := ParseInspectMounts(out)
	if err != nil {
		return nil, err
	}
	for i := range cs {
		cs[i].Mounts = mounts[cs[i].Name]
	}
	return cs, nil
}

// ListPods implements Runtime.
func (p *Podman) ListPods(ctx context.Context) ([]PodInfo, error) {
	out, err := p.output(ctx, PodsArgs(), nil)
	if err != nil {
		return nil, err
	}
	return ParsePods(out)
}

// RemoveContainer implements Runtime.
func (p *Podman) RemoveContainer(ctx context.Context, name string) error {
	_, err := p.output(ctx, RemoveContainerArgs(name), nil)
	return err
}

// ImageExecArgs are ImageExec's arguments: a container removed at its
// end, with no network and no mount, pulled when missing.
func ImageExecArgs(image string, cmd []string) []string {
	return append([]string{"run", "--rm", "--network", "none", image}, cmd...)
}

// ImageExec implements Runtime.
func (p *Podman) ImageExec(ctx context.Context, image string, cmd []string) (string, error) {
	return p.output(ctx, ImageExecArgs(image, cmd), nil)
}
