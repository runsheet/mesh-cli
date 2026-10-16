package gitdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// compareFixtureServer answers octo/widgets' main...feature compare from
// compare_page1.json (and, when withPage2, a second page from
// compare_page2.json that carries no files of its own, matching
// GitHub's own behaviour: a paginated compare's files list lives on
// its first page alone, the pagination being over its commits list),
// plus the head, base-tip and merge-base commits and trees. The branches
// diverged: base's tip has since made file3.go executable (100755) where
// the merge base, and so the comparison, has it a symlink (120000) —
// head removed it.
func compareFixtureServer(t *testing.T, withPage2 bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/widgets/compare/main...feature", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "page=2" {
			serveFixture(t, "compare_page2.json")(w, r)
			return
		}
		if withPage2 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/octo/widgets/compare/main...feature?page=2>; rel="next"`, "http://"+r.Host))
		}
		serveFixture(t, "compare_page1.json")(w, r)
	})
	mux.HandleFunc("/repos/octo/widgets/commits/feature", serveFixture(t, "commit_head.json"))
	mux.HandleFunc("/repos/octo/widgets/commits/main", serveFixture(t, "commit_base.json"))
	mux.HandleFunc("/repos/octo/widgets/git/trees/headtree111111111111111111111111111111", serveFixture(t, "tree_head.json"))
	mux.HandleFunc("/repos/octo/widgets/git/trees/basetree222222222222222222222222222222", serveFixture(t, "tree_base.json"))
	mux.HandleFunc("/repos/octo/widgets/commits/ccccccccccccccccccccccccccccccccccccccc1", serveFixture(t, "commit_mergebase.json"))
	mux.HandleFunc("/repos/octo/widgets/git/trees/mergetree33333333333333333333333333333", serveFixture(t, "tree_mergebase.json"))
	return httptest.NewServer(mux)
}

func TestComparePaginatesAndMergesFiles(t *testing.T) {
	srv := compareFixtureServer(t, true)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	got, err := c.Compare(context.Background(), "octo/widgets", "main", "feature")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if got.MergeBaseSHA != "ccccccccccccccccccccccccccccccccccccccc1" {
		t.Fatalf("MergeBaseSHA = %q", got.MergeBaseSHA)
	}
	if len(got.Files) != 4 {
		t.Fatalf("Files = %d, want 4: %+v", len(got.Files), got.Files)
	}

	want := []ChangedFile{
		{Filename: "file1.go", Status: StatusAdded, SHA: "1111111111111111111111111111111111111a", Mode: "100644"},
		{Filename: "file2.go", Status: StatusRenamed, SHA: "2222222222222222222222222222222222222b", PreviousFilename: "old2.go", Mode: "100755"},
		{Filename: "file3.go", Status: StatusRemoved, Mode: "120000"},
		{Filename: "file4.go", Status: StatusModified, SHA: "4444444444444444444444444444444444444d", Mode: "100644"},
	}
	for i, w := range want {
		if got.Files[i] != w {
			t.Fatalf("Files[%d] = %+v, want %+v", i, got.Files[i], w)
		}
	}
}

func TestCompareARenamedFileCarriesItsPreviousName(t *testing.T) {
	srv := compareFixtureServer(t, false)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	got, err := c.Compare(context.Background(), "octo/widgets", "main", "feature")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var renamed *ChangedFile
	for i := range got.Files {
		if got.Files[i].Status == StatusRenamed {
			renamed = &got.Files[i]
		}
	}
	if renamed == nil {
		t.Fatalf("no renamed file in %+v", got.Files)
	}
	if renamed.Filename != "file2.go" || renamed.PreviousFilename != "old2.go" {
		t.Fatalf("renamed = %+v", renamed)
	}
	if renamed.Mode != "100755" {
		t.Fatalf("renamed.Mode = %q, want executable 100755", renamed.Mode)
	}
}

func TestCompareARemovedFile(t *testing.T) {
	srv := compareFixtureServer(t, false)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	got, err := c.Compare(context.Background(), "octo/widgets", "main", "feature")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var removed *ChangedFile
	for i := range got.Files {
		if got.Files[i].Status == StatusRemoved {
			removed = &got.Files[i]
		}
	}
	if removed == nil {
		t.Fatalf("no removed file in %+v", got.Files)
	}
	if removed.Filename != "file3.go" || removed.SHA != "" {
		t.Fatalf("removed = %+v", removed)
	}
	if removed.Mode != "120000" {
		t.Fatalf("removed.Mode = %q, want symlink 120000 from the merge-base tree", removed.Mode)
	}
}

func TestCompareAddedFileCarriesItsRegularMode(t *testing.T) {
	srv := compareFixtureServer(t, false)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	got, err := c.Compare(context.Background(), "octo/widgets", "main", "feature")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var added *ChangedFile
	for i := range got.Files {
		if got.Files[i].Status == StatusAdded {
			added = &got.Files[i]
		}
	}
	if added == nil {
		t.Fatalf("no added file in %+v", got.Files)
	}
	if added.Mode != "100644" {
		t.Fatalf("added.Mode = %q, want regular 100644", added.Mode)
	}
}

func TestCompareDiffTooLargePastGitHubsCap(t *testing.T) {
	files := make([]map[string]any, 0, DiffCapFiles)
	for i := 0; i < DiffCapFiles; i++ {
		files = append(files, map[string]any{
			"filename": fmt.Sprintf("file%d.go", i),
			"status":   "modified",
			"sha":      fmt.Sprintf("%040d", i),
		})
	}
	body, err := json.Marshal(map[string]any{
		"merge_base_commit": map[string]any{"sha": "ccccccccccccccccccccccccccccccccccccccc1"},
		"files":             files,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	_, err = c.Compare(context.Background(), "octo/widgets", "main", "feature")
	if !IsDiffTooLarge(err) {
		t.Fatalf("err = %v, IsDiffTooLarge = false", err)
	}
}

// TestCompareARemovedFilesModeIsTheMergeBases is the divergent case:
// base's tip changed file3.go's mode (120000 → 100755) after head
// branched and deleted it, so base...head removes the merge base's
// symlink, and Compare must say 120000, never base's 100755. The second
// half drops the compare's inline merge-base tree sha and expects the
// same answer by way of the merge-base commit.
func TestCompareARemovedFilesModeIsTheMergeBases(t *testing.T) {
	for _, inlineTree := range []bool{true, false} {
		t.Run(fmt.Sprintf("inline tree %v", inlineTree), func(t *testing.T) {
			page1, err := os.ReadFile("testdata/compare_page1.json")
			if err != nil {
				t.Fatal(err)
			}
			if !inlineTree {
				var resp map[string]any
				if err := json.Unmarshal(page1, &resp); err != nil {
					t.Fatal(err)
				}
				delete(resp["merge_base_commit"].(map[string]any), "commit")
				if page1, err = json.Marshal(resp); err != nil {
					t.Fatal(err)
				}
			}
			var baseTipRead bool
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/octo/widgets/compare/main...feature", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(page1)
			})
			mux.HandleFunc("/repos/octo/widgets/commits/feature", serveFixture(t, "commit_head.json"))
			mux.HandleFunc("/repos/octo/widgets/git/trees/headtree111111111111111111111111111111", serveFixture(t, "tree_head.json"))
			mux.HandleFunc("/repos/octo/widgets/commits/ccccccccccccccccccccccccccccccccccccccc1", serveFixture(t, "commit_mergebase.json"))
			mux.HandleFunc("/repos/octo/widgets/git/trees/mergetree33333333333333333333333333333", serveFixture(t, "tree_mergebase.json"))
			mux.HandleFunc("/repos/octo/widgets/commits/main", func(w http.ResponseWriter, r *http.Request) {
				baseTipRead = true
				serveFixture(t, "commit_base.json")(w, r)
			})
			mux.HandleFunc("/repos/octo/widgets/git/trees/basetree222222222222222222222222222222", serveFixture(t, "tree_base.json"))
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := &Client{BaseURL: srv.URL}
			got, err := c.Compare(context.Background(), "octo/widgets", "main", "feature")
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			for _, f := range got.Files {
				if f.Filename == "file3.go" {
					if f.Mode != "120000" {
						t.Fatalf("file3.go Mode = %q, want the merge base's 120000 (base's tip has 100755)", f.Mode)
					}
				}
			}
			if baseTipRead {
				t.Fatalf("Compare read base's tip; a removed file's mode is the merge base's")
			}
		})
	}
}
