package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
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
// operator's uid kept, on pasta with no route to the host's loopback or
// gateway (no --map-gw, no host.containers.internal), DNS through the
// forwarder the rules allow.
func PodCreateArgs(spec PodSpec) []string {
	args := []string{"pod", "create", "--name", spec.Name, "--userns", "keep-id",
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
// variable is passed by name (`--env NAME`) and its value put in
// podman's own environment (AgentProcessEnv), so no secret is on a
// command line; --unsetenv-all drops the image's and podman's defaults.
func AgentRunArgs(pod Pod, spec AgentSpec) []string {
	args := []string{"run", "-i", "--pod", pod.Name, "--name", spec.Name,
		"--cap-drop", "all", "--security-opt", "no-new-privileges",
		"--pull", "missing", "--unsetenv-all"}
	args = append(args, LimitArgs(spec.Limits)...)
	if spec.TimeoutSeconds > 0 {
		args = append(args, "--timeout", strconv.Itoa(spec.TimeoutSeconds))
	}
	for _, m := range spec.Mounts {
		opt := "rw"
		if m.ReadOnly {
			opt = "ro"
		}
		args = append(args, "--volume", m.Source+":"+m.Target+":"+opt)
	}
	if spec.Workdir != "" {
		args = append(args, "--workdir", spec.Workdir)
	}
	for _, kv := range spec.Env {
		name, _, _ := strings.Cut(kv, "=")
		args = append(args, "--env", name)
	}
	args = append(args, spec.Image)
	return append(args, spec.Command...)
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
	for _, kv := range spec.Env {
		name, _, _ := strings.Cut(kv, "=")
		args = append(args, "--env", name)
	}
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

// podmanPassthrough are the node's variables podman itself needs to find
// its storage and runtime directory; the container sees none of them.
var podmanPassthrough = []string{"PATH", "HOME", "USER", "LOGNAME", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CONTAINERS_CONF", "CONTAINERS_STORAGE_CONF", "TMPDIR"}

// podmanEnv is podman's own process environment: what it needs from the
// node's, then the values of the container's variables, which
// `--env NAME` reads from here. A container variable of the same name
// wins; the container gets only what its --env names.
func podmanEnv(environ, container []string) []string {
	have := map[string]bool{}
	for _, kv := range container {
		name, _, _ := strings.Cut(kv, "=")
		have[name] = true
	}
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		for _, p := range podmanPassthrough {
			if name == p && !have[name] {
				out = append(out, kv)
			}
		}
	}
	return append(out, container...)
}

// CreatePod implements Runtime: the pod, then its network rules. A pod
// whose rules will not load is removed and not used.
func (p *Podman) CreatePod(ctx context.Context, spec PodSpec) (Pod, error) {
	if _, err := p.output(ctx, PodCreateArgs(spec), nil); err != nil {
		return Pod{}, err
	}
	pod := Pod{Name: spec.Name}
	if _, err := p.output(ctx, NetworkInitArgs(pod, spec.Image), strings.NewReader(spec.Network.Ruleset())); err != nil {
		_ = p.Remove(context.WithoutCancel(ctx), pod)
		return Pod{}, fmt.Errorf("network rules: %w", err)
	}
	return pod, nil
}

// RunAgent implements Runtime. The container is left for Remove, so its
// state can be read: an exit of 137 asks whether it was the OOM killer.
func (p *Podman) RunAgent(ctx context.Context, pod Pod, spec AgentSpec) (Exit, error) {
	// podman's own env carries HOME for its storage; the container's
	// HOME is --env HOME from the same process, so the container's value
	// must win there — podmanEnv puts it last
	err := p.run(ctx, AgentRunArgs(pod, spec), podmanEnv(os.Environ(), spec.Env), spec.Stdin, spec.Stdout, spec.Stderr)
	if err == nil {
		return Exit{}, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return Exit{}, err
	}
	exit := Exit{Code: ee.ExitCode()}
	if exit.Code == 137 {
		if out, ierr := p.output(context.WithoutCancel(ctx), []string{"inspect", "--format", "{{.State.OOMKilled}}", spec.Name}, nil); ierr == nil {
			exit.OOMKilled = out == "true"
		}
	}
	return exit, nil
}

// AddSidecar implements Runtime.
func (p *Podman) AddSidecar(ctx context.Context, pod Pod, spec SidecarSpec) error {
	var errb bytes.Buffer
	if err := p.run(ctx, SidecarRunArgs(pod, spec), podmanEnv(os.Environ(), spec.Env), nil, io.Discard, &errb); err != nil {
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

// Remove implements Runtime: the pod and all its containers, forced.
func (p *Podman) Remove(ctx context.Context, pod Pod) error {
	_, err := p.output(ctx, []string{"pod", "rm", "--force", "--time", "5", pod.Name}, nil)
	return err
}
