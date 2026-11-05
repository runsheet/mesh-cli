// Command flushreading is the reading scripts/transcript-flush-reading.sh
// takes (ADR-020's `Measure:` item, this goal's item 3, "A lease's
// transcript flush interval is set from the brief's flush_s"): for a
// given flush_s it starts a stand-in for the product's upload_url,
// starts an internal/transcript.Uploader against it exactly as a lease
// would, and times the gap between two consecutive chunks landing —
// the uploader's actual flush period, clamped by flushEvery to
// [2s, 15s] — against the flush_s asked for.
//
// It prints one line, `flush_s=<n>s measured=<n>ms`, and exits 0: a
// reading a human or the evaluator reads against the clamp, never a
// verdict a build is judged on (AGENTS.md's host-reading rule).
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/transcript"
)

func main() {
	flushS := flag.Int("flush-s", 0, "the lease's flush_s to measure (clamped to [2, 15] by the uploader)")
	timeout := flag.Duration("timeout", 30*time.Second, "how long to wait for the two chunks this reading times")
	flag.Parse()
	if *flushS <= 0 {
		fmt.Fprintln(os.Stderr, "flushreading: --flush-s must be a positive whole number of seconds")
		os.Exit(2)
	}

	arrived := make(chan time.Time, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		arrived <- time.Now()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	u := transcript.New(meshapi.Transcript{UploadURL: srv.URL, FlushS: *flushS}, "flushreading", nil, transcript.Options{})
	defer u.Close()

	deadline := time.After(*timeout)
	wait := func(what string) time.Time {
		select {
		case t := <-arrived:
			return t
		case <-deadline:
			fmt.Fprintf(os.Stderr, "flushreading: timed out waiting for %s\n", what)
			os.Exit(1)
			return time.Time{}
		}
	}

	// the first chunk goes at the first flush after this line is held;
	// the second line, held for the next flush, times the uploader's
	// actual period rather than this process's own start-up cost.
	u.Line([]byte(`{"type":"result"}`))
	first := wait("the first chunk")
	u.Line([]byte(`{"type":"result"}`))
	second := wait("the second chunk")

	measured := second.Sub(first)
	fmt.Printf("flush_s=%ds measured=%dms\n", *flushS, measured.Milliseconds())
}
