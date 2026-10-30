package sandbox

import (
	"fmt"
	"net"
	"strings"
)

// LANRanges are the private IPv4 ranges a session never reaches (ADR 007
// Network).
var LANRanges = []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12"}

// OtherRefused are the other ranges refused beside the LAN: loopback
// beyond the pod's own interface (the host's, through the user-mode
// network), link-local, CGNAT/tailnet, and IPv6 loopback, ULA and
// link-local.
var OtherRefused = []string{"127.0.0.0/8", "169.254.0.0/16", "100.64.0.0/10"}

// OtherRefused6 are the IPv6 ranges refused.
var OtherRefused6 = []string{"::1/128", "fc00::/7", "fe80::/10"}

// DNSForward is the address the pod's user-mode network answers DNS on
// (pasta's --dns-forward): the one link-local address the rules let a
// session reach, on port 53 only.
const DNSForward = "169.254.1.1"

// NetworkPolicy is what a pod may reach: outbound Ports to public
// addresses and DNS through DNSForward; never Refused, Refused6 or the
// host's own addresses in HostAddrs.
type NetworkPolicy struct {
	Ports     []int
	Refused   []string
	Refused6  []string
	HostAddrs []string
	// NoDNS drops DNS too: a pod whose only way out is the relay
	// (RelayOnly), which resolves for it.
	NoDNS bool
	// None is a pod with no network at all but its own loopback
	// (Isolated): `--network none`, its containers sharing the network
	// namespace and no other, and no rules to load. A lease whose relay
	// is a sidecar in the pod reaches the node over a unix socket, so
	// nothing in the pod needs a route.
	None bool
}

// Isolated is a pod with no route out at all (projectmesh-io/api ADR
// 001 amendment "relay sidecar"): `podman pod create --network none
// --share net`. Its containers share loopback, where the relay sidecar
// listens, and share no PID, IPC or UTS namespace.
func Isolated() NetworkPolicy { return NetworkPolicy{None: true, NoDNS: true} }

// RelayOnly is p for a session whose every exit is its relay (a Mesh v1
// lease whose kind declares its egress): no port out to any address and
// no DNS, so the pod reaches nothing beyond its own loopback, where its
// bridge to the lease's socket listens. Anything that skips the relay's
// forward proxy finds no route out.
func (p NetworkPolicy) RelayOnly() NetworkPolicy {
	p.Ports = nil
	p.NoDNS = true
	return p
}

// DefaultNetwork is ADR 007's policy: HTTPS and HTTP out, the LAN, the
// host and the special ranges refused. The host's own addresses are
// those of its interfaces.
func DefaultNetwork() NetworkPolicy {
	return NetworkPolicy{
		Ports:     []int{443, 80},
		Refused:   append(append([]string{}, LANRanges...), OtherRefused...),
		Refused6:  append([]string{}, OtherRefused6...),
		HostAddrs: hostAddrs(),
	}
}

// hostAddrs are the host's global unicast addresses: a public one is not
// in any refused range, and the host's services listen on it too.
func hostAddrs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || !ipn.IP.IsGlobalUnicast() || ipn.IP.IsPrivate() {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	return out
}

// Ruleset is the nftables table the pod's init container loads into the
// pod's own network namespace: loopback (the pod's sidecars) and the DNS
// forwarder accepted, the refused ranges and the host dropped, the
// allowed ports accepted, everything else dropped.
func (p NetworkPolicy) Ruleset() string {
	var b strings.Builder
	b.WriteString("table inet mesh_sandbox {\n")
	b.WriteString("\tchain output {\n")
	b.WriteString("\t\ttype filter hook output priority 0; policy drop;\n")
	b.WriteString("\t\toif \"lo\" accept\n")
	b.WriteString("\t\tct state established,related accept\n")
	if !p.NoDNS {
		fmt.Fprintf(&b, "\t\tip daddr %s meta l4proto { tcp, udp } th dport 53 accept\n", DNSForward)
	}
	var v4, v6 []string
	for _, a := range p.HostAddrs {
		if strings.Contains(a, ":") {
			v6 = append(v6, a)
		} else {
			v4 = append(v4, a)
		}
	}
	if r := append(append([]string{}, p.Refused...), v4...); len(r) > 0 {
		fmt.Fprintf(&b, "\t\tip daddr { %s } drop\n", strings.Join(r, ", "))
	}
	if r := append(append([]string{}, p.Refused6...), v6...); len(r) > 0 {
		fmt.Fprintf(&b, "\t\tip6 daddr { %s } drop\n", strings.Join(r, ", "))
	}
	if len(p.Ports) > 0 {
		ports := make([]string, len(p.Ports))
		for i, n := range p.Ports {
			ports[i] = fmt.Sprint(n)
		}
		fmt.Fprintf(&b, "\t\ttcp dport { %s } accept\n", strings.Join(ports, ", "))
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String()
}
