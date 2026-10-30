package gitapply

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/githarden/githardentest"
)

// Apply with no Command runs hardened git (mesh-specs gap 7): a hook,
// fsmonitor, hooks path, credential helper or ssh command a session
// planted in the repository runs nothing on the host, clean or
// conflicted, in either mode.
func TestApplyRunsNothingTheSessionPlanted(t *testing.T) {
	r := newRepo(t)
	r.run("checkout", "-q", "-b", "item")
	r.write("f.txt", "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\n")
	r.write("new.txt", "new\n")
	head := r.commit("the item's change")
	r.run("checkout", "-q", "main")
	r.write("g.txt", "g moved\n")
	r.commit("main moves")
	r.run("checkout", "-q", "-b", "clash")
	r.write("f.txt", "uno\ntwo\nthree\nfour\nfive\nsix\nseven\n")
	clash := r.commit("clashes with the item")
	r.run("checkout", "-q", "main")
	p := githardentest.New(t, r.dir)
	ctx := context.Background()
	for _, mode := range []Mode{Linear, Merge} {
		if _, conflict, err := (Git{}).Apply(ctx, r.dir, "main", []Change{{Head: head}}, mode, ident); err != nil || conflict != nil {
			t.Fatalf("%s: %v %v", mode, err, conflict)
		}
		if _, conflict, err := (Git{}).Apply(ctx, r.dir, clash, []Change{{Head: head}}, mode, ident); err != nil || conflict == nil {
			t.Fatalf("%s: the clash applied: %v", mode, err)
		}
	}
	p.NoneRan(t, "gitapply's default git")
}
