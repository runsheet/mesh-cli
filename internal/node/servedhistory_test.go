package node

import (
	"fmt"
	"os"

	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gitdata"
)

// The served-diff scratch carries the served commit's own history, one
// commit deep: runsheet's scan suite takes `git rev-parse HEAD` and
// `HEAD^` and runs gitleaks over HEAD^..HEAD, and failed with exit 128 on
// every served validation while HEAD was a parentless synthetic commit
// (runsheet #5153 and #5154, 2026-10-25).

// servedHistoryRead is what servedYML's api suite wrote to
// $SERVED_HISTORY_OUT from inside the scratch.
type servedHistoryRead struct {
	head, parent, parentTree string
	envHead, envBase         string
	log                      int
	changed                  []string
}

func readServedHistory(t *testing.T, path, logs string) servedHistoryRead {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the suite wrote no history: %v\n%s", err, logs)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 5 {
		t.Fatalf("history = %q", data)
	}
	var h servedHistoryRead
	h.head, h.parent, h.parentTree = lines[0], lines[1], lines[2]
	env := strings.Fields(lines[3])
	if len(env) != 3 || env[0] != "env" {
		t.Fatalf("env line %q, want HEAD_SHA and BASE_SHA", lines[3])
	}
	h.envHead, h.envBase = env[1], env[2]
	if _, err := fmt.Sscanf(strings.Join(strings.Fields(lines[4]), " "), "log %d", &h.log); err != nil {
		t.Fatalf("log line %q: %v", lines[4], err)
	}
	h.changed = lines[5:]
	return h
}

func (h servedHistoryRead) assertChange(t *testing.T, want ...string) {
	t.Helper()
	if h.log != 1 {
		t.Fatalf("git log --oneline HEAD^..HEAD has %d line(s), want 1", h.log)
	}
	if strings.Join(h.changed, ",") != strings.Join(want, ",") {
		t.Fatalf("git diff --name-only HEAD^ HEAD = %v, want %v", h.changed, want)
	}
}

// commitText rebuilds a signed commit from its payload and signature and
// an unsigned one from its fields, and refuses what does not hash.
func TestCommitTextRebuildsTheObject(t *testing.T) {
	payload := "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@x> 1761400000 +1100\ncommitter A <a@x> 1761400000 +1100\n\nmsg\n"
	sig := "-----BEGIN PGP SIGNATURE-----\n\nwsBc\n-----END PGP SIGNATURE-----"
	signed := "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@x> 1761400000 +1100\ncommitter A <a@x> 1761400000 +1100\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n wsBc\n -----END PGP SIGNATURE-----\n\nmsg\n"
	sha := gitObjectID("commit", []byte(signed), 40)
	// GitHub's signature may or may not end in a newline
	for _, s := range []string{sig, sig + "\n"} {
		got, err := commitText(gitdata.Commit{SHA: sha, Payload: payload, Signature: s})
		if err != nil || got != signed {
			t.Fatalf("signed commitText = %q, %v", got, err)
		}
	}
	// GitHub's own signature (web-flow: a squash merge on main, a merge
	// batch candidate made through the API) is served ending in one
	// newline and sits in the object followed by a lone-space line, as
	// runsheet's c2997d424 and every candidate of 2026-10-25 do
	webFlow := "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@x> 1761400000 +1100\ncommitter GitHub <noreply@github.com> 1761400000 +1100\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n wsBc\n -----END PGP SIGNATURE-----\n \n\nmsg\n"
	webPayload := "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@x> 1761400000 +1100\ncommitter GitHub <noreply@github.com> 1761400000 +1100\n\nmsg\n"
	if got, err := commitText(gitdata.Commit{SHA: gitObjectID("commit", []byte(webFlow), 40), Payload: webPayload, Signature: sig + "\n"}); err != nil || got != webFlow {
		t.Fatalf("GitHub-signed commitText = %q, %v", got, err)
	}
	unsigned := gitdata.Commit{
		SHA: gitObjectID("commit", []byte(payload), 40), TreeSHA: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		Author:    gitdata.Person{Name: "A", Email: "a@x", Date: time.Unix(1761400000, 0).UTC()},
		Committer: gitdata.Person{Name: "A", Email: "a@x", Date: time.Unix(1761400000, 0).UTC()},
		Message:   "msg",
	}
	if got, err := commitText(unsigned); err != nil || got != payload {
		t.Fatalf("unsigned commitText = %q, %v", got, err)
	}
	unsigned.SHA = strings.Repeat("ab", 20)
	if _, err := commitText(unsigned); err == nil {
		t.Fatal("a commit whose fields do not hash to its sha was rebuilt")
	}
}
