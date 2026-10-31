package egress

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// The relay's certificate authority: what lets a lease's tunnel
// terminate TLS for a host one of its Rules names (intercept.go). One
// per lease, made when the lease's relay opens and gone when it ends:
// its private key is made in the node's memory, is never written to
// disk, never reaches the pod or a relay sidecar, and dies with the
// lease's tunnel. Its certificate is all the pod gets, in the trust
// store the node writes into the session's HOME (nodev1/trust.go). The
// certificate is name-constrained to the hosts the lease's rules
// terminate, so even its key could mint nothing another host's client
// would take, and no other lease's pod trusts it at all.
const (
	// CALifetime bounds a relay CA's certificate: longer than any lease
	// runs, and moot once the lease ends since its key is gone.
	CALifetime = 7 * 24 * time.Hour
	// LeafLifetime is a minted host certificate's life; one is minted
	// again once less than LeafRenewBefore of it is left.
	LeafLifetime    = time.Hour
	LeafRenewBefore = 10 * time.Minute
	// backdate is how far before now a certificate starts, for a pod
	// whose clock is a little behind the node's.
	backdate = 5 * time.Minute
)

// CA is a lease's relay certificate authority.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	hosts   []string
	// Now is the clock; time.Now when nil (tests).
	Now func() time.Time

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// NewCA makes a lease's CA, its certificate valid for lifetime
// (CALifetime when zero) and permitted to name only hosts and their
// subdomains (RFC 5280 name constraints, critical). lease names it in
// its subject, for a person reading the pod's trust store. hosts may not
// be empty: a CA that may name anything is never made.
func NewCA(lease string, hosts []string, lifetime time.Duration) (*CA, error) {
	var permitted []string
	seen := map[string]bool{}
	for _, h := range hosts {
		h = strings.TrimPrefix(norm(h), "*.")
		if h == "" || seen[h] {
			continue
		}
		if net.ParseIP(h) != nil {
			return nil, fmt.Errorf("egress: a relay CA names hosts, not addresses: %s", h)
		}
		seen[h] = true
		permitted = append(permitted, h)
	}
	if len(permitted) == 0 {
		return nil, errors.New("egress: a relay CA needs the hosts it may name")
	}
	if lifetime <= 0 {
		lifetime = CALifetime
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	name := "Mesh relay CA"
	if lease != "" {
		name += " for lease " + lease
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: name, Organization: []string{"Mesh relay"}},
		NotBefore:                   now.Add(-backdate),
		NotAfter:                    now.Add(lifetime),
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         permitted,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, hosts: permitted,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// CertPEM is the CA's certificate, PEM: what the pod trusts. Never the
// key.
func (c *CA) CertPEM() []byte { return append([]byte(nil), c.certPEM...) }

// Certificate is the CA's parsed certificate.
func (c *CA) Certificate() *x509.Certificate { return c.cert }

// Hosts are the hosts (and their subdomains) the CA may name.
func (c *CA) Hosts() []string { return append([]string(nil), c.hosts...) }

// String names the CA without anything private.
func (c *CA) String() string {
	return "egress.CA(" + c.cert.Subject.CommonName + ", " + strings.Join(c.hosts, " ") + ")"
}

func (c *CA) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Leaf is a certificate for host signed by the CA, minted on first ask
// and again once it is near its end: LeafLifetime long, its own key,
// host its one DNS name. A host outside the CA's constraints is refused
// here rather than minted for a client to refuse.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	host = norm(host)
	if host == "" || net.ParseIP(host) != nil {
		return nil, fmt.Errorf("egress: no certificate for %q", host)
	}
	if !c.permits(host) {
		return nil, fmt.Errorf("egress: the relay CA may not name %s", host)
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.leaves[host]; l != nil && now.Add(LeafRenewBefore).Before(l.Leaf.NotAfter) {
		return l, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(LeafLifetime)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-backdate),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	out := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key, Leaf: leaf}
	if c.leaves == nil {
		c.leaves = map[string]*tls.Certificate{}
	}
	c.leaves[host] = out
	return out, nil
}

// permits says host is one of the CA's hosts or under one.
func (c *CA) permits(host string) bool {
	for _, h := range c.hosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}
