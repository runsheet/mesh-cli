package nodev1

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/secreport"
)

// The values a lease's relay injects are redacted wherever they might
// echo and are the lease's canaries (mesh-specs gap 28 item 5, gap 5
// item 4; ADR-046). Each credential is registered here as it is taken,
// before its first use, in every form it could be seen in: the value,
// its base64 (standard and URL alphabets, padded or not, and the stable
// middle of each at the two other alignments, so a value inside a longer
// base64 run is found too), its URL-encoded forms, and a Basic header's
// user:pass. Every form reads [secret:<ref>]:
//
//   - in every line of the node's log (Runtime.logf), the reasons it
//     sends Mesh (fail, hand-back), the transcript chunks it signs for
//     the product (transcript.Options.Named), the session's errors and
//     stderr (SessionSpec.Redact), so the stop rules' and tools' lines
//     too, which reach those;
//   - as a canary: seen in the URL or a header of a request toward a
//     host outside its rule, or in the body of any request through the
//     lease's tunnel or its model relay, it ends the job: the request is
//     refused 403 credential_exposed, the session stopped, Latchkey told
//     of a Secret's exposed version before the session's token ends
//     (release.go), and the lease failed credential_exposed with the ref
//     and host in the reason (stopped_by_rule on a Mesh older than
//     projectmesh-io/api #83).

// minInjectedValue is the shortest value redacted or watched: a shorter
// one (a username such as "acme") would redact ordinary words and kill
// jobs for them.
const minInjectedValue = 8

// injectedValues are one lease's injected values, by form.
type injectedValues struct {
	lease string
	logf  func(string, ...any)

	mu      sync.RWMutex
	forms   []string          // longest first
	label   map[string]string // form → [secret:<ref>]
	refOf   map[string]string // form → ref
	version map[string]string // ref → the version last taken
	spec    map[string]egress.InjectSpec
	longest int
	// exposed, once one is, is the first exposure; stop ends the
	// session, set once it runs
	exposed *credentialExposed
	stop    func(error)
	// rel ends the lease's carried credentials (release.go)
	rel *releaser
}

func newInjectedValues(lease string, logf func(string, ...any)) *injectedValues {
	return &injectedValues{lease: lease, logf: logf, label: map[string]string{}, refOf: map[string]string{},
		version: map[string]string{}, spec: map[string]egress.InjectSpec{}}
}

// Mark is what a value of ref reads as wherever it is redacted.
func Mark(ref string) string { return "[secret:" + ref + "]" }

// add registers a credential taken for ref: every field's value and the
// header values its rule writes, in every form. Called before the
// credential is first used.
func (v *injectedValues) add(ref string, c *egress.Credential) {
	if v == nil || c == nil {
		return
	}
	var values []string
	for _, f := range c.Fields {
		values = append(values, f)
	}
	if c.Spec.FormOf() == egress.FormBasic {
		if headers, _, err := c.Writes(); err == nil {
			if b, ok := strings.CutPrefix(headers["Authorization"], "Basic "); ok {
				values = append(values, b)
				if dec, err := base64.StdEncoding.DecodeString(b); err == nil {
					values = append(values, string(dec))
				}
			}
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.version[ref], v.spec[ref] = c.Version, c.Spec
	for _, val := range values {
		if len(val) < minInjectedValue {
			continue
		}
		for _, f := range valueForms(val) {
			if len(f) < minInjectedValue {
				continue
			}
			if _, ok := v.label[f]; !ok {
				v.forms = append(v.forms, f)
			}
			v.label[f], v.refOf[f] = Mark(ref), ref
			v.longest = max(v.longest, len(f))
		}
	}
	sort.SliceStable(v.forms, func(i, j int) bool { return len(v.forms[i]) > len(v.forms[j]) })
}

// valueForms are the forms a value could be seen in.
func valueForms(val string) []string {
	out := []string{val, url.QueryEscape(val), url.PathEscape(val)}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		out = append(out, enc.EncodeToString([]byte(val)))
		raw := enc.WithPadding(base64.NoPadding)
		// the stable middle of the value's base64 at each alignment
		// within a longer run: the characters no neighbouring byte
		// touches
		for k := 0; k < 3; k++ {
			padded := append(make([]byte, k), val...)
			s := raw.EncodeToString(padded)
			from, to := (k*8+5)/6, len(padded)*8/6
			if to > from {
				out = append(out, s[from:to])
			}
		}
	}
	seen := map[string]bool{}
	var uniq []string
	for _, f := range out {
		if f != "" && !seen[f] {
			seen[f] = true
			uniq = append(uniq, f)
		}
	}
	return uniq
}

// Redact is s with every registered form as its mark.
func (v *injectedValues) Redact(s string) string {
	if v == nil {
		return s
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, f := range v.forms {
		if strings.Contains(s, f) {
			s = strings.ReplaceAll(s, f, v.label[f])
		}
	}
	return s
}

// Named is every form and its mark, for the transcript's scrubber.
func (v *injectedValues) Named() map[string]string {
	if v == nil {
		return nil
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make(map[string]string, len(v.label))
	for f, l := range v.label {
		out[f] = l
	}
	return out
}

// window is the longest form's length: what a stream holds back.
func (v *injectedValues) window() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.longest
}

// find is the ref of the first form b holds.
func (v *injectedValues) find(b []byte) (string, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, f := range v.forms {
		if strings.Contains(string(b), f) {
			return v.refOf[f], true
		}
	}
	return "", false
}

// credentialExposed is a value seen where it must not go: the cause the
// session is stopped with.
type credentialExposed struct {
	ref, version, host, where string
}

func (e *credentialExposed) Error() string {
	v := ""
	if e.version != "" {
		v = " (version " + e.version + ")"
	}
	return fmt.Sprintf("%s: the value of %s%s was seen in %s toward %s", egress.CodeExposed, e.ref, v, e.where, e.host)
}

// expose records the lease's first exposure and stops its session, now
// or as soon as it runs.
func (v *injectedValues) expose(ref, host, where string) {
	v.mu.Lock()
	if v.exposed != nil {
		v.mu.Unlock()
		return
	}
	e := &credentialExposed{ref: ref, version: v.version[ref], host: host, where: where}
	v.exposed = e
	stop := v.stop
	v.mu.Unlock()
	v.logf("nodev1: lease %s: %s; the job is ended", v.lease, e)
	if stop != nil {
		stop(e)
	}
}

// onExposed has an exposure stop the session with stop; one already
// seen stops it at once.
func (v *injectedValues) onExposed(stop func(error)) {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.stop = stop
	e := v.exposed
	v.mu.Unlock()
	if e != nil {
		stop(e)
	}
}

// exposure is the lease's exposure, nil when none.
func (v *injectedValues) exposure() *credentialExposed {
	if v == nil {
		return nil
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.exposed
}

// Inspect looks for a value in req's URL and headers as the session
// sent them: one toward a host outside its rule is an exposure.
func (v *injectedValues) Inspect(req *http.Request) error {
	host := req.URL.Hostname()
	seen := req.URL.String()
	for k, vals := range req.Header {
		seen += "\n" + k + ": " + strings.Join(vals, ", ")
	}
	ref, ok := v.find([]byte(seen))
	if !ok {
		return nil
	}
	v.mu.RLock()
	spec := v.spec[ref]
	v.mu.RUnlock()
	if spec.MatchesHost(host) {
		// its own host: the rule refuses it as preset if it is what
		// the rule writes
		return nil
	}
	v.expose(ref, host, "a request's URL or headers")
	return &egress.Refusal{Status: http.StatusForbidden, Code: egress.CodeExposed, Message: "the request carried a credential the relay injects"}
}

// Body scans req's body as it streams.
func (v *injectedValues) Body(req *http.Request, body io.ReadCloser) io.ReadCloser {
	host := req.URL.Hostname()
	return egress.ScanBody(body, v.find, v.window, func(ref string) { v.expose(ref, host, "a request body") })
}

// inspectModel scans a model request's body, held whole by the relay.
func (v *injectedValues) inspectModel(body []byte) error {
	if v == nil {
		return nil
	}
	if ref, ok := v.find(body); ok {
		v.expose(ref, "the model", "a model request's body")
		return fmt.Errorf("%s: the request carried a credential the relay injects", egress.CodeExposed)
	}
	return nil
}

// streamRedact is w with every registered form redacted, a form split
// across writes included: the last window()-1 bytes are held until the
// next write or Flush.
type streamRedact struct {
	v    *injectedValues
	w    io.Writer
	mu   sync.Mutex
	held []byte
}

func (s *streamRedact) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := []byte(s.v.Redact(string(append(s.held, p...))))
	keep := min(max(s.v.window()-1, 0), len(buf))
	out := buf[:len(buf)-keep]
	s.held = append([]byte(nil), buf[len(buf)-keep:]...)
	if len(out) > 0 {
		if _, err := s.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes what is held.
func (s *streamRedact) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.held) == 0 {
		return nil
	}
	_, err := s.w.Write([]byte(s.v.Redact(string(s.held))))
	s.held = nil
	return err
}

// openInjected registers the lease's values with the runtime, whose log
// and reports redact them until closeInjected.
func (r *Runtime) openInjected(lease string) *injectedValues {
	v := newInjectedValues(lease, r.logf)
	r.injectedMu.Lock()
	r.injected = append(r.injected, v)
	r.injectedMu.Unlock()
	return v
}

func (r *Runtime) closeInjected(v *injectedValues) {
	r.injectedMu.Lock()
	defer r.injectedMu.Unlock()
	for i, x := range r.injected {
		if x == v {
			r.injected = append(r.injected[:i], r.injected[i+1:]...)
			return
		}
	}
}

// redact is s with every live lease's injected values redacted.
func (r *Runtime) redact(s string) string {
	r.injectedMu.Lock()
	vs := append([]*injectedValues(nil), r.injected...)
	r.injectedMu.Unlock()
	for _, v := range vs {
		s = v.Redact(s)
	}
	return s
}

// injectedOf is lease id's values, nil when it has none.
func (r *Runtime) injectedOf(id string) *injectedValues {
	r.injectedMu.Lock()
	defer r.injectedMu.Unlock()
	for _, v := range r.injected {
		if v.lease == id {
			return v
		}
	}
	return nil
}

// validCAHost says h is a host name the lease's CA can be constrained
// to: not an address.
func validCAHost(h string) bool {
	return h != "" && net.ParseIP(h) == nil && !strings.ContainsAny(h, "*/:")
}

// reportExposed tells Tripline of an exposure, through the lease's
// security reports: a dlp deny naming the rule, never a value. Latchkey
// heard of it before the lease's session ended (release.go,
// tellExposed).
func (r *Runtime) reportExposed(id string, e *credentialExposed) {
	host := e.host
	if !secreport.BareHost(host) {
		host = ""
	}
	now := time.Now().UTC()
	if rp := r.reporter(id); rp != nil {
		rp.Add(meshapi.SecurityReport{Type: meshapi.ReportDLP, Host: host, Verdict: meshapi.VerdictDeny, Rule: egress.CodeExposed, At: &now})
	}
}

// failExposed fails lease id credential_exposed (projectmesh-io/api
// #83); a Mesh that refuses the cause 422, older than #83, has the lease
// failed stopped_by_rule with the same reason instead, so the fail is
// never lost. The cause it was failed with is answered.
func (r *Runtime) failExposed(id, reason string) string {
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	cause := meshapi.CauseCredentialExposed
	st, err := r.API.Fail(ctx, id, meshapi.FailRequest{Cause: cause, Reason: clip(r.redact(reason), maxReason)})
	var me *meshapi.Error
	if err != nil && errors.As(err, &me) && me.Status == http.StatusUnprocessableEntity {
		r.logf("nodev1: lease %s: Mesh refused cause %s (%v); failing it %s", id, cause, err, meshapi.CauseStoppedByRule)
		cause = meshapi.CauseStoppedByRule
		st, err = r.API.Fail(ctx, id, meshapi.FailRequest{Cause: cause, Reason: clip(r.redact(reason), maxReason)})
	}
	switch {
	case err != nil && meshapi.LeaseGone(err):
		r.logf("nodev1: lease %s was no longer the node's to fail (%s)", id, cause)
	case err != nil:
		r.logf("nodev1: lease %s: fail (%s): %v", id, cause, err)
	default:
		r.logf("nodev1: lease %s failed (%s); job %s is %s", id, cause, st.JobID, st.Status)
	}
	return cause
}
