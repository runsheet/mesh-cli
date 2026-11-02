// Package pgrelay is a Postgres wire-protocol login proxy: a client
// connects, the relay logs into the upstream server with its own
// Credentials — never the client's — and once the upstream reaches
// ReadyForQuery the two connections are spliced byte for byte. The
// client is never asked for a password; whatever it sends during its
// own startup is read and discarded. TLS is never offered to the
// client: an SSLRequest (or GSSENCRequest) is refused with a single
// 'N', as a server with no TLS support would.
//
// Serve speaks the upstream side of the handshake for cleartext, MD5
// and SCRAM-SHA-256 password authentication (whichever the upstream
// asks for) and nothing else; an upstream that asks for another method
// fails the connection.
package pgrelay

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// Credentials are what the relay presents to the upstream server. The
// client's own startup parameters and any password it sends are never
// used to authenticate upstream.
type Credentials struct {
	// User is the role the relay logs in as.
	User string
	// Password authenticates User, in whatever form the upstream's
	// chosen method needs; computed here, never sent in the clear
	// unless the upstream itself asked for cleartext.
	Password string
	// Database is the database named in the upstream startup message.
	// Empty uses the database the client's own startup named, if any.
	Database string
}

const (
	protocolVersion3  = 0x00030000
	sslRequestCode    = 80877103
	gssEncRequestCode = 80877104
	maxStartupLen     = 1 << 16
	maxMessageLen     = 1 << 24
	authOK            = 0
	authCleartext     = 3
	authMD5           = 5
	authSASL          = 10
	authSASLContinue  = 11
	authSASLFinal     = 12
	scramMechanism    = "SCRAM-SHA-256"
)

// Serve runs one session: it reads the client's startup (refusing any
// SSL or GSSENC negotiation with 'N'), logs into upstream with cred,
// tells the client the login succeeded, forwards the upstream's own
// startup messages, and then splices client and upstream until either
// side closes or ctx is done. It always closes both connections before
// returning.
func Serve(ctx context.Context, client, upstream net.Conn, cred Credentials) error {
	defer client.Close()
	defer upstream.Close()

	clientParams, err := readClientStartup(client)
	if err != nil {
		return fmt.Errorf("pgrelay: client startup: %w", err)
	}

	database := cred.Database
	if database == "" {
		database = clientParams["database"]
	}
	if err := sendStartup(upstream, cred.User, database); err != nil {
		return fmt.Errorf("pgrelay: upstream startup: %w", err)
	}
	if err := authenticateUpstream(upstream, cred.User, cred.Password); err != nil {
		return fmt.Errorf("pgrelay: upstream auth: %w", err)
	}

	if err := writeAuthenticationOK(client); err != nil {
		return fmt.Errorf("pgrelay: client auth ok: %w", err)
	}
	if err := forwardUntilReady(client, upstream); err != nil {
		return fmt.Errorf("pgrelay: upstream ready: %w", err)
	}

	return splice(ctx, client, upstream)
}

// readClientStartup reads the client's StartupMessage, answering any
// number of leading SSLRequest/GSSENCRequest packets with 'N' (the
// relay never terminates TLS to the client) before the real one.
func readClientStartup(conn net.Conn) (map[string]string, error) {
	for {
		body, err := readStartupPacket(conn)
		if err != nil {
			return nil, err
		}
		if len(body) == 4 {
			switch binary.BigEndian.Uint32(body) {
			case sslRequestCode, gssEncRequestCode:
				if _, err := conn.Write([]byte{'N'}); err != nil {
					return nil, err
				}
				continue
			}
		}
		if len(body) < 4 {
			return nil, fmt.Errorf("short startup message (%d bytes)", len(body))
		}
		version := binary.BigEndian.Uint32(body[:4])
		if version != protocolVersion3 {
			return nil, fmt.Errorf("unsupported protocol version %#x", version)
		}
		return parseStartupParams(body[4:])
	}
}

func readStartupPacket(conn net.Conn) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	if length < 4 || length > maxStartupLen {
		return nil, fmt.Errorf("invalid startup length %d", length)
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	return body, nil
}

func parseStartupParams(body []byte) (map[string]string, error) {
	params := map[string]string{}
	i := 0
	for i < len(body) && body[i] != 0 {
		key, next, err := readCString(body, i)
		if err != nil {
			return nil, err
		}
		val, next2, err := readCString(body, next)
		if err != nil {
			return nil, err
		}
		params[key] = val
		i = next2
	}
	return params, nil
}

func readCString(buf []byte, start int) (string, int, error) {
	end := start
	for end < len(buf) && buf[end] != 0 {
		end++
	}
	if end >= len(buf) {
		return "", 0, errors.New("unterminated string")
	}
	return string(buf[start:end]), end + 1, nil
}

// readMessage reads one tagged post-startup message: a type byte, an
// int32 length (itself included), then length-4 bytes of payload.
func readMessage(conn net.Conn) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length < 4 || length > maxMessageLen {
		return 0, nil, fmt.Errorf("invalid message length %d", length)
	}
	payload := make([]byte, length-4)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

func writeMessage(conn net.Conn, typ byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(payload)))
	copy(buf[5:], payload)
	_, err := conn.Write(buf)
	return err
}

func writeAuthenticationOK(conn net.Conn) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, authOK)
	return writeMessage(conn, 'R', payload)
}

func sendStartup(conn net.Conn, user, database string) error {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, protocolVersion3)
	body = append(body, cstring("user")...)
	body = append(body, cstring(user)...)
	if database != "" {
		body = append(body, cstring("database")...)
		body = append(body, cstring(database)...)
	}
	body = append(body, 0)
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)))
	copy(out[4:], body)
	_, err := conn.Write(out)
	return err
}

func cstring(s string) []byte {
	return append([]byte(s), 0)
}

func sendPassword(conn net.Conn, password string) error {
	return writeMessage(conn, 'p', cstring(password))
}

// authenticateUpstream speaks whichever password method the upstream
// asks for (cleartext, MD5, SCRAM-SHA-256) until the upstream answers
// AuthenticationOk, or fails on anything else.
func authenticateUpstream(conn net.Conn, user, password string) error {
	var scram *scramClient
	for {
		typ, payload, err := readMessage(conn)
		if err != nil {
			return err
		}
		switch typ {
		case 'E':
			return parsePgError(payload)
		case 'N':
			continue // NoticeResponse: informational, keep reading
		case 'R':
			if len(payload) < 4 {
				return errors.New("short authentication message")
			}
			sub := binary.BigEndian.Uint32(payload[:4])
			switch sub {
			case authOK:
				return nil
			case authCleartext:
				if err := sendPassword(conn, password); err != nil {
					return err
				}
			case authMD5:
				if len(payload) < 8 {
					return errors.New("short MD5 salt")
				}
				salt := payload[4:8]
				if err := sendPassword(conn, md5PasswordHash(user, password, salt)); err != nil {
					return err
				}
			case authSASL:
				mechs := parseCStringList(payload[4:])
				if !containsFold(mechs, scramMechanism) {
					return fmt.Errorf("upstream offers no supported SASL mechanism (got %v)", mechs)
				}
				var err error
				scram, err = newSCRAMClient(user, password)
				if err != nil {
					return err
				}
				if err := writeMessage(conn, 'p', scram.initialResponse()); err != nil {
					return err
				}
			case authSASLContinue:
				if scram == nil {
					return errors.New("SASLContinue before SASL started")
				}
				resp, err := scram.continueWith(payload[4:])
				if err != nil {
					return err
				}
				if err := writeMessage(conn, 'p', resp); err != nil {
					return err
				}
			case authSASLFinal:
				if scram == nil {
					return errors.New("SASLFinal before SASL started")
				}
				if err := scram.verifyFinal(payload[4:]); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unsupported authentication method %d", sub)
			}
		default:
			return fmt.Errorf("unexpected message %q during authentication", typ)
		}
	}
}

func parseCStringList(buf []byte) []string {
	var out []string
	i := 0
	for i < len(buf) && buf[i] != 0 {
		s, next, err := readCString(buf, i)
		if err != nil {
			break
		}
		out = append(out, s)
		i = next
	}
	return out
}

func containsFold(ss []string, s string) bool {
	for _, x := range ss {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func md5PasswordHash(user, password string, salt []byte) string {
	inner := md5Hex([]byte(password + user))
	outer := md5Hex(append([]byte(inner), salt...))
	return "md5" + outer
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func parsePgError(payload []byte) error {
	msg := ""
	i := 0
	for i < len(payload) && payload[i] != 0 {
		code := payload[i]
		s, next, err := readCString(payload, i+1)
		if err != nil {
			break
		}
		if code == 'M' {
			msg = s
		}
		i = next
	}
	if msg == "" {
		msg = "upstream refused authentication"
	}
	return fmt.Errorf("%s", msg)
}

// forwardUntilReady forwards every message the upstream sends — the
// post-auth ParameterStatus/BackendKeyData/NoticeResponse messages — to
// the client, stopping once ReadyForQuery is forwarded.
func forwardUntilReady(client, upstream net.Conn) error {
	for {
		typ, payload, err := readMessage(upstream)
		if err != nil {
			return err
		}
		if err := writeMessage(client, typ, payload); err != nil {
			return err
		}
		switch typ {
		case 'Z':
			return nil
		case 'E':
			return parsePgError(payload)
		}
	}
}

// splice copies bytes both ways until one side closes or ctx is done.
func splice(ctx context.Context, a, b net.Conn) error {
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(a, b)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(b, a)
		errc <- err
	}()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			a.Close()
			b.Close()
		case <-done:
		}
	}()

	err := <-errc
	a.Close()
	b.Close()
	<-errc
	return err
}

// --- SCRAM-SHA-256 (RFC 5802 / RFC 7677) client half ---

type scramClient struct {
	password        string
	clientNonce     string
	clientFirstBare string
	serverFirst     string
	saltedPassword  []byte
}

func newSCRAMClient(user, password string) (*scramClient, error) {
	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	return &scramClient{
		password:        password,
		clientNonce:     nonce,
		clientFirstBare: "n=,r=" + nonce,
	}, nil
}

func randomNonce() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

const gs2HeaderNoBinding = "n,,"

func (s *scramClient) initialResponse() []byte {
	clientFirst := gs2HeaderNoBinding + s.clientFirstBare
	payload := cstring(scramMechanism)
	data := []byte(clientFirst)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	payload = append(payload, lenBuf[:]...)
	payload = append(payload, data...)
	return payload
}

func (s *scramClient) continueWith(serverFirst []byte) ([]byte, error) {
	s.serverFirst = string(serverFirst)
	fields := parseSCRAMFields(s.serverFirst)
	nonce, saltB64, iterStr := fields["r"], fields["s"], fields["i"]
	if nonce == "" || saltB64 == "" || iterStr == "" {
		return nil, fmt.Errorf("malformed SCRAM server-first-message %q", s.serverFirst)
	}
	if !strings.HasPrefix(nonce, s.clientNonce) {
		return nil, errors.New("SCRAM server nonce does not extend the client's")
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, fmt.Errorf("SCRAM salt: %w", err)
	}
	iterations, err := strconv.Atoi(iterStr)
	if err != nil || iterations <= 0 {
		return nil, fmt.Errorf("SCRAM iteration count %q", iterStr)
	}

	s.saltedPassword = hi(s.password, salt, iterations)
	clientKey := hmacSHA256(s.saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)

	channelBinding := base64.StdEncoding.EncodeToString([]byte(gs2HeaderNoBinding))
	clientFinalWithoutProof := "c=" + channelBinding + ",r=" + nonce
	authMessage := s.clientFirstBare + "," + s.serverFirst + "," + clientFinalWithoutProof

	clientSignature := hmacSHA256(storedKey[:], []byte(authMessage))
	proof := xorBytes(clientKey, clientSignature)

	clientFinal := clientFinalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
	return []byte(clientFinal), nil
}

func (s *scramClient) verifyFinal(serverFinal []byte) error {
	fields := parseSCRAMFields(string(serverFinal))
	sigB64 := fields["v"]
	if sigB64 == "" {
		return fmt.Errorf("malformed SCRAM server-final-message %q", serverFinal)
	}
	got, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("SCRAM server signature: %w", err)
	}
	serverKey := hmacSHA256(s.saltedPassword, []byte("Server Key"))
	authMessage := s.clientFirstBare + "," + s.serverFirst
	// The client-final-message (without proof) is part of the auth
	// message too; rebuild it the same way continueWith did.
	fields2 := parseSCRAMFields(s.serverFirst)
	nonce := fields2["r"]
	channelBinding := base64.StdEncoding.EncodeToString([]byte(gs2HeaderNoBinding))
	clientFinalWithoutProof := "c=" + channelBinding + ",r=" + nonce
	authMessage += "," + clientFinalWithoutProof
	want := hmacSHA256(serverKey, []byte(authMessage))
	if !hmac.Equal(got, want) {
		return errors.New("SCRAM server signature does not match")
	}
	return nil
}

func parseSCRAMFields(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			out[kv[0]] = kv[1]
		}
	}
	return out
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// hi is RFC 5802's Hi(): PBKDF2 with HMAC-SHA256, one block (dkLen is
// always 32, the HMAC-SHA256 output size).
func hi(password string, salt []byte, iterations int) []byte {
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write(salt)
	mac.Write([]byte{0, 0, 0, 1})
	u := mac.Sum(nil)
	result := make([]byte, len(u))
	copy(result, u)
	for i := 1; i < iterations; i++ {
		mac.Reset()
		mac.Write(u)
		u = mac.Sum(nil)
		for j := range result {
			result[j] ^= u[j]
		}
	}
	return result
}

func xorBytes(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}
