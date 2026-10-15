package harness

import (
	"bytes"
	"io"
	"sync"
)

// streamRedactor passes a stream on with the secrets redacted, a secret
// split across writes included: it holds back the longest tail that
// could begin a secret until the next write completes or rules it out,
// and Flush writes what is held once the stream ends. A pod's stderr
// arrives in whatever chunks podman's pipe gives, not a line at a time.
type streamRedactor struct {
	mu      sync.Mutex
	w       io.Writer
	secrets [][]byte
	held    []byte
}

func newStreamRedactor(w io.Writer, secrets []string) *streamRedactor {
	r := &streamRedactor{w: w}
	for _, s := range secrets {
		if s != "" {
			r.secrets = append(r.secrets, []byte(s))
		}
	}
	return r
}

func (r *streamRedactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.secrets) == 0 {
		return r.w.Write(p)
	}
	buf := append(r.held, p...)
	for _, s := range r.secrets {
		buf = bytes.ReplaceAll(buf, s, []byte(Redacted))
	}
	keep := r.partialTail(buf)
	out := buf[:len(buf)-keep]
	r.held = append([]byte(nil), buf[len(buf)-keep:]...)
	if len(out) > 0 {
		if _, err := r.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// partialTail is the length of the longest suffix of buf that is a
// proper prefix of a secret.
func (r *streamRedactor) partialTail(buf []byte) int {
	longest := 0
	for _, s := range r.secrets {
		n := min(len(s)-1, len(buf))
		for k := n; k > longest; k-- {
			if bytes.HasSuffix(buf, s[:k]) {
				longest = k
				break
			}
		}
	}
	return longest
}

// Flush writes what is held back: the stream ended, so it is no secret.
func (r *streamRedactor) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.held) == 0 {
		return nil
	}
	_, err := r.w.Write(r.held)
	r.held = nil
	return err
}
