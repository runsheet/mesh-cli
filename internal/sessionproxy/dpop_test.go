package sessionproxy

import (
	"net/http"
	"testing"
)

// dpopRefusal takes only a DPoP challenge's invalid_dpop_proof as a
// refusal of the proof; invalid_token, a Bearer challenge and no header
// are not.
func TestDPoPRefusal(t *testing.T) {
	for _, tc := range []struct {
		hdr    []string
		reason string
		ok     bool
	}{
		{[]string{`DPoP error="invalid_dpop_proof", error_description="bad \"htu\""`}, `invalid_dpop_proof: bad "htu"`, true},
		{[]string{`Bearer realm="x"`, `dpop algs="ES256", error=invalid_dpop_proof`}, "invalid_dpop_proof", true},
		{[]string{`DPoP error="invalid_token"`}, "", false},
		{[]string{`Bearer error="invalid_dpop_proof"`}, "", false},
		{nil, "", false},
	} {
		h := http.Header{}
		for _, v := range tc.hdr {
			h.Add("WWW-Authenticate", v)
		}
		reason, ok := dpopRefusal(h)
		if ok != tc.ok || reason != tc.reason {
			t.Errorf("%q: got %q, %v; want %q, %v", tc.hdr, reason, ok, tc.reason, tc.ok)
		}
	}
}
