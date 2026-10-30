package gitrelay

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// A relay sidecar's fetches from the node's mirror (projectmesh-io/api
// ADR 001 amendment, relay sidecar: "Mirrors stay with the node"). The
// node bind-mounts its mirror of the lease's tenant's repository into the
// sidecar, read-only; a fetch whose every want the mirror already holds
// is answered from it by the sidecar's own git, and anything else goes
// upstream through the lease's tunnel as before. The ref advertisement
// (info/refs, and protocol v2's ls-refs) always comes from upstream, so
// what the session sees is the vendor's now; only the pack, the costly
// part, may come from the mirror. The node never runs git for the pod:
// the sidecar runs it, on a mirror it can only read.

// mirrorBodyMax bounds the fetch negotiation read to decide: a body
// longer than this goes upstream, streamed, unread past it.
const mirrorBodyMax = 4 << 20

// oidLine is a want line's object id.
var oidLine = regexp.MustCompile(`^want ([0-9a-f]{40}|[0-9a-f]{64})(\s|$)`)

// MirrorGit runs git in the sidecar; "git" on PATH when empty.
var MirrorGit = "git"

// fromMirror serves an upload-pack POST from mirror when every object
// it wants is there, and answers whether it did. When it did not, body
// is what the request carried, to send upstream whole: the bytes read
// to decide and the rest still unread.
func (r *Relay) fromMirror(w http.ResponseWriter, req *http.Request, mirror string, body io.Reader, ev *Event) (bool, io.Reader) {
	head, err := io.ReadAll(io.LimitReader(body, mirrorBodyMax+1))
	replay := io.MultiReader(bytes.NewReader(head), body)
	if err != nil || len(head) > mirrorBodyMax {
		return false, replay
	}
	plain := head
	if strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(head))
		if err != nil {
			return false, replay
		}
		plain, err = io.ReadAll(io.LimitReader(zr, 4*mirrorBodyMax))
		if err != nil {
			return false, replay
		}
	}
	wants, fetch := wantsOf(plain)
	if !fetch || len(wants) == 0 || !mirrorHas(req.Context(), mirror, wants) {
		return false, replay
	}
	cmd := exec.CommandContext(req.Context(), MirrorGit, "-c", "safe.directory="+mirror,
		"-c", "uploadpack.allowAnySHA1InWant=true", "upload-pack", "--stateless-rpc", mirror)
	cmd.Env = mirrorEnv(req.Header.Get("Git-Protocol"))
	cmd.Stdin = bytes.NewReader(plain)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, replay
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return false, replay
	}
	br := bufio.NewReader(stdout)
	// nothing is answered until git has said something: a git that
	// fails at once leaves the request to go upstream
	if _, err := br.Peek(4); err != nil {
		_ = cmd.Wait()
		return false, replay
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	n, _ := io.Copy(flushWriter{w}, br)
	werr := cmd.Wait()
	ev.Bytes, ev.Outcome, ev.Reason = n, "ok", "from the node's mirror"
	if werr != nil {
		ev.Outcome, ev.Reason = "failed", "the mirror's upload-pack: "+oneLine(stderr.String())
	}
	r.audit(*ev)
	return true, nil
}

// wantsOf reads an upload-pack request's wants: protocol v2's fetch
// command's, or v0's want lines. fetch is false for a request that is
// not a fetch (v2's ls-refs, which upstream answers).
func wantsOf(body []byte) (wants []string, fetch bool) {
	fetch = true
	for len(body) >= 4 {
		var size int
		if _, err := fmt.Sscanf(string(body[:4]), "%04x", &size); err != nil {
			return nil, false
		}
		if size < 4 { // flush, delim, response-end
			body = body[4:]
			continue
		}
		if size > len(body) {
			return nil, false
		}
		line := strings.TrimRight(string(body[4:size]), "\n")
		body = body[size:]
		switch {
		case strings.HasPrefix(line, "command="):
			fetch = line == "command=fetch"
		case strings.HasPrefix(line, "shallow ") || strings.HasPrefix(line, "deepen"):
			// a shallow history is upstream's to compute
			return nil, false
		default:
			if m := oidLine.FindStringSubmatch(line); m != nil {
				wants = append(wants, m[1])
			}
		}
	}
	return wants, fetch
}

// mirrorHas says whether mirror holds every object of wants.
func mirrorHas(ctx context.Context, mirror string, wants []string) bool {
	if fi, err := os.Stat(mirror); err != nil || !fi.IsDir() {
		return false
	}
	cmd := exec.CommandContext(ctx, MirrorGit, "-c", "safe.directory="+mirror, "--git-dir", mirror, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	cmd.Env = mirrorEnv("")
	cmd.Stdin = strings.NewReader(strings.Join(wants, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(wants) {
		return false
	}
	for _, l := range lines {
		if strings.HasSuffix(l, " missing") || !strings.Contains(l, " ") {
			return false
		}
	}
	return true
}

// mirrorEnv is the mirror's git's environment: nothing of the
// sidecar's but its PATH, no config but the command line's, and the
// client's protocol.
func mirrorEnv(protocol string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	if protocol != "" && !strings.ContainsAny(protocol, "\r\n") {
		env = append(env, "GIT_PROTOCOL="+protocol)
	}
	return env
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
