package pgrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// testCA is a CA and a server certificate it issued for host (an IP or
// a DNS name).
func testCA(t *testing.T, host string) (*x509.CertPool, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startTLSUpstream is fakeUpstream behind Postgres's TLS negotiation:
// an SSLRequest answered 'S' (or 'N' when cert is nil), then TLS on
// cert, then the fake's login.
func startTLSUpstream(t *testing.T, cert *tls.Certificate, user, password string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	fu := &fakeUpstream{user: user, password: password, method: authSASL}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				head := make([]byte, 8)
				if _, err := io.ReadFull(c, head); err != nil || binary.BigEndian.Uint32(head[4:]) != sslRequestCode {
					c.Close()
					return
				}
				if cert == nil {
					_, _ = c.Write([]byte{'N'})
					fu.handleConn(c)
					return
				}
				_, _ = c.Write([]byte{'S'})
				fu.handleConn(tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*cert}}))
			}(c)
		}
	}()
	return ln.Addr().String()
}

func dialStartTLS(t *testing.T, addr string, cfg *tls.Config) (net.Conn, error) {
	t.Helper()
	up, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := StartTLS(ctx, up, cfg)
	if err != nil {
		up.Close()
	}
	return c, err
}

// An upstream that only speaks plaintext is refused before any
// credential is sent.
func TestStartTLSRefusesAPlaintextUpstream(t *testing.T) {
	pool, _ := testCA(t, "127.0.0.1")
	addr := startTLSUpstream(t, nil, "app", "s3cret")
	if _, err := dialStartTLS(t, addr, &tls.Config{ServerName: "127.0.0.1", RootCAs: pool}); !errors.Is(err, ErrNoTLS) {
		t.Fatalf("StartTLS to a plaintext upstream = %v, want ErrNoTLS", err)
	}
}

// An upstream whose certificate chains to the CA and names the host is
// logged into over TLS; one the CA did not issue, or for another name,
// is refused at the handshake.
func TestStartTLSVerifiesTheUpstreamsCertificate(t *testing.T) {
	pool, cert := testCA(t, "127.0.0.1")
	addr := startTLSUpstream(t, &cert, "app", "s3cret")

	up, err := dialStartTLS(t, addr, &tls.Config{ServerName: "127.0.0.1", RootCAs: pool})
	if err != nil {
		t.Fatalf("StartTLS: %v", err)
	}
	client, relay := net.Pipe()
	go Serve(context.Background(), relay, up, Credentials{User: "app", Password: "s3cret", Database: "appdb"})
	defer client.Close()
	sendClientStartupMessage(t, client, map[string]string{"user": "whoever"})
	expectLoginSuccess(t, client)
	expectTrivialQuery(t, client)

	other, _ := testCA(t, "127.0.0.1")
	if _, err := dialStartTLS(t, addr, &tls.Config{ServerName: "127.0.0.1", RootCAs: other}); err == nil {
		t.Fatal("an upstream another CA issued was accepted")
	}
	if _, err := dialStartTLS(t, addr, &tls.Config{ServerName: "db.example.test", RootCAs: pool}); err == nil {
		t.Fatal("an upstream certified for another name was accepted")
	}
	if _, err := dialStartTLS(t, addr, &tls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true}); err == nil {
		t.Fatal("StartTLS took a config that verifies nothing")
	}
}
