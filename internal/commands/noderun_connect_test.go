package commands

import "testing"

// TestConnectionsOf: --connect is tenant/product, each half present.
func TestConnectionsOf(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want int
		bad  bool
	}{
		{nil, 0, false},
		{[]string{"acme/conform", " globex/runsheet "}, 2, false},
		{[]string{"acme"}, 0, true},
		{[]string{"/conform"}, 0, true},
		{[]string{"acme/"}, 0, true},
		{[]string{"acme/conform/x"}, 0, true},
	} {
		got, err := connectionsOf(tc.in)
		if (err != nil) != tc.bad || len(got) != tc.want {
			t.Errorf("%v: %v %v", tc.in, got, err)
		}
	}
}
