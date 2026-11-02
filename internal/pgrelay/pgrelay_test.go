package pgrelay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
)

// --- a fake upstream Postgres server, speaking one auth method ---

type fakeUpstream struct {
	user     string
	password string
	method   byte // authOK (trust), authCleartext, authMD5 or authSASL
	ln       net.Listener
}

func startFakeUpstream(t *testing.T, user, password string, method byte) *fakeUpstream {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	fu := &fakeUpstream{user: user, password: password, method: method, ln: ln}
	go fu.acceptLoop()
	return fu
}

func (fu *fakeUpstream) addr() string { return fu.ln.Addr().String() }

func (fu *fakeUpstream) acceptLoop() {
	for {
		conn, err := fu.ln.Accept()
		if err != nil {
			return
		}
		go fu.handleConn(conn)
	}
}

func (fu *fakeUpstream) handleConn(conn net.Conn) {
	defer conn.Close()
	if _, err := readStartupPacket(conn); err != nil {
		return
	}

	switch fu.method {
	case authCleartext:
		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, authCleartext)
		if err := writeMessage(conn, 'R', payload); err != nil {
			return
		}
		typ, pw, err := readMessage(conn)
		if err != nil || typ != 'p' {
			return
		}
		if strings.TrimSuffix(string(pw), "\x00") != fu.password {
			writeMessage(conn, 'E', fakeErrorPayload("password authentication failed"))
			return
		}
	case authMD5:
		salt := []byte{0x01, 0x02, 0x03, 0x04}
		payload := make([]byte, 8)
		binary.BigEndian.PutUint32(payload[:4], authMD5)
		copy(payload[4:], salt)
		if err := writeMessage(conn, 'R', payload); err != nil {
			return
		}
		typ, pw, err := readMessage(conn)
		if err != nil || typ != 'p' {
			return
		}
		want := md5PasswordHash(fu.user, fu.password, salt)
		if strings.TrimSuffix(string(pw), "\x00") != want {
			writeMessage(conn, 'E', fakeErrorPayload("password authentication failed"))
			return
		}
	case authSASL:
		if err := fu.handleSCRAM(conn); err != nil {
			return
		}
	default:
		// trust: no password needed at all.
	}

	if err := writeAuthenticationOK(conn); err != nil {
		return
	}
	if err := writeMessage(conn, 'S', append(cstring("server_version"), cstring("16.0")...)); err != nil {
		return
	}
	if err := writeMessage(conn, 'K', backendKeyDataPayload(1234, 5678)); err != nil {
		return
	}
	if err := writeMessage(conn, 'Z', []byte{'I'}); err != nil {
		return
	}

	for {
		typ, _, err := readMessage(conn)
		if err != nil {
			return
		}
		switch typ {
		case 'Q':
			if writeMessage(conn, 'C', cstring("SELECT 1")) != nil {
				return
			}
			if writeMessage(conn, 'Z', []byte{'I'}) != nil {
				return
			}
		case 'X':
			return
		}
	}
}

// handleSCRAM is the server half of RFC 5802/7677, independent of
// Serve's client half: it proves the client's SCRAM-SHA-256 exchange is
// wire-correct by validating the proof against fu.password itself,
// rather than trusting pgrelay's own math.
func (fu *fakeUpstream) handleSCRAM(conn net.Conn) error {
	list := make([]byte, 4)
	binary.BigEndian.PutUint32(list, authSASL)
	list = append(list, cstring(scramMechanism)...)
	list = append(list, 0)
	if err := writeMessage(conn, 'R', list); err != nil {
		return err
	}

	typ, payload, err := readMessage(conn)
	if err != nil {
		return err
	}
	if typ != 'p' {
		return fmt.Errorf("want SASLInitialResponse, got %q", typ)
	}
	mech, idx, err := readCString(payload, 0)
	if err != nil || !strings.EqualFold(mech, scramMechanism) {
		return fmt.Errorf("unexpected mechanism %q", mech)
	}
	if idx+4 > len(payload) {
		return errors.New("short SASLInitialResponse")
	}
	dataLen := int(binary.BigEndian.Uint32(payload[idx : idx+4]))
	if idx+4+dataLen > len(payload) {
		return errors.New("short SASLInitialResponse data")
	}
	clientFirst := string(payload[idx+4 : idx+4+dataLen])
	if !strings.HasPrefix(clientFirst, gs2HeaderNoBinding) {
		return fmt.Errorf("client-first-message has no gs2 header: %q", clientFirst)
	}
	clientFirstBare := strings.TrimPrefix(clientFirst, gs2HeaderNoBinding)
	clientNonce := parseSCRAMFields(clientFirstBare)["r"]
	if clientNonce == "" {
		return errors.New("client-first-message has no nonce")
	}

	salt := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x11, 0x22}
	iterations := 4096
	serverNonce := clientNonce + "fakeupstreamserver"
	serverFirst := "r=" + serverNonce + ",s=" + base64.StdEncoding.EncodeToString(salt) + ",i=" + strconv.Itoa(iterations)

	cont := make([]byte, 4)
	binary.BigEndian.PutUint32(cont, authSASLContinue)
	cont = append(cont, []byte(serverFirst)...)
	if err := writeMessage(conn, 'R', cont); err != nil {
		return err
	}

	typ, payload, err = readMessage(conn)
	if err != nil {
		return err
	}
	if typ != 'p' {
		return fmt.Errorf("want SASLResponse, got %q", typ)
	}
	clientFinal := string(payload)
	pIdx := strings.Index(clientFinal, ",p=")
	if pIdx < 0 {
		return errors.New("client-final-message has no proof")
	}
	clientFinalWithoutProof := clientFinal[:pIdx]
	gotProof, err := base64.StdEncoding.DecodeString(clientFinal[pIdx+3:])
	if err != nil {
		return fmt.Errorf("client proof: %w", err)
	}

	saltedPassword := hi(fu.password, salt, iterations)
	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof
	wantProof := xorBytes(clientKey, hmacSHA256(storedKey[:], []byte(authMessage)))
	if !hmac.Equal(gotProof, wantProof) {
		writeMessage(conn, 'E', fakeErrorPayload("SCRAM proof mismatch"))
		return errors.New("client proof did not verify")
	}

	serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))
	serverSignature := hmacSHA256(serverKey, []byte(authMessage))
	final := make([]byte, 4)
	binary.BigEndian.PutUint32(final, authSASLFinal)
	final = append(final, []byte("v="+base64.StdEncoding.EncodeToString(serverSignature))...)
	return writeMessage(conn, 'R', final)
}

func backendKeyDataPayload(pid, secret uint32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[:4], pid)
	binary.BigEndian.PutUint32(b[4:], secret)
	return b
}

func fakeErrorPayload(msg string) []byte {
	var buf []byte
	buf = append(buf, 'S')
	buf = append(buf, cstring("ERROR")...)
	buf = append(buf, 'M')
	buf = append(buf, cstring(msg)...)
	buf = append(buf, 0)
	return buf
}

// --- the relay under test, and a raw client driving it over TCP ---

func startRelay(t *testing.T, cred Credentials, upstreamAddr string) net.Conn {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		upstream, err := net.Dial("tcp", upstreamAddr)
		if err != nil {
			conn.Close()
			return
		}
		Serve(context.Background(), conn, upstream, cred)
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func sendClientStartupMessage(t *testing.T, conn net.Conn, params map[string]string) {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, protocolVersion3)
	for k, v := range params {
		body = append(body, cstring(k)...)
		body = append(body, cstring(v)...)
	}
	body = append(body, 0)
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)))
	copy(out[4:], body)
	if _, err := conn.Write(out); err != nil {
		t.Fatalf("send startup: %v", err)
	}
}

func sendSSLRequest(t *testing.T, conn net.Conn) {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out[:4], 8)
	binary.BigEndian.PutUint32(out[4:], sslRequestCode)
	if _, err := conn.Write(out); err != nil {
		t.Fatalf("send SSLRequest: %v", err)
	}
}

// expectLoginSuccess reads the relay's AuthenticationOk — sent with no
// password ever asked of the client — then the forwarded startup
// messages up to ReadyForQuery.
func expectLoginSuccess(t *testing.T, conn net.Conn) {
	typ, payload, err := readMessage(conn)
	if err != nil {
		t.Fatalf("read auth response: %v", err)
	}
	if typ != 'R' || len(payload) < 4 || binary.BigEndian.Uint32(payload) != authOK {
		t.Fatalf("got message %q %v, want AuthenticationOk", typ, payload)
	}
	for {
		typ, payload, err := readMessage(conn)
		if err != nil {
			t.Fatalf("read startup completion: %v", err)
		}
		switch typ {
		case 'Z':
			return
		case 'E':
			t.Fatalf("upstream error forwarded to client: %s", payload)
		}
	}
}

func expectTrivialQuery(t *testing.T, conn net.Conn) {
	if err := writeMessage(conn, 'Q', cstring("SELECT 1")); err != nil {
		t.Fatalf("send query: %v", err)
	}
	sawCommandComplete := false
	for {
		typ, _, err := readMessage(conn)
		if err != nil {
			t.Fatalf("read query response: %v", err)
		}
		if typ == 'C' {
			sawCommandComplete = true
		}
		if typ == 'Z' {
			break
		}
	}
	if !sawCommandComplete {
		t.Fatal("no CommandComplete came back over the spliced connection")
	}
}

func TestServeAuthenticatesEachUpstreamMethod(t *testing.T) {
	cases := []struct {
		name   string
		method byte
	}{
		{"cleartext", authCleartext},
		{"md5", authMD5},
		{"scram-sha-256", authSASL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fu := startFakeUpstream(t, "app", "s3cret", tc.method)
			client := startRelay(t, Credentials{User: "app", Password: "s3cret", Database: "appdb"}, fu.addr())

			sendClientStartupMessage(t, client, map[string]string{"user": "whoever", "database": "appdb"})
			expectLoginSuccess(t, client)
			expectTrivialQuery(t, client)
		})
	}
}

// TestServeNeverAsksTheClientForAPassword proves the relay authenticates
// to upstream (here, with the strongest method, SCRAM-SHA-256) entirely
// on its own Credentials: the client sends only its startup message,
// never a PasswordMessage, and still gets a working proxied connection.
func TestServeNeverAsksTheClientForAPassword(t *testing.T) {
	fu := startFakeUpstream(t, "app", "s3cret", authSASL)
	client := startRelay(t, Credentials{User: "app", Password: "s3cret", Database: "appdb"}, fu.addr())

	sendClientStartupMessage(t, client, map[string]string{"user": "whoever"})
	expectLoginSuccess(t, client)
	expectTrivialQuery(t, client)
}

func TestServeRefusesSSLRequest(t *testing.T) {
	fu := startFakeUpstream(t, "app", "s3cret", authOK)
	client := startRelay(t, Credentials{User: "app", Password: "s3cret"}, fu.addr())

	sendSSLRequest(t, client)
	var reply [1]byte
	if _, err := io.ReadFull(client, reply[:]); err != nil {
		t.Fatalf("read SSL refusal: %v", err)
	}
	if reply[0] != 'N' {
		t.Fatalf("got %q, want 'N'", reply[0])
	}

	// The connection is still usable afterward, as a client falling
	// back to plaintext would expect.
	sendClientStartupMessage(t, client, map[string]string{"user": "whoever"})
	expectLoginSuccess(t, client)
}
