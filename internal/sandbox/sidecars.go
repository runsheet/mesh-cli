package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultServiceTimeout bounds the wait for every service's health
// check (node.yaml sandbox.service_timeout).
const DefaultServiceTimeout = 120 * time.Second

// SidecarPids is each sidecar's process cap.
const SidecarPids = 1024

// ServicesFileName is the file under the session's HOME that lists its
// services, for the agent and its tests to read ($HOME/.mesh-services).
const ServicesFileName = ".mesh-services"

// Defaults of a health check the declaration leaves out.
const (
	healthInterval = time.Second
	healthTimeout  = 5 * time.Second
)

// ServiceStatus is how one declared service came up in a session's pod.
type ServiceStatus struct {
	Name     string
	Image    string
	Required bool
	// Ports are the ports it listens on inside the pod: 127.0.0.1:<port>
	// from the agent's container.
	Ports []int
	Ready bool
	// Why is why it is not ready: refused, did not start, or its health
	// check did not pass.
	Why string
}

// Describe is "name (image)".
func (st ServiceStatus) Describe() string { return fmt.Sprintf("%s (%s)", st.Name, st.Image) }

// Addrs are its 127.0.0.1 addresses, comma-separated ("" when none).
func (st ServiceStatus) Addrs() string {
	var a []string
	for _, p := range st.Ports {
		a = append(a, "127.0.0.1:"+strconv.Itoa(p))
	}
	return strings.Join(a, ", ")
}

// ContainerPorts are the ports a service listens on inside the pod: each
// declared port's container side ("5432:5432", "8080:80/tcp" → 80), in
// order, without repeats. A pod publishes nothing on the host, so the
// host side does not matter.
func ContainerPorts(ports []string) []int {
	var out []int
	seen := map[int]bool{}
	for _, p := range ports {
		p, _, _ = strings.Cut(p, "/")
		if i := strings.LastIndex(p, ":"); i >= 0 {
			p = p[i+1:]
		}
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// Start creates the session's pod before its agent and starts every
// declared service in it as a sidecar — sharing the pod's network, so
// each listens on 127.0.0.1 at its own port — under the services'
// budget split between them, then waits for each one's health check
// (the declared --health-cmd, else a TCP connect on its first port) up
// to timeout (DefaultServiceTimeout when 0). Run then runs the agent in
// that pod.
//
// A refused service is never pulled. When a required one is refused no
// pod is made at all: the others are "not started" and the session
// must not run. The error is the pod not being made, or ctx ending;
// the statuses say the rest. Close removes the pod on every path.
func (s *Session) Start(ctx context.Context, services []Service, timeout time.Duration) ([]ServiceStatus, error) {
	sts := make([]ServiceStatus, len(services))
	refused := ""
	for i, svc := range services {
		sts[i] = ServiceStatus{Name: svc.Name, Image: svc.Image, Required: svc.Required, Ports: ContainerPorts(svc.Ports)}
		if svc.Refused {
			sts[i].Why = refusedWhy(svc)
			if svc.Required && refused == "" {
				refused = svc.Name
			}
		}
	}
	if refused != "" {
		for i := range sts {
			if sts[i].Why == "" {
				sts[i].Why = fmt.Sprintf("not started: the required service %s was refused", refused)
			}
		}
		return sts, nil
	}
	pod, err := s.createPod(ctx)
	if err != nil {
		return sts, err
	}
	s.mu.Lock()
	s.started = &pod
	s.mu.Unlock()
	if len(services) == 0 {
		return sts, nil
	}
	s.mu.Lock()
	s.services = &pod
	s.mu.Unlock()
	if timeout <= 0 {
		timeout = DefaultServiceTimeout
	}
	n := 0
	for _, svc := range services {
		if !svc.Refused {
			n++
		}
	}
	limits := SplitLimits(s.sb.Config.ServicesLimits, n)
	var started []int
	for i, svc := range services {
		if svc.Refused {
			continue
		}
		spec := SidecarSpec{Name: svc.Name, Image: svc.Image, Env: sortedEnv(svc.Env), Command: svc.Command, Limits: limits, Tmpfs: s.sb.Config.tmpfsOf(svc)}
		if err := s.sb.Runtime.AddSidecar(ctx, pod, spec); err != nil {
			if ctx.Err() != nil {
				return sts, ctx.Err()
			}
			sts[i].Why = "did not start: " + oneLine(err.Error())
			continue
		}
		started = append(started, i)
	}
	// every started service is waited for at once, within one bound
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, i := range started {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.waitHealthy(wctx, pod, services[i], timeout); err != nil {
				sts[i].Why = err.Error()
				return
			}
			sts[i].Ready = true
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return sts, ctx.Err()
	}
	return sts, nil
}

func refusedWhy(svc Service) string {
	for _, n := range svc.Notes {
		if strings.Contains(n, "refused") {
			return n
		}
	}
	return fmt.Sprintf("image %s refused: registry %s is not allowed", svc.Image, svc.Registry)
}

// waitHealthy runs svc's health check until it passes, its retries after
// the start period are spent, or ctx (the bound) ends.
func (s *Session) waitHealthy(ctx context.Context, pod Pod, svc Service, bound time.Duration) error {
	rt := s.sb.Runtime
	var check func(context.Context) error
	interval, per, retries, startPeriod := healthInterval, healthTimeout, 0, time.Duration(0)
	what := "health check"
	if h := svc.Health; h != nil {
		if h.Interval > 0 {
			interval = h.Interval
		}
		if h.Timeout > 0 {
			per = h.Timeout
		}
		retries, startPeriod = h.Retries, h.StartPeriod
	}
	ports := ContainerPorts(svc.Ports)
	switch {
	case svc.Health != nil && svc.Health.Cmd != "":
		cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(svc.Health.Cmd), "CMD-SHELL"))
		check = func(c context.Context) error { return rt.ExecSidecar(c, pod, svc.Name, []string{"sh", "-c", cmd}) }
	case len(ports) > 0:
		what = fmt.Sprintf("TCP connect to 127.0.0.1:%d", ports[0])
		check = func(c context.Context) error { return rt.ProbeTCP(c, pod, s.sb.Config.Image, ports[0]) }
	default:
		return nil // nothing to check: started is ready
	}
	begin := time.Now()
	fails := 0
	var last error
	for {
		cctx, cancel := context.WithTimeout(ctx, per)
		err := check(cctx)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		if time.Since(begin) >= startPeriod {
			fails++
		}
		if retries > 0 && fails >= retries {
			return fmt.Errorf("%s failed %d times: %s", what, fails, oneLine(last.Error()))
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%s did not pass within %s: %s", what, bound, oneLine(last.Error()))
			}
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// SplitLimits is each of n sidecars' share of the services' budget:
// the CPUs and the memory divided evenly, and SidecarPids each.
func SplitLimits(budget Limits, n int) Limits {
	if n <= 1 {
		n = 1
	}
	l := Limits{Pids: SidecarPids}
	if budget.CPUs > 0 {
		l.CPUs = budget.CPUs / float64(n)
	}
	if budget.Memory != "" {
		if b, ok := parseSize(budget.Memory); ok {
			each := b / int64(n)
			if each >= 1<<20 {
				l.Memory = strconv.FormatInt(each>>20, 10) + "m"
			} else {
				l.Memory = strconv.FormatInt(each>>10, 10) + "k"
			}
		} else {
			l.Memory = budget.Memory
		}
	}
	return l
}

// parseSize reads a podman size ("4g", "512m", "1024k", "2048b", "100").
func parseSize(s string) (int64, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "b")
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return int64(f * float64(mult)), true
}

func sortedEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:299] + "…"
	}
	return s
}

// ServicesLine is what the agent is told of its services: each ready
// one with its 127.0.0.1 address, then each missing one and why. Empty
// when there are none.
func ServicesLine(sts []ServiceStatus) string {
	if len(sts) == 0 {
		return ""
	}
	var ready, missing []string
	for _, st := range sts {
		if st.Ready {
			at := st.Addrs()
			if at == "" {
				at = "no declared port"
			}
			ready = append(ready, fmt.Sprintf("%s at %s", st.Describe(), at))
		} else {
			missing = append(missing, fmt.Sprintf("%s: %s", st.Describe(), st.Why))
		}
	}
	var b strings.Builder
	if len(ready) > 0 {
		fmt.Fprintf(&b, "The services this repository declares run beside you and are ready on 127.0.0.1 (also listed in `$HOME/%s`): %s.", ServicesFileName, strings.Join(ready, "; "))
	}
	if len(missing) > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "Missing — it could not start, so a test that needs it fails here for that reason, not your code's: %s.", strings.Join(missing, "; "))
	}
	return b.String()
}

// ServicesListing is $HOME/.mesh-services: one line per service, its name,
// image, 127.0.0.1 addresses ("-" when none) and `ready` or `failed:
// <why>`, tab-separated.
func ServicesListing(sts []ServiceStatus) []byte {
	var b strings.Builder
	for _, st := range sts {
		at := strings.ReplaceAll(st.Addrs(), " ", "")
		if at == "" {
			at = "-"
		}
		state := "ready"
		if !st.Ready {
			state = "failed: " + st.Why
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", st.Name, st.Image, at, state)
	}
	return []byte(b.String())
}

// tmpfsOf is svc's tmpfs mounts with each size filled in: its own, else
// ServicesTmpfs, else DefaultServicesTmpfs.
func (c Config) tmpfsOf(svc Service) []Tmpfs {
	if len(svc.Tmpfs) == 0 {
		return nil
	}
	size := c.ServicesTmpfs
	if size == "" {
		size = DefaultServicesTmpfs
	}
	out := make([]Tmpfs, len(svc.Tmpfs))
	for i, m := range svc.Tmpfs {
		if m.Size == "" {
			m.Size = size
		}
		out[i] = m
	}
	return out
}
