package meshapi

import "testing"

func TestGitPinIsTheOneExactCreateOnlyRule(t *testing.T) {
	for _, c := range []struct {
		name string
		git  *Git
		want string
	}{
		{"nil", nil, ""},
		{"no rules", &Git{}, ""},
		{"branch only", &Git{Push: []PushRule{{Ref: "refs/heads/mesh/*"}}}, ""},
		{"one exact", &Git{Push: []PushRule{{Ref: "refs/heads/mesh/*"}, {Ref: "refs/pins/final", CreateOnly: true}}}, "refs/pins/final"},
		{"prefix only", &Git{Push: []PushRule{{Ref: "refs/pins/*", CreateOnly: true}}}, ""},
		{"two exact", &Git{Push: []PushRule{{Ref: "refs/pins/a", CreateOnly: true}, {Ref: "refs/pins/b", CreateOnly: true}}}, ""},
		{"exact beside prefix", &Git{Push: []PushRule{{Ref: "refs/pins/*", CreateOnly: true}, {Ref: "refs/pins/a", CreateOnly: true}}}, "refs/pins/a"},
	} {
		if got := c.git.Pin(); got != c.want {
			t.Errorf("%s: Pin() = %q, want %q", c.name, got, c.want)
		}
	}
}
