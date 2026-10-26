package sessionproxy

import (
	"net/http"
	"strings"
	"testing"
)

// A 401 whose WWW-Authenticate is a DPoP challenge with error
// invalid_dpop_proof is runsheet refusing the proof, and the reason
// names it (runsheet ADR 064 §2).
func TestDPoPRefusal(t *testing.T) {
	answer := func(status int, challenges ...string) *http.Response {
		r := &http.Response{StatusCode: status, Header: http.Header{}}
		for _, c := range challenges {
			r.Header.Add("WWW-Authenticate", c)
		}
		return r
	}
	t.Run("invalid_dpop_proof", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			resp   *http.Response
			reason string
		}{
			{"bare", answer(http.StatusUnauthorized, `DPoP error="invalid_dpop_proof"`), "invalid_dpop_proof"},
			{"described", answer(http.StatusUnauthorized, `DPoP algs="ES256", error="invalid_dpop_proof", error_description="proof iat too old"`), "invalid_dpop_proof: proof iat too old"},
			{"scheme case", answer(http.StatusUnauthorized, `dpop error=invalid_dpop_proof`), "invalid_dpop_proof"},
			{"beside bearer", answer(http.StatusUnauthorized, `Bearer realm="mcp", DPoP error="invalid_dpop_proof"`), "invalid_dpop_proof"},
			{"second header", answer(http.StatusUnauthorized, `Bearer realm="mcp"`, `DPoP error="invalid_dpop_proof"`), "invalid_dpop_proof"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				reason, refused := dpopRefusal(tc.resp)
				if !refused || reason != tc.reason || !strings.Contains(reason, "invalid_dpop_proof") {
					t.Fatalf("dpopRefusal = %q, %v; want %q, true", reason, refused, tc.reason)
				}
			})
		}
		for _, tc := range []struct {
			name string
			resp *http.Response
		}{
			{"invalid_token", answer(http.StatusUnauthorized, `DPoP error="invalid_token"`)},
			{"bearer scheme", answer(http.StatusUnauthorized, `Bearer error="invalid_dpop_proof"`)},
			{"no header", answer(http.StatusUnauthorized)},
			{"not a 401", answer(http.StatusForbidden, `DPoP error="invalid_dpop_proof"`)},
			{"nil", nil},
		} {
			t.Run("not "+tc.name, func(t *testing.T) {
				if reason, refused := dpopRefusal(tc.resp); refused {
					t.Fatalf("dpopRefusal = %q, true; want no refusal", reason)
				}
			})
		}
	})
}
