package gitrelay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const pushLeak = "injected-value-0123456789abcdef"

// A push whose content holds an injected value — a file's, a delta's
// inserted bytes, a commit message — is refused before the vendor can
// accept it, and the ref does not move; a clean push, large or with push
// options, goes through whole (mesh-specs gap 28 item 5).
func TestAPushCarryingAnInjectedValueIsRefused(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path: " + err.Error())
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}
	root := t.TempDir()
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := git(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	served := filepath.Join(root, "served")
	os.MkdirAll(filepath.Join(served, "acme"), 0o755)
	bare := filepath.Join(served, "acme", "app.git")
	run(root, "init", "-q", "--bare", "-b", "main", bare)
	run(bare, "config", "http.receivepack", "true")
	run(bare, "config", "receive.advertisePushOptions", "true")
	backendCGI := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x"}}
	// the body is had whole before git sees it, as a vendor's front
	// does: git-http-backend spins on a body cut short
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "the body was cut short", http.StatusBadRequest)
			return
		}
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
		r.Header.Del("Transfer-Encoding")
		backendCGI.ServeHTTP(w, r)
	}))
	defer gh.Close()

	var mu sync.Mutex
	var found []string
	scanned := 0
	relay := &Relay{
		Policy:   Policy{Repo: "acme/app", Access: Write, Push: []RefRule{{Ref: "refs/heads/*"}}},
		Upstream: gh.URL,
		Audit:    func(e Event) { t.Logf("relay: %s %s %s", e.Service, e.Outcome, e.Reason) },
		Mint: func(context.Context, string, Access) (Token, error) {
			return Token{Value: "job-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
		},
	}
	relay.SetScan(func(where, toward string, b []byte) error {
		mu.Lock()
		defer mu.Unlock()
		scanned += len(b)
		if bytes.Contains(b, []byte(pushLeak)) {
			found = append(found, where+" toward "+toward)
			return errors.New("credential_exposed")
		}
		return nil
	})
	work := filepath.Join(root, "work")
	relay.Bases = CatFileBases(work)
	front := httptest.NewServer(relay)
	defer front.Close()

	run(root, "init", "-q", "-b", "main", work)
	run(work, "remote", "add", "origin", front.URL+"/git/acme/app.git")
	// a large file of text lines, so a later change is sent as a delta
	var big strings.Builder
	for i := range 40000 {
		b := make([]byte, 16)
		rand.Read(b)
		big.WriteString(hex.EncodeToString(b) + " line " + string(rune('a'+i%26)) + "\n")
	}
	os.WriteFile(filepath.Join(work, "big.txt"), []byte(big.String()), 0o644)
	run(work, "add", ".")
	run(work, "commit", "-qm", "one")
	run(work, "push", "-q", "-o", "ci.skip", "origin", "main")
	if run(bare, "rev-parse", "main") != run(work, "rev-parse", "HEAD") {
		t.Fatal("a clean push did not land")
	}
	mu.Lock()
	if scanned < big.Len() || len(found) != 0 {
		t.Fatalf("a clean push: %d bytes scanned, found %q", scanned, found)
	}
	mu.Unlock()

	attempt := func(name string, change func()) {
		t.Helper()
		before := run(bare, "rev-parse", "main")
		change()
		if out, err := git(work, "push", "origin", "main"); err == nil {
			t.Fatalf("%s: the push went through:\n%s", name, out)
		}
		if run(bare, "rev-parse", "main") != before {
			t.Fatalf("%s: main moved", name)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(found) != 1 || found[0] != "a git push's content toward acme/app" {
			t.Fatalf("%s: found %q", name, found)
		}
		found = nil
		run(work, "reset", "-q", "--hard", before)
	}
	attempt("a delta's insert", func() {
		s := big.String()
		mid := len(s) / 2
		os.WriteFile(filepath.Join(work, "big.txt"), []byte(s[:mid]+"key="+pushLeak+"\n"+s[mid:]), 0o644)
		run(work, "commit", "-qam", "two")
	})
	attempt("a commit message", func() {
		os.WriteFile(filepath.Join(work, "small.txt"), []byte("clean\n"), 0o644)
		run(work, "add", ".")
		run(work, "commit", "-qm", "the key is "+pushLeak)
	})
	attempt("a new file", func() {
		os.WriteFile(filepath.Join(work, "leak.env"), []byte("KEY="+pushLeak+"\n"), 0o644)
		run(work, "add", ".")
		run(work, "commit", "-qm", "three")
	})
}

// A value split across two of the scanner's chunks is found.
func TestChunkScanFindsASplitValue(t *testing.T) {
	var hit bool
	cs := &chunkScan{scan: func(b []byte) error {
		if bytes.Contains(b, []byte(pushLeak)) {
			hit = true
			return errors.New("found")
		}
		return nil
	}}
	cs.Write([]byte(strings.Repeat("x", packChunk-5) + pushLeak[:10]))
	cs.Write([]byte(pushLeak[10:] + "tail"))
	if err := cs.flush(); err == nil || !hit || !errors.Is(err, ErrPushExposed) {
		t.Fatalf("err %v hit %v", err, hit)
	}
}

// A pack the relay cannot read is refused, not passed unread.
func TestAnUnreadablePackIsRefused(t *testing.T) {
	err := readPack(context.Background(), strings.NewReader("PACK\x00\x00\x00\x02\x00\x00\x00\x01\xff\xff"), func([]byte) error { return nil }, nil)
	var unread *errPackUnread
	if !errors.As(err, &unread) {
		t.Fatalf("err %v", err)
	}
	if err := readPack(context.Background(), strings.NewReader(""), func([]byte) error { return nil }, nil); err != nil {
		t.Fatalf("a push with no pack: %v", err)
	}
}
