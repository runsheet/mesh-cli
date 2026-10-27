package node

import (
	"encoding/json"
	"testing"
)

// TestPushStatementNamesOnlyGitAndTheJob: a push statement is an in-toto
// v1 statement whose subject is the commit and whose predicate names the
// repository, ref, commit, tree, job and session — nothing of a product.
func TestPushStatementNamesOnlyGitAndTheJob(t *testing.T) {
	raw, err := PushStatement(PushPredicate{Repo: "acme/widgets", Ref: "refs/x/1", Commit: "abc", Tree: "def", JobID: "j1", SessionID: "j1"})
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Type          string `json:"_type"`
		PredicateType string `json:"predicateType"`
		Subject       []struct {
			Name   string
			Digest map[string]string
		}
		Predicate map[string]string
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Type != "https://in-toto.io/Statement/v1" || st.PredicateType != PushPredicateType {
		t.Fatalf("statement type: %+v", st)
	}
	if len(st.Subject) != 1 || st.Subject[0].Name != "acme/widgets" || st.Subject[0].Digest["gitCommit"] != "abc" {
		t.Fatalf("subject: %+v", st.Subject)
	}
	want := map[string]string{"repo": "acme/widgets", "ref": "refs/x/1", "commit": "abc", "tree": "def", "job_id": "j1", "session_id": "j1"}
	if len(st.Predicate) != len(want) {
		t.Fatalf("predicate: %+v", st.Predicate)
	}
	for k, v := range want {
		if st.Predicate[k] != v {
			t.Fatalf("predicate %s = %q, want %q", k, st.Predicate[k], v)
		}
	}
}
