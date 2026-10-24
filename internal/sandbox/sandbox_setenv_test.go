package sandbox

import (
	"slices"
	"testing"
)

// SetEnv replaces a variable env has where it stands and appends the
// rest, each once, the last value of a name given twice winning.
func TestSetEnvReplacesInPlaceAndAppends(t *testing.T) {
	got := SetEnv([]string{"A=1", "HEAD_SHA=old", "B=2"}, []string{"HEAD_SHA=new", "BASE_SHA=p", "BASE_SHA=q"})
	want := []string{"A=1", "HEAD_SHA=new", "B=2", "BASE_SHA=q"}
	if !slices.Equal(got, want) {
		t.Fatalf("SetEnv = %v, want %v", got, want)
	}
	if got := SetEnv([]string{"A=1"}, nil); !slices.Equal(got, []string{"A=1"}) {
		t.Fatalf("SetEnv with no vars = %v", got)
	}
}
