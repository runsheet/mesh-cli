package relaysidecar

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pgTestCA is a CA's PEM and a server certificate it issued for host.
func pgTestCA(t *testing.T, host string) (string, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pg test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})), tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakePG is a Postgres upstream: an SSLRequest answered 'S' and TLS on
// cert, or 'N' when cert is nil (a server that speaks plaintext alone);
// then a cleartext password login. passwords are what it was sent.
type fakePG struct {
	addr string

	mu        sync.Mutex
	passwords []string
}

func (f *fakePG) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.passwords...)
}

func startFakePG(t *testing.T, cert *tls.Certificate) *fakePG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakePG{addr: ln.Addr().String()}
	write := func(c net.Conn, typ byte, payload []byte) {
		b := []byte{typ, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(b[1:], uint32(4+len(payload)))
		_, _ = c.Write(append(b, payload...))
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				packet := func() []byte {
					head := make([]byte, 4)
					if _, err := io.ReadFull(c, head); err != nil {
						return nil
					}
					body := make([]byte, binary.BigEndian.Uint32(head)-4)
					if _, err := io.ReadFull(c, body); err != nil {
						return nil
					}
					return body
				}
				first := packet()
				if len(first) == 4 && binary.BigEndian.Uint32(first) == 80877103 { // SSLRequest
					if cert == nil {
						_, _ = c.Write([]byte{'N'})
					} else {
						_, _ = c.Write([]byte{'S'})
						tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*cert}})
						if tc.Handshake() != nil {
							return
						}
						c = tc
					}
					if packet() == nil { // the StartupMessage
						return
					}
				}
				write(c, 'R', []byte{0, 0, 0, 3}) // cleartext password
				head := make([]byte, 5)
				if _, err := io.ReadFull(c, head); err != nil || head[0] != 'p' {
					return
				}
				pw := make([]byte, binary.BigEndian.Uint32(head[1:])-4)
				if _, err := io.ReadFull(c, pw); err != nil {
					return
				}
				f.mu.Lock()
				f.passwords = append(f.passwords, strings.TrimRight(string(pw), "\x00"))
				f.mu.Unlock()
				write(c, 'R', []byte{0, 0, 0, 0})
				write(c, 'Z', []byte{'I'})
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()
	return f
}

// pgLogin connects to a bridge on port and answers whether its login
// reached ReadyForQuery.
func pgLogin(t *testing.T, port int) bool {
	t.Helper()
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	body := append([]byte{0, 3, 0, 0}, []byte("user\x00whoever\x00\x00")...)
	head := make([]byte, 4)
	binary.BigEndian.PutUint32(head, uint32(4+len(body)))
	if _, err := c.Write(append(head, body...)); err != nil {
		return false
	}
	for {
		h := make([]byte, 5)
		if _, err := io.ReadFull(c, h); err != nil {
			return false
		}
		if _, err := io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(h[1:])-4)); err != nil {
			return false
		}
		if h[0] == 'Z' {
			return true
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The bridge's upstream leg is TLS, verified: an upstream that speaks
// plaintext alone is never sent the password and the pod's login fails;
// one whose certificate chains to the credential's CA and names its host
// is logged into; plaintext is only for a loopback upstream that the
// credential says is one, never another host.
func TestThePostgresBridgeLogsInOverVerifiedTLSAlone(t *testing.T) {
	caPEM, cert := pgTestCA(t, "db.example.test")
	otherPEM, _ := pgTestCA(t, "db.example.test")
	g := newRig(t, testCreds(t), nil)
	plain := startFakePG(t, nil)
	secure := startFakePG(t, &cert)
	g.tunnel.routes["plain.example.test:5432"] = plain.addr
	g.tunnel.routes["db.example.test:5432"] = secure.addr

	bridge := func(p Postgres) int {
		t.Helper()
		p.Listen, p.Port, p.User, p.Password = freePort(t), 5432, "app", "pg-secret-value"
		if err := g.sidecar.ServePostgres(p); err != nil {
			t.Fatal(err)
		}
		return p.Listen
	}

	// a plaintext-only upstream: refused, nothing sent
	if pgLogin(t, bridge(Postgres{Host: "plain.example.test", RootCA: caPEM})) {
		t.Fatal("a login through a plaintext-only upstream succeeded")
	}
	if got := plain.sent(); len(got) != 0 {
		t.Fatalf("a plaintext-only upstream was sent %q", got)
	}
	// plaintext asked for, to a host that is not loopback: refused
	if pgLogin(t, bridge(Postgres{Host: "plain.example.test", Plaintext: true})) {
		t.Fatal("a plaintext login to a non-loopback upstream succeeded")
	}
	if got := plain.sent(); len(got) != 0 {
		t.Fatalf("a non-loopback upstream was sent %q in plaintext", got)
	}
	// TLS from a CA the credential does not name: refused
	if pgLogin(t, bridge(Postgres{Host: "db.example.test", RootCA: otherPEM})) {
		t.Fatal("a login to an upstream another CA certified succeeded")
	}
	// the system's roots do not know the test CA: refused
	if pgLogin(t, bridge(Postgres{Host: "db.example.test"})) {
		t.Fatal("a login to an upstream the system's roots do not trust succeeded")
	}
	if got := secure.sent(); len(got) != 0 {
		t.Fatalf("an unverified upstream was sent %q", got)
	}
	// TLS on the credential's CA: logged in
	if !pgLogin(t, bridge(Postgres{Host: "db.example.test", RootCA: caPEM})) {
		t.Fatal("a login over verified TLS failed")
	}
	if got := secure.sent(); len(got) != 1 || got[0] != "pg-secret-value" {
		t.Fatalf("the verified upstream was sent %q", got)
	}
}

func TestPlaintextIsAllowedToALoopbackHostAlone(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "[::1]": true, "localhost": true, "LOCALHOST.": true, "127.8.9.10": true,
		"10.0.0.1": false, "db.example.test": false, "localhost.example.test": false, "": false,
	} {
		if got := PlaintextAllowed(host); got != want {
			t.Errorf("PlaintextAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}
