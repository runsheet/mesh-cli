package relaysidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/githarden"
)

// One enforcement point for every push (projectmesh-io/api ADR 001
// amendment "relay sidecar"): the node's push tool holds no git token
// and checks no push rule. It asks the sidecar, on the sidecar's control
// socket, to push a commit of the checkout to a ref; the sidecar pushes
// it through its own /git/ route — the same relay, the same rules, the
// same words as the session's own `git push` — and answers the ref and
// commit the vendor took, or the relay's refusal. The node signs that.

// pushTimeout bounds one push.
const pushTimeout = 5 * time.Minute

// PushRequest is the push tool's ask: a commit of the checkout and the
// ref to push it to.
type PushRequest struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// PushResult is the sidecar's answer: Pushed, or Refused with the
// relay's reason, or Failed with git's last words.
type PushResult struct {
	Pushed  bool   `json:"pushed"`
	Ref     string `json:"ref"`
	SHA     string `json:"sha"`
	Refused string `json:"refused,omitempty"`
	Failed  string `json:"failed,omitempty"`
}

func (s *Sidecar) servePush(w http.ResponseWriter, r *http.Request) {
	var req PushRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, controlBodyMax)).Decode(&req); err != nil {
		http.Error(w, "not a push", http.StatusBadRequest)
		return
	}
	res := s.Push(r.Context(), req)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// Push pushes req.SHA of the checkout to req.Ref through the sidecar's
// own git route.
func (s *Sidecar) Push(ctx context.Context, req PushRequest) PushResult {
	res := PushResult{Ref: req.Ref, SHA: req.SHA}
	fail := func(why string) PushResult { res.Failed = why; return res }
	g := s.creds.Git
	if g == nil || s.relay == nil {
		return fail("this lease has no repository")
	}
	if g.Checkout == "" {
		return fail("the sidecar has no checkout to push from")
	}
	if !strings.HasPrefix(req.Ref, "refs/") || strings.ContainsAny(req.Ref, " \t\n:^~?*[\\") || strings.Contains(req.Ref, "..") {
		return fail(fmt.Sprintf("%q is not a full ref name", req.Ref))
	}
	if !objectID(req.SHA) {
		return fail(fmt.Sprintf("%q is not a commit id", req.SHA))
	}
	base, err := s.base()
	if err != nil {
		return fail(err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	target := base + "/git/" + g.Policy.Repo + ".git"
	start := time.Now()
	cmd := githarden.ForeignCommand(ctx, g.Checkout, nil, "push", "--porcelain", "--no-verify", target, req.SHA+":"+req.Ref)
	out, err := cmd.CombinedOutput()
	if err == nil {
		res.Pushed = true
		return res
	}
	if why := s.refusalSince(start); why != "" {
		res.Refused = why
		return res
	}
	return fail(strings.ReplaceAll(lastLines(string(out), 5), target, "<relay>"))
}

// checkoutAncestry is the relay's fast-forward proof in the read-only
// checkout the sidecar has mounted: `git merge-base --is-ancestor`,
// hardened, in a checkout another user owns.
func checkoutAncestry(dir string) func(ctx context.Context, old, new string) error {
	return func(ctx context.Context, old, new string) error {
		if dir == "" {
			return errors.New("there is no checkout to prove a fast-forward in")
		}
		if !objectID(old) || !objectID(new) {
			return fmt.Errorf("%q to %q is not a pair of commits", old, new)
		}
		out, err := githarden.ForeignCommand(ctx, dir, nil, "merge-base", "--is-ancestor", old, new).CombinedOutput()
		if err == nil {
			return nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return fmt.Errorf("%s does not descend from %s: a force push", shortID(new), shortID(old))
		}
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("cannot prove %s descends from %s in the pushing checkout: %s", shortID(new), shortID(old), lastLines(msg, 1))
	}
}

// objectID says whether s is a full hex object id.
func objectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// PushClient is the node's side of the push control: the push tool's
// way to its lease's sidecar.
type PushClient struct {
	// Socket is the sidecar's control socket.
	Socket string
}

// Push asks the sidecar to push sha to ref. An error is the sidecar not
// answering; a refusal or a failed push is in the result.
func (c PushClient) Push(ctx context.Context, ref, sha string) (PushResult, error) {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", c.Socket)
	}}}
	b, _ := json.Marshal(PushRequest{Ref: ref, SHA: sha})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://sidecar"+PushPath, bytes.NewReader(b))
	if err != nil {
		return PushResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return PushResult{}, fmt.Errorf("the lease's relay sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PushResult{}, fmt.Errorf("the lease's relay sidecar answered %d", resp.StatusCode)
	}
	var out PushResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, controlBodyMax)).Decode(&out); err != nil {
		return PushResult{}, fmt.Errorf("the lease's relay sidecar: %w", err)
	}
	if out.Ref != ref || out.SHA != sha {
		return PushResult{}, fmt.Errorf("the lease's relay sidecar answered for %s %s, not %s %s", out.Ref, out.SHA, ref, sha)
	}
	return out, nil
}
