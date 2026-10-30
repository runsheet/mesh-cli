package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// A sandboxed lease's relay runs as a sidecar container in the lease's
// own pod (projectmesh-io/api ADR 001 amendment "relay sidecar"), so a
// bug in the code that reads what the pod sends exposes one lease's
// short-lived credentials and never the node's. What keeps the agent
// container from reading it:
//
//   - the pod is Isolated: no network but loopback, and its containers
//     share the network namespace alone, so the agent cannot see the
//     sidecar's processes, their environ, memory or file descriptors;
//   - the sidecar runs as RelayUID, not the agent's user, and every
//     container runs with all capabilities dropped and
//     no-new-privileges, so the agent cannot become it;
//   - its credentials are on a tmpfs mounted into the sidecar alone,
//     mode 0700 and owned by RelayUID, written 0400 through the
//     sidecar's stdin (RelayInstallArgs), never through env or argv;
//   - its root filesystem is read-only;
//   - no runtime socket is mounted into any container of the pod.

// RelayUID is the relay sidecar's user inside the pod: not the agent's
// (the image's user, DefaultImageUID unless the image says otherwise),
// and in the rootless pod's range of sub-ids, so no host user is it.
const RelayUID = 64000

// RelayCredentialsDir is the sidecar's credential tmpfs.
const RelayCredentialsDir = "/run/mesh-relay"

// RelayBinary is where the node's own binary is mounted, read-only, in
// the sidecar.
const RelayBinary = "/usr/local/libexec/mesh-relay"

// RelaySocketDir is where the lease's socket directory is mounted in the
// sidecar.
const RelaySocketDir = "/mesh-lease"

// RelayCheckout is where the lease's checkout is mounted, read-only, in
// the sidecar: the relay proves a push's fast-forward there.
const RelayCheckout = "/mesh-checkout"

// relayPath is the sidecar's PATH: the node image's git, nothing of the
// node's.
const relayPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// RelayLimits bound the sidecar: it relays, it does not build.
var RelayLimits = Limits{CPUs: 1, Memory: "512m", Pids: 256}

// RelaySpec is a lease's relay sidecar.
type RelaySpec struct {
	// Image is the node image, which carries git.
	Image string
	// Binary is the host path of the node's own binary.
	Binary string
	// SocketDir is the host path of the lease's socket directory.
	SocketDir string
	// Checkout is the host path of the lease's checkout, "" for none.
	Checkout string
	// Mirrors are host paths of the node's mirrors this lease may read
	// (its tenant's repository and read_repos), each mounted read-only
	// at its own path.
	Mirrors []string
	// Args follow the binary: `relay-sidecar --credentials-dir …`.
	Args []string
}

// RelayName is the relay sidecar's container name in pod.
func RelayName(pod Pod) string { return pod.Name + "-relay" }

// RelayRunArgs is `podman run -d` for the relay sidecar. Nothing on it
// is a secret: the credentials come later, through RelayInstallArgs'
// stdin.
func RelayRunArgs(pod Pod, spec RelaySpec) []string {
	uid := strconv.Itoa(RelayUID)
	args := []string{"run", "-d", "--init", "--pod", pod.Name, "--name", RelayName(pod),
		"--user", uid + ":" + uid,
		"--read-only",
		"--cap-drop", "all", "--security-opt", "no-new-privileges",
		"--pull", "missing", "--unsetenv-all",
		"--env", "PATH=" + relayPath, "--env", "HOME=" + RelayCredentialsDir,
		"--mount", "type=tmpfs,destination=" + RelayCredentialsDir + ",tmpfs-size=1m,tmpfs-mode=0700,chown=true",
		"--volume", spec.Binary + ":" + RelayBinary + ":ro",
		"--volume", spec.SocketDir + ":" + RelaySocketDir + ":rw",
	}
	if spec.Checkout != "" {
		args = append(args, "--volume", spec.Checkout+":"+RelayCheckout+":ro")
	}
	for _, m := range spec.Mirrors {
		args = append(args, "--volume", m+":"+m+":ro")
	}
	args = append(args, LimitArgs(RelayLimits)...)
	args = append(args, spec.Image, RelayBinary)
	return append(args, spec.Args...)
}

// RelayInstallArgs is `podman exec -i` of the sidecar's own install, as
// its own user: the credentials it reads on stdin go into its tmpfs,
// mode 0400.
func RelayInstallArgs(pod Pod) []string {
	uid := strconv.Itoa(RelayUID)
	return []string{"exec", "-i", "--user", uid + ":" + uid, RelayName(pod),
		RelayBinary, "relay-sidecar", "install", "--dir", RelayCredentialsDir}
}

// RelayChownArgs gives path to the sidecar's user, as the rootless
// pod's user namespace maps it (`podman unshare chown`).
func RelayChownArgs(path string) []string {
	uid := strconv.Itoa(RelayUID)
	return []string{"unshare", "chown", uid + ":" + uid, "--", path}
}

// RelayRuntime is a Runtime that runs relay sidecars.
type RelayRuntime interface {
	// StartRelay starts the relay sidecar in pod.
	StartRelay(ctx context.Context, pod Pod, spec RelaySpec) error
	// InstallRelay hands the sidecar its credentials on stdin.
	InstallRelay(ctx context.Context, pod Pod, creds io.Reader) error
	// ChownForRelay gives a host path to the sidecar's user.
	ChownForRelay(ctx context.Context, path string) error
}

// StartRelay implements RelayRuntime.
func (p *Podman) StartRelay(ctx context.Context, pod Pod, spec RelaySpec) error {
	if _, err := p.output(ctx, RelayRunArgs(pod, spec), nil); err != nil {
		return fmt.Errorf("the relay sidecar: %w", err)
	}
	return nil
}

// InstallRelay implements RelayRuntime. Podman's own error words are
// kept; the credentials, on stdin, are in none of them.
func (p *Podman) InstallRelay(ctx context.Context, pod Pod, creds io.Reader) error {
	if _, err := p.output(ctx, RelayInstallArgs(pod), creds); err != nil {
		return fmt.Errorf("installing the relay sidecar's credentials: %w", err)
	}
	return nil
}

// ChownForRelay implements RelayRuntime.
func (p *Podman) ChownForRelay(ctx context.Context, path string) error {
	_, err := p.output(ctx, RelayChownArgs(path), nil)
	return err
}

// StartRelay implements RelayRuntime: it records the spec.
func (f *Fake) StartRelay(_ context.Context, pod Pod, spec RelaySpec) error {
	f.call("relay " + pod.Name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Relays = append(f.Relays, spec)
	return f.RelayErr
}

// InstallRelay implements RelayRuntime: it keeps what was installed, so
// a test can read it, and calls RelayInstalled when set (a test's
// in-process sidecar).
func (f *Fake) InstallRelay(_ context.Context, pod Pod, creds io.Reader) error {
	f.call("relay-install " + pod.Name)
	b, err := io.ReadAll(creds)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.RelayCredentials = append(f.RelayCredentials, b)
	hook := f.RelayInstalled
	f.mu.Unlock()
	if hook != nil {
		return hook(pod, b)
	}
	return nil
}

// ChownForRelay implements RelayRuntime: it records the path.
func (f *Fake) ChownForRelay(_ context.Context, path string) error {
	f.call("relay-chown " + path)
	return nil
}

// ErrNoRelayRuntime is a sandbox whose runtime runs no relay sidecar.
var ErrNoRelayRuntime = errors.New("sandbox: the runtime runs no relay sidecar")

// StartRelay starts the session's relay sidecar in its pod, making the
// pod first when nothing has: the agent, its services and its steps run
// beside it.
func (s *Session) StartRelay(ctx context.Context, spec RelaySpec) error {
	rr, ok := s.sb.Runtime.(RelayRuntime)
	if !ok {
		return ErrNoRelayRuntime
	}
	if spec.Image == "" {
		spec.Image = s.sb.Config.Image
	}
	for _, src := range append([]string{spec.Binary, spec.SocketDir}, spec.Mirrors...) {
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("the relay sidecar's mount: %w", err)
		}
	}
	pod, err := s.ensurePod(ctx)
	if err != nil {
		return err
	}
	return rr.StartRelay(ctx, pod, spec)
}

// InstallRelay hands the session's relay sidecar its credentials.
func (s *Session) InstallRelay(ctx context.Context, creds io.Reader) error {
	rr, ok := s.sb.Runtime.(RelayRuntime)
	if !ok {
		return ErrNoRelayRuntime
	}
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started == nil {
		return errors.New("sandbox: the relay sidecar's pod is not made")
	}
	return rr.InstallRelay(ctx, *started, creds)
}

// ChownForRelay gives path to the relay sidecar's user.
func (s *Session) ChownForRelay(ctx context.Context, path string) error {
	rr, ok := s.sb.Runtime.(RelayRuntime)
	if !ok {
		return ErrNoRelayRuntime
	}
	return rr.ChownForRelay(ctx, path)
}

// RemoveTree removes a tree the relay sidecar's user owns part of.
func (s *Session) RemoveTree(ctx context.Context, path string) error {
	return s.sb.Runtime.RemoveTree(ctx, path)
}

// ensurePod is the session's started pod, made now when there is none.
func (s *Session) ensurePod(ctx context.Context) (Pod, error) {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started != nil {
		return *started, nil
	}
	pod, err := s.createPod(ctx)
	if err != nil {
		return Pod{}, err
	}
	s.mu.Lock()
	s.started = &pod
	s.mu.Unlock()
	return pod, nil
}
